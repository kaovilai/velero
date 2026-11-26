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
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/datapath"
	"github.com/velero-io/velero/pkg/exposer"
	"github.com/velero-io/velero/pkg/uploader/block"
	"github.com/velero-io/velero/pkg/util/csi"
	"github.com/velero-io/velero/pkg/util/datamover"
	"github.com/velero-io/velero/pkg/util/kube"
)

// The retention path below runs in the node agent, on the DataUpload
// reconciler, and writes snapshot.storage.k8s.io objects the rest of the data
// mover never touches. The verbs are exactly the ones the path exercises:
// reading and patching the deletion policy of the backup content, seeding its
// status so the snapshotter issues DeleteSnapshot, deleting the backup
// VolumeSnapshot, PVC and content, and polling for the PVC's and the content's
// disappearance. The node agent has no informer on these objects -- every
// access is a typed client call -- so no list or watch is granted.
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotcontents,verbs=get;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotcontents/status,verbs=get;update
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;delete

const (
	// dataUploadRetainLeaseAnnotation marks a DataUpload whose completion
	// callback is between the persisted record and the Retain patch of the
	// backup VolumeSnapshotContent. A rollback that finds an unexpired mark
	// fails and retries after the mark expires, so the two intents serialize
	// through the DataUpload object.
	dataUploadRetainLeaseAnnotation = "velero.io/dataupload-retain-snapshot-lease"

	// dataUploadRollbackLeaseAnnotation marks a DataUpload whose retained
	// snapshot is being released by the patch-back. Every patch-back actor
	// conditionally marks the DataUpload as rolling back as its first write,
	// which fails while an unexpired retaining mark exists.
	dataUploadRollbackLeaseAnnotation = "velero.io/dataupload-release-snapshot-lease"

	// dataUploadLeaseDuration is long enough for the two writes each lease
	// guards, and short enough that a crashed holder does not block the other
	// side for long.
	dataUploadLeaseDuration = time.Minute

	// dataUploadLeaseCleanupMargin is the unit of scheduling slack added to the
	// deletion timeout when the retention lease is renewed for the cleanup that
	// follows the Retain patch. The cleanup the renewal must cover is the
	// exposer cleanup -- itself bounded by one margin -- plus the explicit
	// content deletion and its bounded wait, so the renewal spans
	// RetainedSnapshotDeletionTimeout + 2*margin: a single margin is fully
	// consumed by the exposer cleanup's own bound and leaves the renewal
	// expiring while the deletion wait still runs.
	dataUploadLeaseCleanupMargin = time.Minute
)

// leaseRefusedError reports that a lease was not acquired because another
// holder's unexpired mark is on the DataUpload. It carries the remaining time
// of the blocking mark, so the caller requeues until that mark has expired --
// which a renewed retention mark can hold far beyond the lease duration --
// instead of relying on another event to retry.
type leaseRefusedError struct {
	error
	// retryAfter is how long the blocking mark still runs; zero when unknown,
	// e.g. the DataUpload reached a final state between the guard and the
	// acquisition attempt.
	retryAfter time.Duration
}

// retainedSnapshotReleaseTimeoutError reports that the patch-back's bounded
// wait for the content's disappearance expired. At the bound the DataUpload
// fails and the record stays in place, so the backup deletion's
// read-any-record rule owns the snapshot (design section 3); the reconcile
// hook marks the terminal failure instead of requeueing forever.
type retainedSnapshotReleaseTimeoutError struct {
	error
}

// dataUploadLease is the value of a lease annotation: the holder's identity and
// the time the lease expires.
type dataUploadLease struct {
	Holder    string
	ExpiresAt time.Time
}

func (l *dataUploadLease) String() string {
	return l.Holder + "/" + strconv.FormatInt(l.ExpiresAt.Unix(), 10)
}

// clearDataUploadLeaseAnnotations drops both lease marks from the DataUpload.
// The marks only fence the switch window before a terminal phase; once the
// phase is terminal nothing reads them, so the update that marks the terminal
// phase also removes them, instead of letting expired marks accumulate on the
// object for its remaining lifetime. Deleting from a nil map is a no-op.
func clearDataUploadLeaseAnnotations(du *velerov2alpha1api.DataUpload) {
	delete(du.Annotations, dataUploadRetainLeaseAnnotation)
	delete(du.Annotations, dataUploadRollbackLeaseAnnotation)
}

func parseDataUploadLease(value string) (*dataUploadLease, bool) {
	holder, expiry, found := strings.Cut(value, "/")
	if !found {
		return nil, false
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		return nil, false
	}
	return &dataUploadLease{Holder: holder, ExpiresAt: time.Unix(unix, 0)}, true
}

// tryAcquireDataUploadLease conditionally sets the lease annotation on the
// DataUpload to the given holder, expiring after the given duration. It fails,
// without writing, while another holder's unexpired lease is on either lease
// annotation -- the completion callback and the rollback path serialize their
// two intents through the one object, so an unexpired retaining mark blocks a
// rollback and an unexpired rolling-back mark blocks a retention. An absent,
// expired or own lease is (re-)acquired. The update is conflict-retried with a
// fresh read, so two racers cannot both acquire. When the acquisition is
// refused, the returned duration is the longest remaining time of a blocking
// mark, so the caller can wait out the actual holder instead of a fixed
// window; a final-state refusal reports zero.
func (r *DataUploadReconciler) tryAcquireDataUploadLease(ctx context.Context, du *velerov2alpha1api.DataUpload, annotation, holder string, duration time.Duration, log logrus.FieldLogger) (bool, time.Duration, error) {
	acquired := false
	blockedFor := time.Duration(0)
	err := UpdateDataUploadWithRetry(ctx, r.client, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, log, func(current *velerov2alpha1api.DataUpload) bool {
		// Every attempt reports its own outcome: an earlier, conflict-retried
		// attempt's acquisition must not leak out when a later attempt finds
		// another holder or a final state, or the caller would proceed without
		// owning the lease.
		acquired = false
		blockedFor = time.Duration(0)
		// The completion callback never retains for a DataUpload in any final
		// phase. The patch-back is refused only on a Completed one: a
		// Canceled or Failed DataUpload with a record must still be able to
		// release its snapshot.
		if current.Status.Phase == velerov2alpha1api.DataUploadPhaseCompleted ||
			(annotation == dataUploadRetainLeaseAnnotation && isDataUploadInFinalState(current)) {
			return false
		}
		for _, leaseAnnotation := range []string{dataUploadRetainLeaseAnnotation, dataUploadRollbackLeaseAnnotation} {
			if lease, ok := parseDataUploadLease(current.Annotations[leaseAnnotation]); ok &&
				lease.Holder != holder && r.Clock.Now().Before(lease.ExpiresAt) {
				if remaining := lease.ExpiresAt.Sub(r.Clock.Now()); remaining > blockedFor {
					blockedFor = remaining
				}
				return false
			}
		}
		if current.Annotations == nil {
			current.Annotations = make(map[string]string)
		}
		current.Annotations[annotation] = (&dataUploadLease{
			Holder:    holder,
			ExpiresAt: r.Clock.Now().Add(duration),
		}).String()
		acquired = true
		return true
	})
	if err != nil {
		return false, blockedFor, errors.Wrapf(err, "error acquiring the %s lease of DataUpload %s", annotation, du.Name)
	}
	return acquired, blockedFor, nil
}

