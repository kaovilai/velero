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
	"time"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	storagev1api "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	"github.com/velero-io/velero/pkg/constant"
	"github.com/velero-io/velero/pkg/util/csi"
	retainedsnapshot "github.com/velero-io/velero/pkg/util/retainedsnapshot"
)

// retainedSnapshotReconciler deletes the retained snapshots of a backup whose
// deletion is in progress. It is keyed by the record ConfigMap family, runs
// its own workers and requeues on its own objects, so a slow snapshot delays
// only its own entry; the DeleteBackupRequest itself never waits.
type retainedSnapshotReconciler struct {
	client.Client
	// apiReader reads from the API server instead of the cache: the
	// verification reads of the retained snapshot deletion must see what the
	// API server holds, not an informer-lagged copy, or a just-written status
	// reads back missing and fails the deletion spuriously.
	apiReader client.Reader
	logger    logrus.FieldLogger
	clock     clock.Clock
	recorder  kubeEventRecorder
}

// kubeEventRecorder is the subset of the event recorders used to report
// skipped and escalated records; it is satisfied by kube.EventRecorder.
type kubeEventRecorder interface {
	Event(object runtime.Object, warning bool, reason string, message string, a ...any)
}

// NewRetainedSnapshotReconciler creates a new retained-snapshot reconciler.
// The apiReader reads uncached; a nil apiReader falls back to the cached
// client.
func NewRetainedSnapshotReconciler(logger logrus.FieldLogger, client client.Client, apiReader client.Reader, clock clock.Clock) *retainedSnapshotReconciler {
	return &retainedSnapshotReconciler{
		Client:    client,
		apiReader: apiReader,
		logger:    logger,
		clock:     clock,
	}
}

