/*
Copyright the Velero contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/util/csi"
	retainedsnapshot "github.com/velero-io/velero/pkg/util/retainedsnapshot"
)

// retainedSnapshotReleaseControllerName names the rolling-base release worker
// of the retained-snapshot controller.
const retainedSnapshotReleaseControllerName = "retained-snapshot-release"

// retainedSnapshotInFlightRequeue spaces the re-checks of a release that waits
// for a data movement of the same volume to finish.
var retainedSnapshotInFlightRequeue = 30 * time.Second

// retainedSnapshotReleaseAttemptsAnnotation persists, on the DataUpload whose
// snapshot is being released, the number of counted DeleteSnapshot failures
// of the release, so the escalation bound holds across restarts.
const retainedSnapshotReleaseAttemptsAnnotation = "velero.io/retained-snapshot-release-attempts"

// retainedSnapshotReleaser is the rolling-base half of the retained-snapshot
// controller: one retained snapshot per volume chain -- driver plus CSI volume
// handle -- is kept as the base of the next incremental backup. When a
// DataUpload of the chain completes with a record, every other unreleased
// retained snapshot of the chain that completed before it is released: deleted
// from the storage through the temp-content deletion path, oldest first, and
// its owning DataUpload's record marked released by the new backup, with
// RetainedSnapshotReleased emitted on the releasing DataUpload. A released
// record no longer owns a snapshot, so backup deletion skips it. A release that
// keeps failing escalates to the orphan record at the same bound as a deletion.
//
// The release runs only after a data movement completed, never during one: a
// data movement of the same PVC that is still running may be computing its
// delta against one of the snapshots, and a failed movement's retry must still
// find its base. The release waits until no DataUpload of the PVC is in flight.
type retainedSnapshotReleaser struct {
	*retainedSnapshotReconciler
}

// +kubebuilder:rbac:groups=velero.io,resources=datauploads,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=velero.io,resources=datauploads/status,verbs=get;update;patch

func (r *retainedSnapshotReleaser) setupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&velerov2alpha1api.DataUpload{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			du, ok := object.(*velerov2alpha1api.DataUpload)
			return ok && isRollingBaseHolder(du)
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named(retainedSnapshotReleaseControllerName).
		Complete(r)
}

// isRollingBaseHolder reports whether the DataUpload completed with an
// unreleased, chained record: the trigger of a release.
func isRollingBaseHolder(du *velerov2alpha1api.DataUpload) bool {
	record := du.Status.RetainedCSIStorageSnapshot
	return du.Status.Phase == velerov2alpha1api.DataUploadPhaseCompleted && du.DeletionTimestamp.IsZero() &&
		record != nil && !record.Released && record.CSIVolumeHandle != "" && record.Driver != ""
}

func (r *retainedSnapshotReleaser) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.logger.WithFields(logrus.Fields{
		"controller": retainedSnapshotReleaseControllerName,
		"dataupload": req.String(),
	})

	holder := &velerov2alpha1api.DataUpload{}
	if err := r.Get(ctx, req.NamespacedName, holder); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.Wrap(err, "error getting the DataUpload")
	}
	if !isRollingBaseHolder(holder) {
		return ctrl.Result{}, nil
	}

	all := &velerov2alpha1api.DataUploadList{}
	if err := r.List(ctx, all, client.InNamespace(holder.Namespace)); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "error listing the DataUploads")
	}

	// Never release during a data movement of the same volume.
	if pvcUID := holder.Labels[velerov1api.PVCUIDLabel]; pvcUID != "" {
		for i := range all.Items {
			du := &all.Items[i]
			if du.Name != holder.Name && du.Labels[velerov1api.PVCUIDLabel] == pvcUID && !isDataUploadInFinalState(du) {
				log.Infof("DataUpload %s of the same volume is in phase %q, releasing the older retained snapshots after it finishes", du.Name, du.Status.Phase)
				return ctrl.Result{RequeueAfter: retainedSnapshotInFlightRequeue}, nil
			}
		}
	}

	candidates := releaseCandidates(holder, all.Items)
	if len(candidates) == 0 {
		return ctrl.Result{}, nil
	}
	releasedBy := holder.Labels[velerov1api.BackupUIDLabel]
	log = log.WithField("releasedBy", releasedBy)

	for _, candidate := range candidates {
		record := candidate.Status.RetainedCSIStorageSnapshot
		if err := csi.DeleteRetainedSnapshot(ctx, r.Client, r.apiReader, record, types.UID(candidate.Labels[velerov1api.BackupUIDLabel]), log); err != nil {
			escalated, countErr := r.countReleaseFailure(ctx, holder, candidate, err, log)
			if countErr != nil {
				return ctrl.Result{}, countErr
			}
			if escalated {
				continue
			}
			// The record stays unreleased and its backup deletion still owns
			// the snapshot; the release is retried on this controller's own
			// schedule.
			r.dataUploadEventf(holder, true, "RetainedSnapshotReleaseFailed",
				"the retained snapshot %s of DataUpload %s was not released for the newer base of DataUpload %s: %v; retrying", record.CSISnapshotHandle, candidate.Name, holder.Name, err)
			log.WithError(err).Warnf("Failed to release the retained snapshot %s of DataUpload %s, retrying", record.CSISnapshotHandle, candidate.Name)
			return ctrl.Result{RequeueAfter: retainedSnapshotRetryDelay}, nil
		}
		if err := r.markReleased(ctx, candidate, releasedBy, log); err != nil {
			return ctrl.Result{}, err
		}
		r.dataUploadEventf(holder, false, "RetainedSnapshotReleased",
			"the retained snapshot %s of DataUpload %s was released: DataUpload %s of backup %s completed and holds the newer base of the volume", record.CSISnapshotHandle, candidate.Name, holder.Name, releasedBy)
		log.Infof("Released the retained snapshot %s of DataUpload %s, DataUpload %s holds the newer base", record.CSISnapshotHandle, candidate.Name, holder.Name)
	}
	return ctrl.Result{}, nil
}

// markReleased marks the candidate's record released by the holder's backup and
// drops its release attempt count. A DataUpload deleted meanwhile had its
// record delivered to its backup deletion, which finds the snapshot already
// gone, a success.
func (r *retainedSnapshotReleaser) markReleased(ctx context.Context, candidate *velerov2alpha1api.DataUpload, releasedBy string, log logrus.FieldLogger) error {
	err := UpdateDataUploadWithRetry(ctx, r.Client, types.NamespacedName{Namespace: candidate.Namespace, Name: candidate.Name}, log, func(current *velerov2alpha1api.DataUpload) bool {
		if current.Status.RetainedCSIStorageSnapshot == nil || current.Status.RetainedCSIStorageSnapshot.Released {
			return false
		}
		current.Status.RetainedCSIStorageSnapshot.Released = true
		current.Status.RetainedCSIStorageSnapshot.ReleasedBy = releasedBy
		if current.Annotations != nil {
			annotations := map[string]string{}
			for key, value := range current.Annotations {
				if key != retainedSnapshotReleaseAttemptsAnnotation {
					annotations[key] = value
				}
			}
			current.Annotations = annotations
		}
		return true
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "error marking the record of DataUpload %s released", candidate.Name)
	}
	return nil
}

// countReleaseFailure counts a failed release toward the escalation bound, the
// same bound and the same counted errors as a deletion: a DeleteSnapshot call
// failure, or a timeout whose deletion secret exists. Other errors are not
// counted and are retried. At the bound the record moves to the orphan-snapshot
// ConfigMap with an event, the temp content is released, and the record is
// marked released so the backup deletion skips it; the orphan record is the
// home for manual cleanup. It reports whether the record escalated.
func (r *retainedSnapshotReleaser) countReleaseFailure(ctx context.Context, holder, candidate *velerov2alpha1api.DataUpload, releaseErr error, log logrus.FieldLogger) (bool, error) {
	var callErr *csi.SnapshotDeleteCallError
	if !errors.As(releaseErr, &callErr) {
		return false, nil
	}
	attempts := 0
	if err := UpdateDataUploadWithRetry(ctx, r.Client, types.NamespacedName{Namespace: candidate.Namespace, Name: candidate.Name}, log, func(current *velerov2alpha1api.DataUpload) bool {
		attempts, _ = strconv.Atoi(current.Annotations[retainedSnapshotReleaseAttemptsAnnotation])
		attempts++
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[retainedSnapshotReleaseAttemptsAnnotation] = strconv.Itoa(attempts)
		return true
	}); err != nil {
		return false, errors.Wrapf(err, "error persisting the release attempt count of DataUpload %s", candidate.Name)
	}
	if attempts < retainedsnapshot.MaxAttempts {
		return false, nil
	}

	record := candidate.Status.RetainedCSIStorageSnapshot
	owner := &velerov1api.Backup{ObjectMeta: metav1.ObjectMeta{
		Namespace: candidate.Namespace,
		Name:      candidate.Labels[velerov1api.BackupNameLabel],
		UID:       types.UID(candidate.Labels[velerov1api.BackupUIDLabel]),
	}}
	entry := &retainedsnapshot.Entry{
		CSISnapshotHandle:   record.CSISnapshotHandle,
		Driver:              record.Driver,
		DeletionSecretRef:   record.DeletionSecretRef,
		ManagedByLabelValue: record.ManagedByLabelValue,
		CSIVolumeHandle:     record.CSIVolumeHandle,
		Attempts:            attempts,
		Trusted:             true,
	}
	if err := retainedsnapshot.WriteOrphanEntry(ctx, r.Client, owner, candidate.Name, entry); err != nil {
		return false, errors.Wrapf(err, "error moving the record of DataUpload %s to the orphan-snapshot ConfigMap", candidate.Name)
	}
	if err := csi.ReleaseTempVSC(ctx, r.Client, r.apiReader, record, log); err != nil {
		log.WithError(err).Warnf("Error releasing the temp VolumeSnapshotContent of the escalated snapshot of DataUpload %s; remove its bound-protection finalizer by hand", candidate.Name)
	}
	if err := r.markReleased(ctx, candidate, holder.Labels[velerov1api.BackupUIDLabel], log); err != nil {
		return false, err
	}
	r.dataUploadEventf(holder, true, "RetainedSnapshotReleaseEscalated",
		"the CSI driver failed to delete the retained snapshot %s of DataUpload %s %d times (%v); it was moved to the orphan-snapshot ConfigMap for manual cleanup", record.CSISnapshotHandle, candidate.Name, attempts, callErr)
	log.Warnf("Escalated the release of the retained snapshot %s of DataUpload %s to the orphan-snapshot ConfigMap after %d attempts", record.CSISnapshotHandle, candidate.Name, attempts)
	return true, nil
}

// dataUploadEventf records an event on the releasing DataUpload.
func (r *retainedSnapshotReleaser) dataUploadEventf(du *velerov2alpha1api.DataUpload, warning bool, reason, message string, args ...any) {
	if r.recorder == nil {
		r.logger.WithField("dataupload", du.Name).Infof(reason+": "+message, args...)
		return
	}
	r.recorder.Event(du, warning, reason, fmt.Sprintf(message, args...))
}

// releaseCandidates returns the DataUploads whose unreleased retained snapshot
// belongs to the holder's chain and completed before the holder, oldest first.
// A newer DataUpload of the chain is never released by an older one: the newer
// one's own completion releases the older one instead. A DataUpload that is
// being deleted is left to its backup deletion.
func releaseCandidates(holder *velerov2alpha1api.DataUpload, all []velerov2alpha1api.DataUpload) []*velerov2alpha1api.DataUpload {
	chain := holder.Status.RetainedCSIStorageSnapshot
	holderDone := completionOf(holder)
	var out []*velerov2alpha1api.DataUpload
	for i := range all {
		du := &all[i]
		record := du.Status.RetainedCSIStorageSnapshot
		if du.Name == holder.Name || record == nil || record.Released || !du.DeletionTimestamp.IsZero() ||
			record.Driver != chain.Driver || record.CSIVolumeHandle != chain.CSIVolumeHandle ||
			record.CSISnapshotHandle == "" || record.CSISnapshotHandle == chain.CSISnapshotHandle {
			continue
		}
		if !completionOf(du).Before(holderDone) {
			continue
		}
		out = append(out, du)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return completionOf(out[i]).Before(completionOf(out[j]))
	})
	return out
}

// completionOf returns when the DataUpload completed, falling back to its
// creation for an object without a completion timestamp.
func completionOf(du *velerov2alpha1api.DataUpload) time.Time {
	if du.Status.CompletionTimestamp != nil {
		return du.Status.CompletionTimestamp.Time
	}
	return du.CreationTimestamp.Time
}