// dataUploadLeaseHeld reports whether the lease annotation on the DataUpload is
// still held by the given holder and has not expired. The read goes through the
// uncached reader, so a throttled cached client cannot fail the check
// spuriously.
func (r *DataUploadReconciler) dataUploadLeaseHeld(ctx context.Context, du *velerov2alpha1api.DataUpload, annotation, holder string) (bool, error) {
	current := &velerov2alpha1api.DataUpload{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, current); err != nil {
		return false, errors.Wrap(err, "error reading the DataUpload lease")
	}
	lease, ok := parseDataUploadLease(current.Annotations[annotation])
	return ok && lease.Holder == holder && r.Clock.Now().Before(lease.ExpiresAt), nil
}

// dataUploadLeaseLive reports whether the annotation carries an unexpired lease
// of any holder, read uncached.
func (r *DataUploadReconciler) dataUploadLeaseLive(ctx context.Context, du *velerov2alpha1api.DataUpload, annotation string) (bool, error) {
	current := &velerov2alpha1api.DataUpload{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, current); err != nil {
		return false, errors.Wrap(err, "error reading the DataUpload lease")
	}
	lease, ok := parseDataUploadLease(current.Annotations[annotation])
	return ok && r.Clock.Now().Before(lease.ExpiresAt), nil
}

// uncachedReader reads DataUploads from the API server. The node-agent's cached
// client can return the pre-write object for a plain status update, and both
// the persisted-record verification and the lease re-check need what the API
// server actually holds. Without a manager, e.g. in tests, the client itself is
// a direct client.
func (r *DataUploadReconciler) uncachedReader() crclient.Reader {
	if r.apiReader != nil {
		return r.apiReader
	}
	return r.client
}

// shouldRetainSnapshot reports whether the DataUpload opted into base snapshot
// retention. The decision was made server-side, where the DataUpload was
// created, and travels in spec.dataMoverConfig; the node-agent never
// re-evaluates the volume policy, because the policy ConfigMap is not
// guaranteed to be readable from the node-agent.
func shouldRetainSnapshot(du *velerov2alpha1api.DataUpload) bool {
	return du.Spec.SnapshotType == velerov2alpha1api.SnapshotTypeCSI &&
		datamover.IsVeleroBlockDataMover(du.Spec.DataMover) &&
		du.Spec.DataMoverConfig[velerov2alpha1api.RetainSnapshotDataMoverConfigKey] == "true"
}