// NewRetainedSnapshotReconcilerWithRecorder creates a new retained-snapshot
// reconciler with an event recorder, for reporting skipped and escalated
// records. The apiReader reads uncached; a nil apiReader falls back to the
// cached client.
func NewRetainedSnapshotReconcilerWithRecorder(logger logrus.FieldLogger, client client.Client, apiReader client.Reader, clock clock.Clock, recorder kubeEventRecorder) *retainedSnapshotReconciler {
	return &retainedSnapshotReconciler{
		Client:    client,
		apiReader: apiReader,
		logger:    logger,
		clock:     clock,
		recorder:  recorder,
	}
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;update;delete
// The events list verb backs the timeout diagnosis: the SnapshotDeleteError
// events of a cluster-scoped VolumeSnapshotContent are listed to tell a
// DeleteSnapshot call failure, which escalates after the bounded attempts,
// from a credential failure or a plain timeout, which stay retryable.
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;create;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=csidrivers,verbs=get;list;watch
// +kubebuilder:rbac:groups=velero.io,resources=deletebackuprequests,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=velero.io,resources=deletebackuprequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotcontents,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotcontents/status,verbs=get;update;patch

func (r *retainedSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1api.ConfigMap{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			// Keyed by the record ConfigMap family, not by the backup or the
			// request: shard 0, the manifest, for the deletion work, and the
			// entry shards for the sweep of an inert family whose Backup is
			// gone.
			cm, ok := object.(*corev1api.ConfigMap)
			return ok && (retainedsnapshot.IsManifest(cm) || retainedsnapshot.IsEntryShard(cm))
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Named(constant.ControllerRetainedSnapshot).
		Complete(r)
}

func (r *retainedSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.logger.WithFields(logrus.Fields{
		"controller": constant.ControllerRetainedSnapshot,
		"configmap":  req.String(),
	})

	shard := &corev1api.ConfigMap{}
	if err := r.Get(ctx, req.NamespacedName, shard); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.Wrap(err, "error getting the retained snapshot record shard")
	}
	isManifest := retainedsnapshot.IsManifest(shard)
	if !isManifest && !retainedsnapshot.IsEntryShard(shard) {
		return ctrl.Result{}, nil
	}
	backup, err := r.backupOfShard(ctx, shard)
	if err != nil {
		return ctrl.Result{}, err
	}
	if backup == nil {
		// The Backup is gone by other means. A GC that finds a family whose
		// Backup is gone, with or without a shard 0, removes it only when it
		// has no pending entries; when pending entries exist, they are moved
		// to the orphan-snapshot ConfigMap with an event, the same path as
		// escalation and skip, and only then is the family removed, so no
		// path deletes a pending record without leaving the orphan trace the
		// Security section requires. The sweep covers an inert family too: an
		// entry parked by the finalizer hold of section 3, or a crash
		// leftover, whose Backup was deleted by other means must not sit
		// unowned forever.
		return ctrl.Result{}, r.garbageCollectShard(ctx, shard, log)
	}

	// An entry shard of a live Backup is inert until the backup deletion
	// writes shard 0: the controller is keyed by shard 0 and never lists entry
	// shards on its own, so a deletion whose repository step failed gathers
	// records but never creates a temporary content. The requeue re-checks
	// whether the Backup is still there, for the sweep above.
	if !isManifest {
		log.Debug("The entry shard has no manifest, ignoring it")
		return ctrl.Result{RequeueAfter: inertShardRecheckDelay}, nil
	}

	log = log.WithField("backup", backup.Name)

	// Shard 0 then exactly the entry shards it counts.
	family, err := retainedsnapshot.LoadFamily(ctx, r.Client, backup)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "error reading the retained snapshot records")
	}

	retry := false
	for _, entryShard := range family.Shards {
		for _, name := range pendingShardKeys(entryShard) {
			outcome, err := r.deleteEntry(ctx, backup, entryShard, name, log)
			if err != nil {
				// An uncounted failure -- credentials, clear-status, an
				// unknown message, an API error -- fails the backup deletion
				// so it can be retried, and deletes shard 0 so no unowned
				// work continues; the retry is a new request that regathers
				// into the inert entry shards, which is idempotent, writes
				// shard 0 again and replays only the unfinished entries
				// (design section 4).
				if deactivateErr := retainedsnapshot.DeleteManifest(ctx, r.Client, backup); deactivateErr != nil {
					log.WithError(deactivateErr).Error("Error deleting the retained snapshot record manifest")
					return ctrl.Result{}, deactivateErr
				}
				if failErr := r.failDeleteBackupRequest(ctx, backup, err); failErr != nil {
					log.WithError(failErr).Error("Error failing the backup deletion")
					return ctrl.Result{}, failErr
				}
				log.WithError(err).Errorf("Error deleting the retained snapshot of DataUpload %s, failing the backup deletion", name)
				return ctrl.Result{}, nil
			}
			switch outcome {
			case entryOrphaned:
				// The entry was skipped or escalated, its record moved to the
				// orphan-snapshot ConfigMap; the deletion proceeds.
				log.Infof("The retained snapshot of DataUpload %s was skipped, its record is in the orphan-snapshot ConfigMap", name)
			case entryRetrying:
				// A counted DeleteSnapshot failure under the bound does not
				// fail the request: it stays InProgress with its annotation,
				// and this controller requeues on its own. Failing the request
				// would kill the only object the watches can re-enter, and
				// the backup would sit in Deleting until a manual retry
				// (design section 4).
				retry = true
			}
		}
	}
	if retry {
		return ctrl.Result{RequeueAfter: retainedSnapshotRetryDelay}, nil
	}

	// This controller never writes to the request. Re-entry is watch-driven:
	// the ConfigMap update that emptied or escalated the family fires the
	// backup-deletion controller's family watch, and the temp content's
	// deletion fires its content watch, both mapped to the request through
	// the backup UID label; the request's own 30-second RequeueAfter is the
	// backstop for an event lost between its read and its return (design
	// section 4). While pending work remains, this reconcile's writes
	// re-enqueue the shard, and the requeue below is the backstop for a
	// deletion whose wait expired without a further write.
	family, err = retainedsnapshot.LoadFamily(ctx, r.Client, backup)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "error re-reading the retained snapshot records")
	}
	if !family.HasPendingWork() {
		log.Info("The retained snapshots are deleted; the backup deletion re-enters through its watches")
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: csi.RetainedSnapshotDeletionTimeout}, nil
}