// retainSnapshotOnCompletion records the snapshot of a completed DataUpload and
// switches the backup VolumeSnapshotContent to Retain, so the snapshot survives
// the exposer cleanup that follows.
//
// The ordering is a hard rule, not an implementation detail:
//
//   - The record is persisted before the content is switched to Retain, because
//     a retained storage snapshot whose record was never serialized into the
//     backup tarball is an unrecoverable leak, while the other order at worst
//     deletes a snapshot and the next incremental falls back to a full backup.
//   - Persisted means verified: after the status write, the DataUpload is read
//     back through an uncached reader and the record is asserted to carry the
//     handle and the driver, before the content is patched. A cluster whose
//     DataUpload CRD predates the field prunes it on write while the update
//     still reports success, so the read-back, not the API response, is the
//     proof; if the field comes back empty, the caller fails the DataUpload and
//     the content keeps Delete.
//   - The completion callback and the rollback path are fenced against each
//     other by a lease on the DataUpload: the callback conditionally marks the
//     DataUpload as retaining (waiting out and taking a completion-side mark
//     another holder left, see acquireRetainLease), re-checks that the mark
//     is still its own immediately before it patches the content, and the
//     Retain patch is itself preconditioned on the content's resourceVersion,
//     captured right after the re-check and used without a re-read, so a
//     rollback that has patched or
//     deleted the content in between makes the delayed write fail instead of
//     landing after it.
//   - Every step is safe to re-enter, so a restarted node-agent finishes a
//     completion its predecessor died in: a record already present is kept as
//     it is, and a backup content that is gone, or being deleted under Retain,
//     while the record exists means the exposer cleanup already ran under
//     Retain, so the completion goes on to Completed and the release. A gone
//     content without a record, or after a patch-back marked the DataUpload,
//     still fails it.
//
// Any error returned here must fail the DataUpload and release the snapshot;
// the content keeps Delete in that case and section 3's patch-back reclaims it.
func (r *DataUploadReconciler) retainSnapshotOnCompletion(ctx context.Context, du *velerov2alpha1api.DataUpload, fallbackFull bool, log logrus.FieldLogger) error {
	if !shouldRetainSnapshot(du) {
		return nil
	}

	// The backup VSC is named after the DataUpload, see
	// csiSnapshotExposer.createBackupVSC. The exposer creates it synchronously,
	// with the handle already in spec.source.snapshotHandle.
	holder := fmt.Sprintf("%s-%d", r.nodeName, r.Clock.Now().UnixNano())
	vsc, err := r.csiSnapshotClient.VolumeSnapshotContents().Get(ctx, du.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			if resumed, resumeErr := r.resumeAfterRetainedCleanup(ctx, du, holder, log); resumed || resumeErr != nil {
				return resumeErr
			}
		}
		return errors.Wrapf(err, "error getting the backup VolumeSnapshotContent %s to retain its snapshot", du.Name)
	}
	record := csi.GetRetainedSnapshotFromVSC(vsc)
	if record == nil {
		return errors.Errorf("the backup VolumeSnapshotContent %s carries no snapshot handle to retain", du.Name)
	}
	record.CSIVolumeHandle = r.sourceCSIVolumeHandle(ctx, du, log)
	if !fallbackFull {
		record.BaseCSISnapshotHandle = r.baseSnapshotHandle(ctx, du, log)
	}

	if err := r.acquireRetainLease(ctx, du, holder, log); err != nil {
		return err
	}

	// A record already present is kept as it is: a restarted node-agent
	// re-enters the completion of a DataUpload whose dead predecessor already
	// recorded the snapshot.
	resumed := false
	existing := &velerov2alpha1api.DataUpload{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, existing); err != nil {
		return errors.Wrap(err, "error reading the DataUpload before recording its retained snapshot")
	}
	if kept := existing.Status.RetainedCSIStorageSnapshot; kept != nil {
		if kept.CSISnapshotHandle != record.CSISnapshotHandle || kept.Driver != record.Driver {
			return errors.Errorf("DataUpload %s records snapshot %s of driver %s, not the snapshot %s of its backup VolumeSnapshotContent", du.Name,
				kept.CSISnapshotHandle, kept.Driver, record.CSISnapshotHandle)
		}
		resumed = true
		record = kept
		log.Infof("DataUpload %s already records the retained snapshot %s, resuming its completion", du.Name, record.CSISnapshotHandle)
	} else if err := UpdateDataUploadWithRetry(ctx, r.client, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, log, func(current *velerov2alpha1api.DataUpload) bool {
		if isDataUploadInFinalState(current) {
			return false
		}
		current.Status.RetainedCSIStorageSnapshot = record
		return true
	}); err != nil {
		return errors.Wrapf(err, "error recording the retained snapshot %s on DataUpload %s", record.CSISnapshotHandle, du.Name)
	}

	// Persisted means verified, through an uncached read.
	updated := &velerov2alpha1api.DataUpload{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, updated); err != nil {
		return errors.Wrap(err, "error reading back the retained snapshot record")
	}
	persisted := updated.Status.RetainedCSIStorageSnapshot
	if persisted == nil || persisted.CSISnapshotHandle != record.CSISnapshotHandle || persisted.Driver != record.Driver {
		return errors.Errorf("the retained snapshot record of DataUpload %s did not persist, the DataUpload CRD may predate the %s field and must be upgraded before enabling retention",
			du.Name, "retainedCSIStorageSnapshot")
	}
	log.Infof("Recorded retained snapshot %s of driver %s on DataUpload %s", record.CSISnapshotHandle, record.Driver, du.Name)

	// The Retain patch is preconditioned on the resourceVersion of a read that
	// shows the content still live, and the lease is re-checked after that
	// read, not before it: a patch-back that took over between a lease check
	// and the read -- the lease expired, or the content was deleted under it
	// -- is then caught either way. The read aborts on a content that is gone
	// or being deleted; the re-check aborts when the callback's mark is no
	// longer its own or the rollback mark is live. Only then is the content
	// patched, with that read's resourceVersion and no further re-read, so a
	// rollback write that lands after the re-check makes the patch fail with a
	// conflict instead of landing after it. The read is taken here and not at
	// the top of this function: the snapshot controllers update the content
	// routinely (finalizers, the being-deleted annotation, status) during the
	// lease acquisition, the record persist and the read-back, and a
	// resourceVersion captured there fails the patch spuriously, which fails
	// the DataUpload and degrades the next incremental to a full backup.
	fresh, err := r.csiSnapshotClient.VolumeSnapshotContents().Get(ctx, du.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The rollback path already removed the content; the record is kept
			// and the backup deletion owns the snapshot.
			return errors.Wrapf(err, "the backup VolumeSnapshotContent %s vanished before its snapshot was retained", du.Name)
		}
		return errors.Wrapf(err, "error re-reading the backup VolumeSnapshotContent %s before retaining its snapshot", du.Name)
	}
	if fresh.DeletionTimestamp != nil {
		if resumed && fresh.Spec.DeletionPolicy == snapshotv1api.VolumeSnapshotContentRetain && !hasRollbackMark(existing) {
			// The predecessor's cleanup is deleting the content under Retain:
			// the snapshot survives, the completion goes on.
			log.Infof("The backup VolumeSnapshotContent %s is being deleted under Retain, the cleanup already ran", du.Name)
			return r.renewRetainLeaseForCleanup(ctx, du, holder, log)
		}
		return errors.Errorf("the backup VolumeSnapshotContent %s is being deleted, not retaining its snapshot", du.Name)
	}
	held, err := r.dataUploadLeaseHeld(ctx, du, dataUploadRetainLeaseAnnotation, holder)
	if err != nil {
		return err
	}
	if !held {
		return errors.Errorf("the retention lease of DataUpload %s was lost before its snapshot was retained", du.Name)
	}
	rollingBack, err := r.dataUploadLeaseLive(ctx, du, dataUploadRollbackLeaseAnnotation)
	if err != nil {
		return err
	}
	if rollingBack {
		return errors.Errorf("a rollback of DataUpload %s is in progress, not retaining its snapshot", du.Name)
	}
	if _, err := csi.PatchVSCDeletionPolicy(ctx, r.csiSnapshotClient, fresh.Name, snapshotv1api.VolumeSnapshotContentRetain, fresh.ResourceVersion); err != nil {
		if apierrors.IsNotFound(err) {
			// The rollback path already removed the content; the record is kept
			// and the backup deletion owns the snapshot.
			return errors.Wrapf(err, "the backup VolumeSnapshotContent %s vanished before its snapshot was retained", du.Name)
		}
		return errors.Wrapf(err, "error switching the backup VolumeSnapshotContent %s to Retain", vsc.Name)
	}
	log.Infof("Switched the backup VolumeSnapshotContent %s to Retain, snapshot %s survives the cleanup", vsc.Name, record.CSISnapshotHandle)

	// The fence the lease buys holds until the Completed mark, not only until
	// the patch: the retention-path cleanup that follows (the exposer cleanup
	// and the explicit content deletion, bounded by the deletion timeout)
	// outlasts the one-minute lease, and an expired, stealable mark there
	// would let the reconcile hook patch the content back to Delete and
	// destroy a successfully retained snapshot -- the next incremental of the
	// volume would silently fall back to a full backup. The lease is
	// therefore renewed, as the same holder, to cover the whole bounded
	// cleanup -- the exposer cleanup's own bound plus the content deletion's
	// deletion-timeout bound -- which outlasts a single margin; a callback
	// that crashes after the renewal blocks the rollback for at most that
	// long, which the deletion-timeout bound already tolerates. A refused
	// renewal means a rollback is running or ran: the callback must not
	// continue, it fails the DataUpload and keeps the record, leaving the
	// snapshot to the patch-back and the backup deletion.
	return r.renewRetainLeaseForCleanup(ctx, du, holder, log)
}