// backupOfShard resolves the Backup a record shard belongs to through its
// annotation, and returns nil when the Backup is gone. The full name is taken
// from the annotation: the backup-name label stores a sanitized value that
// hashes names longer than 63 characters, and looking a Backup up by that
// value would report a live Backup as gone.
func (r *retainedSnapshotReconciler) backupOfShard(ctx context.Context, shard *corev1api.ConfigMap) (*velerov1api.Backup, error) {
	backupName := shard.Annotations[velerov1api.RetainedSnapshotsBackupNameAnnotation]
	if backupName == "" {
		backupName = shard.Labels[velerov1api.BackupNameLabel]
	}
	if backupName == "" {
		return nil, errors.Errorf("the record shard %s carries no %s annotation", shard.Name, velerov1api.RetainedSnapshotsBackupNameAnnotation)
	}
	backup := &velerov1api.Backup{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: shard.Namespace, Name: backupName}, backup); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, errors.Wrapf(err, "error getting the backup %s of the record shard %s", backupName, shard.Name)
	}
	// The name can repeat; the UID is the identity the family was gathered for.
	if uid := shard.Labels[velerov1api.BackupUIDLabel]; uid != "" && string(backup.UID) != uid {
		return nil, nil
	}
	return backup, nil
}

// entryOutcome is the result of one entry's deletion attempt.
type entryOutcome int

const (
	// entryDeleted: the snapshot is confirmed deleted and the entry removed.
	entryDeleted entryOutcome = iota
	// entryOrphaned: the record was moved to the orphan-snapshot ConfigMap --
	// skipped for an unknown driver, undecodable, or escalated after the
	// bounded number of persisted DeleteSnapshot failures -- so the backup
	// deletion proceeds.
	entryOrphaned
	// entryRetrying: a counted DeleteSnapshot failure under the bound; the
	// entry stays pending with its attempt count persisted and the controller
	// requeues on its own, the request untouched.
	entryRetrying
)

// retainedSnapshotRetryDelay is the requeue delay after a counted
// DeleteSnapshot failure under the bound. Each attempt is itself bounded by
// the deletion timeout, so the delay only spaces the attempts.
var retainedSnapshotRetryDelay = 10 * time.Second

// inertShardRecheckDelay is the requeue delay of an entry shard whose family
// has no shard 0 while its Backup still exists: the shard is inert, and the
// requeue only re-checks whether the Backup is gone, for the sweep.
var inertShardRecheckDelay = time.Hour