// renewRetainLeaseForCleanup renews the completion side's mark, as the same
// holder, to cover the bounded cleanup that follows the Retain patch.
func (r *DataUploadReconciler) renewRetainLeaseForCleanup(ctx context.Context, du *velerov2alpha1api.DataUpload, holder string, log logrus.FieldLogger) error {
	renewed, _, err := r.tryAcquireDataUploadLease(ctx, du, dataUploadRetainLeaseAnnotation, holder, csi.RetainedSnapshotDeletionTimeout+2*dataUploadLeaseCleanupMargin, log)
	if err != nil {
		return errors.Wrapf(err, "error renewing the retention lease of DataUpload %s for its cleanup", du.Name)
	}
	if !renewed {
		return errors.Errorf("another actor holds the retention lease of DataUpload %s, not retaining its snapshot for the cleanup", du.Name)
	}
	return nil
}

// resumeAfterRetainedCleanup handles a completion re-entered by a restarted
// node-agent whose backup VolumeSnapshotContent is NotFound. With a record
// already on the DataUpload, the predecessor recorded the snapshot, switched
// the content to Retain and ran the exposer cleanup before it died, so the
// snapshot survives and the completion goes on to Completed and the release:
// it reports resumed after taking over the completion-side lease. Without a
// record, or after a patch-back ever marked the DataUpload -- its deletion may
// have removed the snapshot -- it reports not resumed and the caller fails the
// DataUpload as before.
func (r *DataUploadReconciler) resumeAfterRetainedCleanup(ctx context.Context, du *velerov2alpha1api.DataUpload, holder string, log logrus.FieldLogger) (bool, error) {
	resumable := func() (bool, error) {
		current := &velerov2alpha1api.DataUpload{}
		if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, current); err != nil {
			return false, errors.Wrap(err, "error reading the DataUpload to resume its completion")
		}
		return current.Status.RetainedCSIStorageSnapshot != nil && !hasRollbackMark(current), nil
	}
	if ok, err := resumable(); !ok || err != nil {
		return false, err
	}
	if err := r.acquireRetainLease(ctx, du, holder, log); err != nil {
		return true, err
	}
	// Re-read under the lease: a patch-back that marked the DataUpload in
	// between leaves the decision to it.
	if ok, err := resumable(); err != nil {
		return true, err
	} else if !ok {
		return false, nil
	}
	log.Infof("The backup VolumeSnapshotContent %s is gone and DataUpload %s records its snapshot: the cleanup already ran under Retain, resuming the completion", du.Name, du.Name)
	return true, r.renewRetainLeaseForCleanup(ctx, du, holder, log)
}

// hasRollbackMark reports whether a patch-back ever marked the DataUpload,
// expired or not. The completion side never writes that mark.
func hasRollbackMark(du *velerov2alpha1api.DataUpload) bool {
	_, ok := du.Annotations[dataUploadRollbackLeaseAnnotation]
	return ok
}

// acquireRetainLease acquires the completion side's lease mark for holder. A
// mark of the completion side left by another holder -- a node-agent that died
// inside the completion callback, whose restarted successor now runs the
// callback again -- is treated like an expired mark: the callback waits out
// its expiry and takes it, and never fails the DataUpload on that refusal, so
// the restarted node-agent finishes the completion. The wait is bounded by the
// longest mark the completion side writes, the renewal that covers the
// cleanup, plus one lease duration. A live patch-back mark, or a DataUpload
// that reached a final phase, refuses for good: the patch-back owns the
// snapshot then.
func (r *DataUploadReconciler) acquireRetainLease(ctx context.Context, du *velerov2alpha1api.DataUpload, holder string, log logrus.FieldLogger) error {
	deadline := r.Clock.Now().Add(csi.RetainedSnapshotDeletionTimeout + 2*dataUploadLeaseCleanupMargin + dataUploadLeaseDuration)
	for {
		acquired, blockedFor, err := r.tryAcquireDataUploadLease(ctx, du, dataUploadRetainLeaseAnnotation, holder, dataUploadLeaseDuration, log)
		if err != nil {
			return err
		}
		if acquired {
			return nil
		}
		rollingBack, err := r.dataUploadLeaseLive(ctx, du, dataUploadRollbackLeaseAnnotation)
		if err != nil {
			return err
		}
		if rollingBack {
			return errors.Errorf("a rollback of DataUpload %s is in progress, not retaining its snapshot", du.Name)
		}
		if blockedFor <= 0 {
			return errors.Errorf("DataUpload %s reached a final phase before its snapshot was retained", du.Name)
		}
		if r.Clock.Now().Add(blockedFor).After(deadline) {
			return errors.Errorf("another holder keeps the retention lease of DataUpload %s beyond the longest completion-side mark", du.Name)
		}
		log.Infof("The retention lease of DataUpload %s is held by another holder, e.g. a node-agent that died inside the completion callback; taking it once it expires in %v", du.Name, blockedFor)
		if err := ctx.Err(); err != nil {
			return err
		}
		r.Clock.Sleep(blockedFor + time.Second)
	}
}

// sourceCSIVolumeHandle returns the CSI volume handle of the DataUpload's source
// volume, which keys the rolling-base chain with the driver. A lookup failure
// leaves it empty: the snapshot is still retained and deleted with its backup,
// but no newer backup releases it early.
func (r *DataUploadReconciler) sourceCSIVolumeHandle(ctx context.Context, du *velerov2alpha1api.DataUpload, log logrus.FieldLogger) string {
	pvc, err := r.kubeClient.CoreV1().PersistentVolumeClaims(du.Spec.SourceNamespace).Get(ctx, du.Spec.SourcePVC, metav1.GetOptions{})
	if err != nil || pvc.Spec.VolumeName == "" {
		log.WithError(err).Warnf("Failed to read the source PVC %s/%s, the retained snapshot is not chained for the rolling base", du.Spec.SourceNamespace, du.Spec.SourcePVC)
		return ""
	}
	pv, err := r.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil || pv.Spec.CSI == nil {
		log.WithError(err).Warnf("Failed to read the CSI volume handle of PV %s, the retained snapshot is not chained for the rolling base", pvc.Spec.VolumeName)
		return ""
	}
	return pv.Spec.CSI.VolumeHandle
}