// deleteEntry deletes the retained snapshot of one DataUpload; see
// entryOutcome for the results. A returned error is an uncounted failure that
// fails the backup deletion so it can be retried.
func (r *retainedSnapshotReconciler) deleteEntry(ctx context.Context, backup *velerov1api.Backup, shard *corev1api.ConfigMap, name string, log logrus.FieldLogger) (entryOutcome, error) {
	value, ok := shard.Data[name]
	if !ok {
		return entryDeleted, nil
	}
	entry, err := retainedsnapshot.UnmarshalEntry(value)
	if err != nil {
		// A record that cannot be decoded can never be deleted; it is moved to
		// the orphan ConfigMap for manual cleanup instead of failing the
		// deletion forever.
		r.eventf(shard, backup, "RetainedSnapshotRecordInvalid", "the retained snapshot record of DataUpload %s cannot be decoded and was moved to the orphan-snapshot ConfigMap: %v", name, err)
		return entryOrphaned, r.moveToOrphan(ctx, backup, name, &retainedsnapshot.Entry{Attempts: retainedsnapshot.MaxAttempts})
	}
	if entry.CSISnapshotHandle == "" {
		return entryDeleted, errors.Errorf("the retained snapshot record of DataUpload %s carries no handle", name)
	}

	// Records sourced from DataUpload.status are trusted -- they entered
	// through the API -- and are preferred over tarball- or tag-derived values
	// wherever both exist. For an untrusted record naming a driver the cluster
	// does not run, the deletion neither issues the DeleteSnapshot call nor
	// hard-fails: a secondary cluster that runs no such driver is exactly the
	// sync use case, and a hard failure would make synced backups undeletable.
	if !entry.Trusted {
		known, err := r.driverExists(ctx, entry.Driver)
		if err != nil {
			return entryDeleted, errors.Wrapf(err, "error checking whether the cluster runs driver %s", entry.Driver)
		}
		if !known {
			r.eventf(shard, backup, "RetainedSnapshotDriverUnknown", "the cluster does not run driver %s, the retained snapshot of DataUpload %s was moved to the orphan-snapshot ConfigMap for manual cleanup", entry.Driver, name)
			return entryOrphaned, r.moveToOrphan(ctx, backup, name, entry)
		}
	}

	err = csi.DeleteRetainedSnapshot(ctx, r.Client, r.apiReader, entry.Record(), backup.UID, log)
	if err == nil {
		// The entry is removed only after the snapshot is confirmed deleted.
		if err := retainedsnapshot.MarkCompleted(ctx, r.Client, backup, name); err != nil {
			return entryDeleted, errors.Wrapf(err, "error marking the retained snapshot of DataUpload %s as deleted", name)
		}
		log.Infof("Deleted the retained snapshot %s of DataUpload %s", entry.CSISnapshotHandle, name)
		r.resolveStaleOrphans(ctx, backup, shard, name, entry, log)
		return entryDeleted, nil
	}

	var callErr *csi.SnapshotDeleteCallError
	if errors.As(err, &callErr) {
		// A counted failure: the DeleteSnapshot call itself failed, or the
		// deletion timed out. The timeout is the authoritative signal and
		// counts whether or not the sidecar's event message matched; the
		// message match is best effort and only shortens the time to get
		// here (design section 4). The sidecar emits the same event reason
		// for credential failures, which are an environment problem that
		// stays retryable, and for status-clear failures, which follow a
		// successful delete; those come back as plain errors below. A driver
		// that answers an already-gone snapshot with an internal error can
		// fail every retry, so the deletion escalates after a bounded number
		// of these instead of relying on driver compliance. Escalating trades
		// a possible leak against an undeletable backup, and that trade is
		// deliberate.
		entry.Attempts++
		if entry.Attempts >= retainedsnapshot.MaxAttempts {
			r.eventf(shard, backup, "RetainedSnapshotDeleteEscalated",
				"the CSI driver failed to delete the snapshot %s of DataUpload %s %d times (%v); it was moved to the orphan-snapshot ConfigMap for manual cleanup, and the backup deletion proceeds",
				entry.CSISnapshotHandle, name, entry.Attempts, callErr)
			if err := r.moveToOrphan(ctx, backup, name, entry); err != nil {
				return entryOrphaned, err
			}
			// Escalation also releases the temp content: it was left deleting
			// while the sidecar retried, and nothing retries it once the entry
			// has moved to the orphan ConfigMap, so without this step it would
			// sit with a deletionTimestamp forever (design section 4). The
			// orphan record is already durable, so a failure here is reported
			// and does not fail the deletion.
			if err := csi.ReleaseTempVSC(ctx, r.Client, r.apiReader, entry.Record(), log); err != nil {
				log.WithError(err).Warnf("Error releasing the temp VolumeSnapshotContent of the escalated snapshot of DataUpload %s; remove its bound-protection finalizer by hand", name)
			}
			return entryOrphaned, nil
		}
		// The counter is persisted, so a retry of `velero backup delete`,
		// which is a new DeleteBackupRequest, still reaches the bound across
		// retries instead of looping forever. The request itself is not
		// failed while under the bound: it stays InProgress with its
		// annotation, this controller requeues, and the per-attempt event is
		// the operator's signal.
		if err := retainedsnapshot.UpdateEntry(ctx, r.Client, backup, name, entry); err != nil {
			return entryDeleted, errors.Wrapf(err, "error persisting the failed attempt count of DataUpload %s", name)
		}
		r.eventf(shard, backup, "RetainedSnapshotDeleteFailed",
			"the CSI driver failed to delete the snapshot %s of DataUpload %s, attempt %d of %d (%v); the deletion is retried",
			entry.CSISnapshotHandle, name, entry.Attempts, retainedsnapshot.MaxAttempts, callErr)
		log.WithError(callErr).Warnf("The CSI driver failed to delete the snapshot of DataUpload %s, attempt %d of %d; retrying", name, entry.Attempts, retainedsnapshot.MaxAttempts)
		return entryRetrying, nil
	}

	// Any other failure -- a credential failure, an API error -- fails the
	// backup deletion so it can be retried.
	return entryDeleted, err
}