// baseSnapshotHandle returns the retained snapshot an incremental DataUpload's
// changed blocks were computed against: the record of the most recently
// completed DataUpload of the same PVC, which is the parent the block data mover
// selects. A DataUpload that read the volume in full has no base.
func (r *DataUploadReconciler) baseSnapshotHandle(ctx context.Context, du *velerov2alpha1api.DataUpload, log logrus.FieldLogger) string {
	previous := r.previousCompletedDataUpload(ctx, du, log)
	if previous == nil || previous.Status.RetainedCSIStorageSnapshot == nil {
		return ""
	}
	return previous.Status.RetainedCSIStorageSnapshot.CSISnapshotHandle
}

// confirmRetainedSnapshotCleanup finishes the exposer cleanup on the retention
// path. The exposer cleanup deletes the backup pod, PVC and VolumeSnapshot, but
// a VolumeSnapshot deletion cascades to its content only when the content's
// policy is Delete, so under Retain the content object would stay behind, one
// per volume per backup, while the storage snapshot survives as intended. The
// content is therefore deleted explicitly, and the Completed mark is held until
// the intended end state is confirmed: deleting a Retain content is exactly
// what leaves the storage snapshot in place.
func (r *DataUploadReconciler) confirmRetainedSnapshotCleanup(ctx context.Context, du *velerov2alpha1api.DataUpload, log logrus.FieldLogger) error {
	if !shouldRetainSnapshot(du) {
		return nil
	}

	vsc, err := r.csiSnapshotClient.VolumeSnapshotContents().Get(ctx, du.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "error getting the backup VolumeSnapshotContent %s to clean it up", du.Name)
	}
	if vsc.Spec.DeletionPolicy != snapshotv1api.VolumeSnapshotContentRetain {
		// Never switched to Retain; the VolumeSnapshot deletion cascades.
		return nil
	}

	if err := r.csiSnapshotClient.VolumeSnapshotContents().Delete(ctx, du.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "error deleting the retained backup VolumeSnapshotContent %s", du.Name)
	}
	if err := csi.EnsureDeleteVSC(ctx, r.csiSnapshotClient, du.Name, csi.RetainedSnapshotDeletionTimeout); err != nil {
		return errors.Wrapf(err, "the retained backup VolumeSnapshotContent %s is not gone", du.Name)
	}
	log.Infof("Deleted the retained backup VolumeSnapshotContent %s, the storage snapshot stays", du.Name)
	return nil
}

// releaseRetainedSnapshot reclaims the snapshot of a DataUpload that did not
// complete: it patches the backup VolumeSnapshotContent, never the user's
// VolumeSnapshotClass, back to Delete before the cleanup, so the CSI driver
// deletes the snapshot instead of leaving it in the storage untracked.
//
// The patch-back is owned by the reconcile, not only by the failing code path:
// a hard kill of the node-agent (OOM, force delete) inside the switch window
// leaves no in-process retry, so any reconciler must be able to run it. It is
// idempotent and safe for any node-agent to run, last-writer-wins on each
// step; the backup VolumeSnapshotContent, VolumeSnapshot and PVC are all named
// after the DataUpload.
//
// The patch-back is ordered delete-first, clear-last:
//
//   - seed status.snapshotHandle, so the sidecar never sees a Delete content
//     without it;
//   - patch the content to Delete with a resourceVersion precondition;
//   - delete the backup PVC, the backup VolumeSnapshot and the backup
//     VolumeSnapshotContent. The PVC deletion is a prerequisite because the
//     common snapshot controller refuses to delete a VolumeSnapshot that a
//     pending claim is being provisioned from, reporting SnapshotDeletePending,
//     and a failed exposer leaves exactly that state when it dies after
//     creating the backup PVC. The VolumeSnapshot deletion is a prerequisite
//     because the snapshotter deletes a bound content only after the snapshot
//     deletion has begun, so deleting the content alone leaves it waiting;
//   - wait for the content to disappear, bounded by the deletion timeout, so a
//     driver that keeps failing DeleteSnapshot cannot block the reconcile
//     forever. At the bound, the DataUpload fails, the record stays in place
//     so backup deletion owns the snapshot;
//   - only then clear the record. Confirming the policy is not release: a
//     Delete content still has to be deleted for the sidecar to call
//     DeleteSnapshot, and a crash between a record clear and that deletion
//     would strand the snapshot with no record at all, the one leak this path
//     exists to prevent.
//
// A NotFound at any step clears nothing: the content can be absent because the
// retention-path cleanup already deleted it under a Retain policy -- the
// storage snapshot then still exists -- after a Completed update that failed to
// stick. Clearing the record on absence alone would strand a retained snapshot
// with no cleanup record, so the record is kept and the backup deletion owns
// the snapshot by rebuilding the temp content from it, which is safe whether
// the snapshot exists or not.
func (r *DataUploadReconciler) releaseRetainedSnapshot(ctx context.Context, du *velerov2alpha1api.DataUpload, log logrus.FieldLogger) error {
	if du.Spec.SnapshotType != velerov2alpha1api.SnapshotTypeCSI {
		return nil
	}

	// The patch-back no-ops on a Completed DataUpload only: a patch-back that
	// ran there would delete the base snapshot of a completed backup. A
	// Canceled or Failed DataUpload with a record did not complete -- the
	// cancel path marks the object Canceled before it cleans up, the prepare
	// timeout marks it Failed -- and its snapshot is released like any other
	// unfinished data movement's, instead of being left to the backup
	// deletion. The in-memory DataUpload can be stale, so the guard reads the
	// object the API server holds.
	current := &velerov2alpha1api.DataUpload{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, current); err != nil {
		return errors.Wrap(err, "error reading the DataUpload before releasing its snapshot")
	}
	if current.Status.Phase == velerov2alpha1api.DataUploadPhaseCompleted {
		log.Infof("DataUpload %s is Completed, not releasing its snapshot; its record is owned by the rolling base and the backup deletion", du.Name)
		return nil
	}

	vsc, err := r.csiSnapshotClient.VolumeSnapshotContents().Get(ctx, du.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if du.Status.RetainedCSIStorageSnapshot != nil {
			log.Infof("The backup VolumeSnapshotContent %s is gone; keeping the retained snapshot record so the backup deletion owns the snapshot", du.Name)
		}
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "error getting the backup VolumeSnapshotContent %s to release its snapshot", du.Name)
	}

	record := du.Status.RetainedCSIStorageSnapshot
	if record == nil && vsc.Spec.DeletionPolicy != snapshotv1api.VolumeSnapshotContentRetain {
		// Nothing was retained: the exposer cleanup reclaims the snapshot
		// through the VolumeSnapshot deletion cascade.
		return nil
	}

	// Lease fence: the patch-back marks its intent first and fails while an
	// unexpired retaining mark exists, retrying after it expires, so the two
	// intents serialize through one object. Both marks carry the same
	// expiry and steal an expired mark, so a crashed holder blocks the other
	// side for at most the lease duration.
	holder := fmt.Sprintf("%s-%d", r.nodeName, r.Clock.Now().UnixNano())
	acquired, blockedFor, err := r.tryAcquireDataUploadLease(ctx, du, dataUploadRollbackLeaseAnnotation, holder, dataUploadLeaseDuration, log)
	if err != nil {
		return err
	}
	if !acquired {
		// A refused mark must drive a retry: nothing else re-enqueues the
		// DataUpload for this, the node-agent has no sync period. The caller
		// requeues for the blocking mark's remaining time, which a renewed
		// retention mark can hold far beyond the lease duration.
		return &leaseRefusedError{
			error:      errors.Errorf("another actor holds the retention lease of DataUpload %s, retrying the release after it expires", du.Name),
			retryAfter: blockedFor,
		}
	}

	// Seed status.snapshotHandle before the policy is flipped to Delete and
	// before any deletion: the sidecar only issues DeleteSnapshot for a
	// content being deleted when its policy is Delete and its status carries
	// the handle, otherwise it removes the finalizer with no CSI call and the
	// content's disappearance would read as release while the snapshot leaks.
	// Seeding first closes the window in which the content is already Delete
	// -- flipped here or by a restart of this path -- while the status is
	// still empty, and a VolumeSnapshot deletion cascades onto it: the sidecar
	// would drop the finalizer with no DeleteSnapshot and the confirmation
	// below would clear the only record while the snapshot leaks.
	seedHandle := ""
	if record != nil {
		seedHandle = record.CSISnapshotHandle
	}
	if seedHandle == "" && vsc.Spec.Source.SnapshotHandle != nil {
		seedHandle = *vsc.Spec.Source.SnapshotHandle
	}
	// The seeded object's resourceVersion is intentionally not carried into
	// the delete below: the PVC wait that follows gives the sidecar a window
	// to write status and bump it, so the delete re-reads the content instead.
	seeded, err := csi.SeedVSCStatusSnapshotHandle(ctx, r.csiSnapshotClient, vsc, seedHandle)
	if err != nil {
		// A content that vanished before the seed released nothing under this
		// path's control; the record is kept and the backup deletion owns the
		// snapshot.
		return errors.Wrapf(err, "error seeding the status of the backup VolumeSnapshotContent %s before releasing its snapshot", vsc.Name)
	}
	vsc = seeded

	if vsc.Spec.DeletionPolicy == snapshotv1api.VolumeSnapshotContentRetain {
		// A raced Retain write makes this fail and be retried instead of
		// slipping between a confirm and a delete: the patch carries the
		// content's captured resourceVersion, so a concurrent write in between
		// makes it fail with a conflict instead of landing after it.
		//
		// Every write bumps the content's resourceVersion, so each subsequent
		// preconditioned operation is built on the object its predecessor
		// returned: the Delete patch is preconditioned on the object the
		// status seed returned, and the delete uses the resourceVersion of the
		// re-read that follows the PVC wait, not the post-patch one, because
		// the sidecar writes status during that wait. Chaining the
		// resourceVersion of the Get at the top instead would self-conflict on
		// every Retain-to-Delete release against a real API server, and the
		// one-shot callers -- errorOut, cleanUp, onPrepareTimeout -- would
		// never converge. The fake snapshot clientset enforces neither the
		// delete preconditions nor the resourceVersion bumps, so this is
		// proven by the focused unit test asserting the resourceVersions the
		// operations carry.
		patched, err := csi.PatchVSCDeletionPolicy(ctx, r.csiSnapshotClient, vsc.Name, snapshotv1api.VolumeSnapshotContentDelete, vsc.ResourceVersion)
		if err != nil {
			return errors.Wrapf(err, "error switching the backup VolumeSnapshotContent %s back to Delete", vsc.Name)
		}
		vsc = patched
		log.Infof("Switched the backup VolumeSnapshotContent %s back to Delete to release its snapshot", vsc.Name)
	}

	// The PVC is a prerequisite of the VolumeSnapshot deletion, see the comment
	// above; it is owner-referenced by the DataUpload, so nothing but this path
	// removes it before DataUpload deletion.
	kube.DeletePVCIfAny(ctx, r.kubeClient.CoreV1(), du.Name, du.Namespace, time.Minute, log)

	vs, err := r.csiSnapshotClient.VolumeSnapshots(du.Namespace).Get(ctx, du.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "error getting the backup VolumeSnapshot %s/%s", du.Namespace, du.Name)
	}
	if err == nil {
		if err := csi.DeleteVSWithResourceVersion(ctx, r.csiSnapshotClient, du.Namespace, du.Name, vs.ResourceVersion); err != nil {
			return err
		}
	}

	// The bounded PVC wait above sits between the status seed and the content
	// delete, and the sidecar's own status writes in that window bump the
	// content's resourceVersion: deleting with the post-seed resourceVersion
	// would conflict on the first pass against a real API server. The content
	// is re-read after the wait and deleted with the resourceVersion that read
	// carries; a content that vanished in between needs no delete at all, the
	// confirmation below absorbs it.
	postWait, err := r.csiSnapshotClient.VolumeSnapshotContents().Get(ctx, du.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "error re-reading the backup VolumeSnapshotContent %s before deleting it", du.Name)
	}
	if err == nil {
		if err := csi.DeleteVSCWithResourceVersion(ctx, r.csiSnapshotClient, du.Name, postWait.ResourceVersion); err != nil {
			return err
		}
	}

	if err := csi.EnsureDeleteVSC(ctx, r.csiSnapshotClient, du.Name, csi.RetainedSnapshotDeletionTimeout); err != nil {
		// At the bound, the DataUpload fails and the record is kept so the
		// backup deletion owns the snapshot (design section 3). The typed
		// error carries that rule to the reconcile hook, which marks the
		// terminal failure instead of requeueing every lease duration
		// forever.
		var timeout *csi.VSCDeletionTimeoutError
		if errors.As(err, &timeout) {
			return &retainedSnapshotReleaseTimeoutError{errors.Wrapf(err, "error confirming the release of the snapshot of DataUpload %s", du.Name)}
		}
		return errors.Wrapf(err, "error confirming the release of the snapshot of DataUpload %s", du.Name)
	}

	// Delete-first, clear-last.
	if record != nil {
		if err := UpdateDataUploadWithRetry(ctx, r.client, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, log, func(current *velerov2alpha1api.DataUpload) bool {
			current.Status.RetainedCSIStorageSnapshot = nil
			return true
		}); err != nil {
			return errors.Wrapf(err, "error clearing the retained snapshot record of DataUpload %s", du.Name)
		}
		log.Infof("Released the retained snapshot %s of DataUpload %s, the record is cleared", record.CSISnapshotHandle, du.Name)
	}
	return nil
}