// resolveStaleOrphans removes the orphan entries that name the snapshot just
// deleted. An orphan entry goes stale when its Backup object was gone but the
// tarball still existed: a BSL resync recreates the Backup under a new UID,
// and this deletion then removes the snapshot through the tarball record. The
// entry would otherwise outlive the snapshot and send the operator to clean up
// a snapshot that no longer exists (design section 4). Only the snapshot
// identity can match the entry, because its UID is the earlier Backup's. The
// snapshot is gone either way, so a failure here is reported, not fatal.
func (r *retainedSnapshotReconciler) resolveStaleOrphans(ctx context.Context, backup *velerov1api.Backup, shard *corev1api.ConfigMap, name string, entry *retainedsnapshot.Entry, log logrus.FieldLogger) {
	removed, err := retainedsnapshot.RemoveOrphanEntriesForSnapshot(ctx, r.Client, backup.Namespace, entry.Driver, entry.CSISnapshotHandle)
	if err != nil {
		log.WithError(err).Warnf("Error removing the stale orphan entries of the deleted snapshot %s", entry.CSISnapshotHandle)
	}
	for _, o := range removed {
		r.eventf(shard, backup, "RetainedSnapshotOrphanResolved",
			"the snapshot %s was deleted through the record of DataUpload %s; the stale orphan entry %s (backup UID %s) was removed from ConfigMap %s",
			entry.CSISnapshotHandle, name, o.Key, o.BackupUID, o.Shard)
		log.Infof("Removed the stale orphan entry %s of the deleted snapshot %s from ConfigMap %s", o.Key, entry.CSISnapshotHandle, o.Shard)
	}
}

// moveToOrphan moves an escalated or skipped record to the orphan-snapshot
// ConfigMap, keyed by <backup-UID>.<DataUpload-name>, which satisfies the
// ConfigMap data key charset and cannot collide across backups once the
// backup label is absent. The orphan ConfigMap is not labeled by backup and is
// therefore neither deleted by the family's last-entry rule nor
// garbage-collected when the Backup is gone; it is the durable home for manual
// cleanup, and removing an entry after its snapshot is cleaned up by hand is
// the operator's action.
func (r *retainedSnapshotReconciler) moveToOrphan(ctx context.Context, backup *velerov1api.Backup, name string, entry *retainedsnapshot.Entry) error {
	if err := retainedsnapshot.WriteOrphanEntry(ctx, r.Client, backup, name, entry); err != nil {
		return errors.Wrapf(err, "error moving the record of DataUpload %s to the orphan-snapshot ConfigMap", name)
	}
	// The removal leaves a tombstone in the completed set: an unchanged
	// tarball would otherwise resurrect the entry on the next attempt of the
	// backup deletion, with its attempt count reset to zero.
	return retainedsnapshot.TombstoneEntry(ctx, r.Client, backup, name)
}

// driverExists reports whether the cluster runs the named CSI driver.
func (r *retainedSnapshotReconciler) driverExists(ctx context.Context, driver string) (bool, error) {
	list := &storagev1api.CSIDriverList{}
	if err := r.Client.List(ctx, list); err != nil {
		return false, errors.Wrap(err, "error listing the CSIDrivers")
	}
	for i := range list.Items {
		if list.Items[i].Name == driver {
			return true, nil
		}
	}
	return false, nil
}