// releaseOlderRetainedSnapshots releases, through the exposer, every retained
// snapshot of the completed DataUpload's chain -- the same driver and CSI
// volume handle -- that an older DataUpload recorded and no backup released
// yet, oldest first. That is the direct parent, and any older snapshot an
// earlier release missed: the catch-up. Each release marks the owning
// DataUpload's record released by the completed DataUpload's backup and emits
// RetainedSnapshotReleased.
//
// It runs after the Completed write, never before: a crash in between leaves a
// Completed DataUpload with an unreleased parent, which the next backup of the
// chain catches up. A release never fails the backup. One that times out or
// errors leaves its static content being deleted, emits
// RetainedSnapshotReleaseTimedOut and keeps the record unreleased, so the next
// backup of the volume retries it; that retry checks a content still being
// deleted once instead of waiting on it again.
func (r *DataUploadReconciler) releaseOlderRetainedSnapshots(ctx context.Context, key types.NamespacedName, log logrus.FieldLogger) {
	holder := &velerov2alpha1api.DataUpload{}
	if err := r.uncachedReader().Get(ctx, key, holder); err != nil {
		log.WithError(err).Warn("Failed to read the completed DataUpload, not releasing the older retained snapshots of its volume")
		return
	}
	chain := holder.Status.RetainedCSIStorageSnapshot
	if holder.Status.Phase != velerov2alpha1api.DataUploadPhaseCompleted || chain == nil || chain.Released ||
		chain.CSIVolumeHandle == "" || chain.Driver == "" {
		return
	}

	all := &velerov2alpha1api.DataUploadList{}
	if err := r.client.List(ctx, all, crclient.InNamespace(holder.Namespace)); err != nil {
		log.WithError(err).Warn("Failed to list the DataUploads, not releasing the older retained snapshots of the volume")
		return
	}
	timeout := csi.RetainedSnapshotDeletionTimeout
	releasedBy := holder.Labels[velerov1api.BackupUIDLabel]
	for _, candidate := range releaseCandidates(holder, all.Items) {
		record := candidate.Status.RetainedCSIStorageSnapshot
		candidateLog := log.WithFields(logrus.Fields{"releasedDataUpload": candidate.Name, "snapshot": record.CSISnapshotHandle})
		if err := exposer.ReleaseRetainedSnapshot(ctx, r.client, r.uncachedReader(), record, types.UID(candidate.Labels[velerov1api.BackupUIDLabel]), timeout, candidateLog); err != nil {
			candidateLog.WithError(err).Warn("Failed to release the retained snapshot, a later backup of the volume retries it")
			r.events().Event(holder, true, "RetainedSnapshotReleaseTimedOut",
				"the retained snapshot %s of DataUpload %s was not released: %v; a later backup of the volume retries it", record.CSISnapshotHandle, candidate.Name, err)
			continue
		}
		if err := UpdateDataUploadWithRetry(ctx, r.client, types.NamespacedName{Namespace: candidate.Namespace, Name: candidate.Name}, candidateLog, func(current *velerov2alpha1api.DataUpload) bool {
			if current.Status.RetainedCSIStorageSnapshot == nil || current.Status.RetainedCSIStorageSnapshot.Released {
				return false
			}
			current.Status.RetainedCSIStorageSnapshot.Released = true
			current.Status.RetainedCSIStorageSnapshot.ReleasedBy = releasedBy
			return true
		}); err != nil && !apierrors.IsNotFound(err) {
			// The snapshot is gone; a record left unreleased is deleted again
			// by its backup deletion, which finds nothing to delete.
			candidateLog.WithError(err).Warn("Failed to mark the retained snapshot record released")
			continue
		}
		candidateLog.Infof("Released the retained snapshot %s of DataUpload %s, DataUpload %s holds the newer base of the volume", record.CSISnapshotHandle, candidate.Name, holder.Name)
		r.events().Event(holder, false, "RetainedSnapshotReleased",
			"the retained snapshot %s of DataUpload %s was released: DataUpload %s of backup %s holds the newer base of the volume", record.CSISnapshotHandle, candidate.Name, holder.Name, releasedBy)
	}
}

// releaseCandidates returns the DataUploads whose unreleased retained snapshot
// belongs to the holder's chain and completed before the holder, oldest first.
func releaseCandidates(holder *velerov2alpha1api.DataUpload, all []velerov2alpha1api.DataUpload) []*velerov2alpha1api.DataUpload {
	chain := holder.Status.RetainedCSIStorageSnapshot
	holderDone := dataUploadCompletion(holder)
	var out []*velerov2alpha1api.DataUpload
	for i := range all {
		du := &all[i]
		record := du.Status.RetainedCSIStorageSnapshot
		if du.Name == holder.Name || record == nil || record.Released || !du.DeletionTimestamp.IsZero() ||
			record.Driver != chain.Driver || record.CSIVolumeHandle != chain.CSIVolumeHandle ||
			record.CSISnapshotHandle == "" || record.CSISnapshotHandle == chain.CSISnapshotHandle {
			continue
		}
		if !dataUploadCompletion(du).Before(holderDone) {
			continue
		}
		out = append(out, du)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return dataUploadCompletion(out[i]).Before(dataUploadCompletion(out[j]))
	})
	return out
}

// dataUploadCompletion returns when the DataUpload completed, falling back to
// its creation for an object without a completion timestamp.
func dataUploadCompletion(du *velerov2alpha1api.DataUpload) time.Time {
	if du.Status.CompletionTimestamp != nil {
		return du.Status.CompletionTimestamp.Time
	}
	return du.CreationTimestamp.Time
}

// events returns the reconciler's event recorder, built on first use.
func (r *DataUploadReconciler) events() kube.EventRecorder {
	r.eventRecorderOnce.Do(func() {
		if r.eventRecorder == nil {
			r.eventRecorder = kube.NewEventRecorder(r.kubeClient, r.client.Scheme(), "velero-data-upload", r.nodeName, r.logger)
		}
	})
	return r.eventRecorder
}

// fallbackReasonOf returns the reason a velero-block incremental fell back to
// a full read, as the uploader classified it, refined with what the node-agent
// can see: when the driver failed to compute the delta, and the previous
// completed DataUpload of the same PVC carries no retention record, the base
// snapshot was not retained -- the most common Case 2 cause, which the
// driver's own error (Ceph RBD answers a deleted base with an internal error)
// cannot name. When the parent the uploader took is the repository snapshot
// of a movement that ended failed or canceled, whose storage snapshot the
// patch-back removed, the reason names that DataUpload instead of the
// previous completed one. The uploader's reason stays as the detail.
func (r *DataUploadReconciler) fallbackReasonOf(ctx context.Context, du *velerov2alpha1api.DataUpload, result datapath.BackupResult, log logrus.FieldLogger) string {
	if !result.FallbackFull {
		return ""
	}
	reason := result.FallbackReason
	if reason == "" {
		reason = "fallen back to a full read"
	}
	if !datamover.IsVeleroBlockDataMover(du.Spec.DataMover) ||
		strings.HasPrefix(reason, block.FallbackReasonNoParent) ||
		strings.HasPrefix(reason, block.FallbackReasonCBTUnavailable) {
		return reason
	}

	// The block uploader takes the newest repository snapshot of the volume as
	// the parent whatever the DataUpload's phase, so a movement that ended
	// failed or canceled after its repository snapshot was committed is the
	// parent, and its own storage snapshot was removed by the patch-back.
	// Choosing a parent whose base is still retained is Open Issue 5; the
	// reason names the parent actually used.
	if parent := r.parentRepositorySnapshotOwner(ctx, du, log); parent != nil && parent.Status.Phase != velerov2alpha1api.DataUploadPhaseCompleted {
		return fmt.Sprintf("parent repository snapshot %s belongs to DataUpload %s (%s), whose snapshot was not retained; %s",
			parent.Status.SnapshotID, parent.Name, parent.Status.Phase, reason)
	}

	previous := r.previousCompletedDataUpload(ctx, du, log)
	if previous == nil {
		return reason
	}
	if previous.Status.RetainedCSIStorageSnapshot == nil {
		return fmt.Sprintf("base snapshot not retained by the previous backup (DataUpload %s); %s", previous.Name, reason)
	}
	if record := previous.Status.RetainedCSIStorageSnapshot; record.Released {
		// The backup that held the rolling base was deleted, so the parent is
		// an older backup whose snapshot was released when the base moved on.
		return fmt.Sprintf("base snapshot %s of the previous backup (DataUpload %s) was released as the rolling base moved on (released by backup %s), and the backup holding the newer base is gone; %s",
			record.CSISnapshotHandle, previous.Name, record.ReleasedBy, reason)
	}
	return fmt.Sprintf("base snapshot %s of the previous backup (DataUpload %s) was retained but could not be used; %s",
		previous.Status.RetainedCSIStorageSnapshot.CSISnapshotHandle, previous.Name, reason)
}

// parentRepositorySnapshotOwner returns the DataUpload whose repository
// snapshot the block uploader took as the parent of du, mirroring its choice
// (pkg/uploader/block/snapshot.go findPreviousSnapshot: the newest repository
// snapshot of the source by start time, whatever the outcome of the movement
// that wrote it): among the velero-block DataUploads of the same PVC and
// backup storage location that recorded a repository snapshot and started
// before du, the one that started last. Nil when there is none or the lookup
// fails; the lookup only refines a diagnostic.
func (r *DataUploadReconciler) parentRepositorySnapshotOwner(ctx context.Context, du *velerov2alpha1api.DataUpload, log logrus.FieldLogger) *velerov2alpha1api.DataUpload {
	pvcUID := du.Labels[velerov1api.PVCUIDLabel]
	if pvcUID == "" || du.Status.StartTimestamp == nil {
		return nil
	}
	list := &velerov2alpha1api.DataUploadList{}
	if err := r.client.List(ctx, list, crclient.InNamespace(du.Namespace), crclient.MatchingLabels{velerov1api.PVCUIDLabel: pvcUID}); err != nil {
		log.WithError(err).Debug("Failed to list the DataUploads of the PVC for the fallback reason")
		return nil
	}
	var parent *velerov2alpha1api.DataUpload
	for i := range list.Items {
		candidate := &list.Items[i]
		if candidate.Name == du.Name || candidate.Status.SnapshotID == "" || candidate.Status.StartTimestamp == nil ||
			!datamover.IsVeleroBlockDataMover(candidate.Spec.DataMover) ||
			candidate.Spec.BackupStorageLocation != du.Spec.BackupStorageLocation ||
			!candidate.Status.StartTimestamp.Before(du.Status.StartTimestamp) {
			continue
		}
		if parent == nil || candidate.Status.StartTimestamp.After(parent.Status.StartTimestamp.Time) {
			parent = candidate
		}
	}
	return parent
}

// previousCompletedDataUpload returns the most recently completed DataUpload
// of the same PVC, by the pvc-uid label, or nil when there is none or the
// lookup fails; the lookup only refines a diagnostic and never fails the
// completion.
func (r *DataUploadReconciler) previousCompletedDataUpload(ctx context.Context, du *velerov2alpha1api.DataUpload, log logrus.FieldLogger) *velerov2alpha1api.DataUpload {
	pvcUID := du.Labels[velerov1api.PVCUIDLabel]
	if pvcUID == "" {
		return nil
	}
	list := &velerov2alpha1api.DataUploadList{}
	if err := r.client.List(ctx, list, crclient.InNamespace(du.Namespace), crclient.MatchingLabels{velerov1api.PVCUIDLabel: pvcUID}); err != nil {
		log.WithError(err).Debug("Failed to list the previous DataUploads of the PVC for the fallback reason")
		return nil
	}
	var previous *velerov2alpha1api.DataUpload
	for i := range list.Items {
		candidate := &list.Items[i]
		if candidate.Name == du.Name || candidate.Status.Phase != velerov2alpha1api.DataUploadPhaseCompleted || candidate.Status.CompletionTimestamp == nil {
			continue
		}
		if du.Status.StartTimestamp != nil && candidate.Status.CompletionTimestamp.After(du.Status.StartTimestamp.Time) {
			continue
		}
		if previous == nil || candidate.Status.CompletionTimestamp.After(previous.Status.CompletionTimestamp.Time) {
			previous = candidate
		}
	}
	return previous
}