// garbageCollectShard sweeps a record family whose Backup is gone by other
// means, keyed on the backup-uid label: shard 0, when there is one, and every
// entry shard of the UID. A shard without pending entries is simply deleted;
// pending entries are moved to the orphan-snapshot ConfigMap with an event
// first, so no path deletes a pending record without leaving the orphan trace.
// An inert family -- entry shards and no shard 0, parked by the finalizer hold
// or left by a crash -- is swept the same way.
func (r *retainedSnapshotReconciler) garbageCollectShard(ctx context.Context, shard *corev1api.ConfigMap, log logrus.FieldLogger) error {
	// The Backup is gone, so the orphan entries are keyed by the backup name
	// and UID the shard still carries.
	backup := &velerov1api.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: shard.Namespace,
			Name:      shard.Labels[velerov1api.BackupNameLabel],
			UID:       types.UID(shard.Labels[velerov1api.BackupUIDLabel]),
		},
	}
	if name := shard.Annotations[velerov1api.RetainedSnapshotsBackupNameAnnotation]; name != "" {
		backup.Name = name
	}
	family, err := retainedsnapshot.LoadFamily(ctx, r.Client, backup)
	if err != nil {
		return err
	}
	// Shard 0 counts exactly the entry shards it was written for; the sweep
	// also takes the inert shards a crash or a later delivery left outside
	// that count.
	shards := map[string]*corev1api.ConfigMap{shard.Name: shard}
	for _, s := range family.Shards {
		shards[s.Name] = s
	}
	if family.Manifest != nil {
		shards[family.Manifest.Name] = family.Manifest
	}
	inert, err := retainedsnapshot.ListInertShards(ctx, r.Client, backup)
	if err != nil {
		return err
	}
	for _, s := range inert {
		shards[s.Name] = s
	}

	moved := 0
	for _, s := range shards {
		for _, name := range pendingShardKeys(s) {
			entry, err := retainedsnapshot.UnmarshalEntry(s.Data[name])
			if err != nil {
				entry = &retainedsnapshot.Entry{Attempts: retainedsnapshot.MaxAttempts}
			}
			if err := retainedsnapshot.WriteOrphanEntry(ctx, r.Client, backup, name, entry); err != nil {
				return errors.Wrapf(err, "error moving the record of DataUpload %s to the orphan-snapshot ConfigMap", name)
			}
			moved++
		}
	}
	if moved > 0 {
		r.eventf(shard, backup, "RetainedSnapshotOrphaned", "the backup is gone; %d pending retained snapshot records were moved to the orphan-snapshot ConfigMap for manual cleanup", moved)
		log.Warnf("The backup is gone; moved %d pending retained snapshot records to the orphan-snapshot ConfigMap", moved)
	}
	// Shard 0 first, then the entry shards, as the tail does.
	if family.Manifest != nil {
		if err := client.IgnoreNotFound(r.Delete(ctx, family.Manifest)); err != nil {
			return errors.Wrapf(err, "error deleting the retained snapshot record manifest %s", family.Manifest.Name)
		}
		delete(shards, family.Manifest.Name)
	}
	for _, s := range shards {
		if err := client.IgnoreNotFound(r.Delete(ctx, s)); err != nil {
			return errors.Wrapf(err, "error deleting the retained snapshot record shard %s", s.Name)
		}
	}
	return nil
}

// failDeleteBackupRequest marks the DeleteBackupRequest that waits on this
// family Processed with the deletion error, so the backup deletion fails and
// can be retried; a retry creates a new request that regathers from the
// ConfigMap, which is idempotent.
func (r *retainedSnapshotReconciler) failDeleteBackupRequest(ctx context.Context, backup *velerov1api.Backup, err error) error {
	message := fmt.Sprintf("backup deletion failed: error deleting the retained snapshots: %v", err)
	return r.patchWaitingRequest(ctx, backup, func(req *velerov1api.DeleteBackupRequest) {
		req.Status.Phase = velerov1api.DeleteBackupRequestPhaseProcessed
		req.Status.Errors = []string{message}
	})
}

func (r *retainedSnapshotReconciler) patchWaitingRequest(ctx context.Context, backup *velerov1api.Backup, mutate func(*velerov1api.DeleteBackupRequest)) error {
	list := &velerov1api.DeleteBackupRequestList{}
	// The request is selected by the backup UID alone: the requests carry a
	// sanitized backup-name label, which does not match the raw name of a
	// backup with a long name.
	selector := labels.SelectorFromSet(map[string]string{
		velerov1api.BackupUIDLabel: string(backup.UID),
	})
	if err := r.List(ctx, list, &client.ListOptions{Namespace: backup.Namespace, LabelSelector: selector}); err != nil {
		return errors.Wrap(err, "error listing the DeleteBackupRequests of the backup")
	}
	for i := range list.Items {
		req := &list.Items[i]
		if req.Status.Phase != velerov1api.DeleteBackupRequestPhaseInProgress ||
			req.Annotations[velerov1api.RetainedSnapshotsPendingAnnotation] == "" {
			continue
		}
		original := req.DeepCopy()
		mutate(req)
		if err := r.Patch(ctx, req, client.MergeFrom(original)); err != nil {
			return errors.Wrap(err, "error patching the DeleteBackupRequest")
		}
	}
	return nil
}

func (r *retainedSnapshotReconciler) eventf(shard *corev1api.ConfigMap, backup *velerov1api.Backup, reason, message string, args ...any) {
	if r.recorder == nil {
		r.logger.WithField("backup", backup.Name).Warningf(reason+": "+message, args...)
		return
	}
	r.recorder.Event(shard, true, reason, fmt.Sprintf(message, args...))
}
