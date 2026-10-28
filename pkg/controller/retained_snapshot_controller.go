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
	"time"

	"github.com/cockroachdb/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clocks "k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/constant"
	"github.com/velero-io/velero/pkg/retainedsnapshot"
	"github.com/velero-io/velero/pkg/util/csi"
)

// RetainedSnapshotGCGracePeriod and RetainedSnapshotDeletionTimeout are set by
// the server flags --retained-snapshot-gc-grace-period and
// --retained-snapshot-deletion-timeout.
var (
	RetainedSnapshotGCGracePeriod   = time.Hour
	RetainedSnapshotDeletionTimeout = 5 * time.Minute
	// RetainedSnapshotResyncInterval is how often every chain is resynced for GC.
	RetainedSnapshotResyncInterval = 5 * time.Minute
)

const (
	// RetainedSnapshotConfigMap optionally overrides the settings of the
	// retained snapshot controller and backup deletion (prototype only, so
	// tests don't need a server restart): keys gcGracePeriod, gcInterval
	// (resync) and deletionWait (deletion timeout), Go durations.
	RetainedSnapshotConfigMap = "retained-snapshot-config"

	// goneSinceAnnotation records on a record when the controller first saw its
	// source PVC or Backup gone; the grace period starts then.
	goneSinceAnnotation = "velero.io/retained-snapshot-gone-since"
	// goneWhatAnnotation records what is gone: volume or backup.
	goneWhatAnnotation = "velero.io/retained-snapshot-gone"

	pauseRequeue   = 15 * time.Second
	pendingRequeue = 30 * time.Second

	// deletionOwner names this controller on the records it moves to deleting.
	deletionOwner = "retained-snapshot-controller"
)

// retainedSnapshotReconciler reconciles the retained snapshot records of a
// chain (BSL, source namespace, source PVC); a request's Name is the chain
// hash. For the chain it:
//   - pauses while any DataUpload of the chain, or an in-place incremental
//     DataDownload of a backup of the chain, is not in a final phase;
//   - rotates: deletes every live record whose repository snapshot started
//     earlier than the latest completed block DataUpload of the chain, and keeps
//     the record as a tombstone;
//   - deletes the snapshots of records whose DataUpload ended without completing;
//   - deletes the snapshots of live records whose source PVC is gone or has
//     another UID, after the grace period;
//   - deletes the snapshots of live records, and removes tombstones, whose
//     Backup is gone, after the grace period;
//   - finishes records left deleting.
//
// Records are read from the API server and changed with updates conditioned on
// their resourceVersion. Records of another cluster are left alone. A periodic
// sweep enqueues every chain with records and deletes leftover temp
// VolumeSnapshotContents of records that are not live.
type retainedSnapshotReconciler struct {
	client    client.Client
	apiReader client.Reader
	namespace string
	logger    logrus.FieldLogger
	clock     clocks.WithTickerAndDelayedExecution
	clusterID string
	sweep     chan event.GenericEvent
}

func NewRetainedSnapshotReconciler(c client.Client, namespace string, logger logrus.FieldLogger) *retainedSnapshotReconciler {
	return &retainedSnapshotReconciler{
		client:    c,
		namespace: namespace,
		logger:    logger.WithField("controller", constant.ControllerRetainedSnapshot),
		clock:     clocks.RealClock{},
		sweep:     make(chan event.GenericEvent, 1024),
	}
}

type retainedSnapshotSettings struct {
	gcGracePeriod time.Duration
	gcInterval    time.Duration
	// deletionWait is the deletion timeout: how long one attempt waits for a
	// temp VolumeSnapshotContent to disappear. A pending deletion keeps the
	// record deleting and is retried on the same object.
	deletionWait time.Duration
}

// settings returns the server flag values, overridden by the optional
// prototype ConfigMap.
func (r *retainedSnapshotReconciler) settings(ctx context.Context) retainedSnapshotSettings {
	return readRetainedSnapshotSettings(ctx, r.reader(), r.namespace, r.logger)
}

func readRetainedSnapshotSettings(ctx context.Context, reader client.Reader, namespace string, log logrus.FieldLogger) retainedSnapshotSettings {
	s := retainedSnapshotSettings{gcGracePeriod: RetainedSnapshotGCGracePeriod, gcInterval: RetainedSnapshotResyncInterval, deletionWait: RetainedSnapshotDeletionTimeout}
	cm := &corev1api.ConfigMap{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: RetainedSnapshotConfigMap}, cm); err != nil {
		return s
	}
	parse := func(key string, into *time.Duration) {
		if v, ok := cm.Data[key]; ok {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				*into = d
			} else {
				log.Warnf("Ignoring invalid %s %q in configmap %s", key, v, RetainedSnapshotConfigMap)
			}
		}
	}
	parse("gcGracePeriod", &s.gcGracePeriod)
	parse("gcInterval", &s.gcInterval)
	parse("deletionWait", &s.deletionWait)
	return s
}

func (r *retainedSnapshotReconciler) SetupWithManager(mgr manager.Manager) error {
	r.apiReader = mgr.GetAPIReader()

	chainOf := func(_ context.Context, obj client.Object) []reconcile.Request {
		chain := obj.GetLabels()[retainedsnapshot.ChainLabel]
		if chain == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.namespace, Name: chain}}}
	}
	inNamespaceWithChain := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.namespace && obj.GetLabels()[retainedsnapshot.ChainLabel] != ""
	})
	recordPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.namespace && obj.GetLabels()[retainedsnapshot.RecordLabel] == "true" &&
			obj.GetLabels()[retainedsnapshot.ChainLabel] != ""
	})

	if err := mgr.Add(manager.RunnableFunc(r.runSweep)); err != nil {
		return err
	}

	return ctrl.ControllerManagedBy(mgr).
		Named(constant.ControllerRetainedSnapshot).
		Watches(&corev1api.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(chainOf), ctrl.WithPredicates(recordPredicate)).
		Watches(&velerov2alpha1api.DataUpload{}, handler.EnqueueRequestsFromMapFunc(chainOf), ctrl.WithPredicates(inNamespaceWithChain)).
		Watches(&velerov2alpha1api.DataDownload{}, handler.EnqueueRequestsFromMapFunc(chainOf), ctrl.WithPredicates(inNamespaceWithChain)).
		WatchesRawSource(source.Channel(r.sweep, &handler.EnqueueRequestForObject{})).
		Complete(r)
}

// runSweep periodically enqueues every chain that has records, so that the GC
// by PVC and Backup runs without events, and deletes leftover temp
// VolumeSnapshotContents.
func (r *retainedSnapshotReconciler) runSweep(ctx context.Context) error {
	for {
		s := r.settings(ctx)
		r.sweepOnce(ctx, s)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.gcInterval):
		}
	}
}

func (r *retainedSnapshotReconciler) sweepOnce(ctx context.Context, s retainedSnapshotSettings) {
	cms := &corev1api.ConfigMapList{}
	if err := r.reader().List(ctx, cms, client.InNamespace(r.namespace), client.MatchingLabels{retainedsnapshot.RecordLabel: "true"}); err != nil {
		r.logger.WithError(err).Warn("Failed to list the retained snapshot records for the GC sweep")
		return
	}
	chains := map[string]struct{}{}
	records := map[string]*retainedsnapshot.Record{}
	for i := range cms.Items {
		cm := &cms.Items[i]
		if c := cm.Labels[retainedsnapshot.ChainLabel]; c != "" {
			chains[c] = struct{}{}
		}
		if rec, err := retainedsnapshot.FromConfigMap(cm); err == nil {
			records[cm.Name] = rec
		}
	}
	for c := range chains {
		obj := &corev1api.ConfigMap{}
		obj.SetNamespace(r.namespace)
		obj.SetName(c)
		select {
		case r.sweep <- event.GenericEvent{Object: obj}:
		default:
		}
	}

	// Leftover temp VSCs of records that are not live, e.g. from a deletion
	// interrupted by a restart after the record was removed.
	vscs := &snapshotv1api.VolumeSnapshotContentList{}
	if err := r.reader().List(ctx, vscs, client.HasLabels{retainedsnapshot.TempVSCRecordLabel}); err != nil {
		r.logger.WithError(err).Warn("Failed to list temp VolumeSnapshotContents for the GC sweep")
		return
	}
	for i := range vscs.Items {
		vsc := &vscs.Items[i]
		name := vsc.Labels[retainedsnapshot.TempVSCRecordLabel]
		if rec, ok := records[name]; ok && rec.State != retainedsnapshot.StateDeleted {
			// The record's own deletion owns it.
			continue
		}
		if vsc.CreationTimestamp.Add(s.deletionWait).After(r.clock.Now()) {
			continue
		}
		log := r.logger.WithFields(logrus.Fields{"tempVSC": vsc.Name, "record": name})
		log.Info("Deleting leftover temp VolumeSnapshotContent of a retained snapshot record that is not live")
		if err := csi.DeleteSnapshotByTempVSCWait(ctx, r.deletionClient(), vsc.DeepCopy(), s.deletionWait, log); err != nil {
			log.WithError(err).Warn("Failed to delete leftover temp VolumeSnapshotContent, retrying at the next sweep")
		}
	}
}

func (r *retainedSnapshotReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	chain := req.Name
	log := r.logger.WithField("chain", chain)
	s := r.settings(ctx)

	cms := &corev1api.ConfigMapList{}
	if err := r.reader().List(ctx, cms, client.InNamespace(r.namespace), client.MatchingLabels{
		retainedsnapshot.RecordLabel: "true",
		retainedsnapshot.ChainLabel:  chain,
	}); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "error listing retained snapshot records")
	}
	if len(cms.Items) == 0 {
		return reconcile.Result{}, nil
	}

	cs, err := r.chainState(ctx, chain)
	if err != nil {
		return reconcile.Result{}, err
	}
	byName, newest := cs.byName, cs.newest

	if r.clusterID == "" {
		id, err := retainedsnapshot.GetClusterIDFromReader(ctx, r.reader())
		if err != nil {
			return reconcile.Result{}, err
		}
		r.clusterID = id
	}

	running := cs.running
	paused := len(running) > 0

	var errs []error
	requeueAfter := time.Duration(0)
	requeue := func(d time.Duration) {
		if requeueAfter == 0 || d < requeueAfter {
			requeueAfter = d
		}
	}

	for i := range cms.Items {
		name := cms.Items[i].Name
		// Act only on a fresh read of the record.
		cm, record, err := retainedsnapshot.Get(ctx, r.reader(), r.namespace, name)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				log.WithError(err).Warnf("Skipping retained snapshot record %s", name)
			}
			continue
		}
		rlog := log.WithFields(logrus.Fields{"record": name, "snapshotHandle": record.SnapshotHandle, "dataupload": record.DataUpload})
		if retainedsnapshot.IsForeign(record.ClusterID, r.clusterID) {
			continue
		}

		action, opts, wait := r.decide(ctx, cm, record, byName, newest, cs, paused, s, rlog)
		if wait > 0 {
			requeue(wait)
		}
		if action == "" {
			continue
		}
		if opts.needsUnpaused {
			// Re-check the pause from the API server right before the
			// conditioned update to deleting, so that a DataUpload or
			// DataDownload created since the list above isn't missed.
			fresh, err := r.chainState(ctx, chain)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if len(fresh.running) > 0 {
				rlog.Infof("Not deleting retained snapshot %s, the chain was paused meanwhile by %v", record.SnapshotHandle, fresh.running)
				requeue(pauseRequeue)
				continue
			}
		}
		rlog.Info(action)
		opts.Wait = s.deletionWait
		opts.Timeout = s.deletionWait
		opts.Now = r.clock.Now
		opts.Owner = deletionOwner
		if err := retainedsnapshot.DeleteSnapshot(ctx, r.client, r.deletionClient(), cm, opts.DeleteOptions, rlog); err != nil {
			switch {
			case errors.Is(err, retainedsnapshot.ErrGone):
				rlog.Info("Retained snapshot record was removed by another writer")
			case errors.Is(err, retainedsnapshot.ErrConflict):
				rlog.Info("Retained snapshot record changed by another writer, re-reading it")
				requeue(time.Second)
			case errors.Is(err, retainedsnapshot.ErrOwnedByOther):
				requeue(pendingRequeue)
			case errors.Is(err, csi.ErrTempVSCDeletionInterrupted):
				rlog.Infof("Deletion of retained snapshot %s was interrupted, the record stays %s", record.SnapshotHandle, retainedsnapshot.StateDeleting)
				requeue(pendingRequeue)
			case errors.Is(err, csi.ErrTempVSCDeletionPending):
				rlog.Infof("Deletion of retained snapshot %s is pending after %v, the record stays %s, retrying in %v", record.SnapshotHandle, s.deletionWait, retainedsnapshot.StateDeleting, pendingRequeue)
				requeue(pendingRequeue)
			default:
				errs = append(errs, errors.Wrapf(err, "record %s", name))
			}
		}
	}

	if len(errs) > 0 {
		return reconcile.Result{}, errors.Errorf("failed to process %d retained snapshot record(s) of chain %s, first error: %v", len(errs), chain, errs[0])
	}
	if paused {
		log.Debugf("Chain paused by %v", running)
		requeue(pauseRequeue)
	}
	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

// chainStatus is what the controller knows about the DataUploads and
// DataDownloads of a chain, listed from the API server.
type chainStatus struct {
	byName map[string]*velerov2alpha1api.DataUpload
	// newest is the completed block DataUpload with the latest repository
	// snapshot start time.
	newest *velerov2alpha1api.DataUpload
	// running names the DataUploads and in-place DataDownloads of the chain not
	// in a final phase; the chain is paused while there is any.
	running []string
	// restoresWithDataDownload are the restores that have a DataDownload of the
	// chain; their hold is replaced by the DataDownload pause.
	restoresWithDataDownload map[string]bool
}

func (r *retainedSnapshotReconciler) chainState(ctx context.Context, chain string) (*chainStatus, error) {
	cs := &chainStatus{byName: map[string]*velerov2alpha1api.DataUpload{}, restoresWithDataDownload: map[string]bool{}}
	dus := &velerov2alpha1api.DataUploadList{}
	if err := r.reader().List(ctx, dus, client.InNamespace(r.namespace), client.MatchingLabels{retainedsnapshot.ChainLabel: chain}); err != nil {
		return nil, errors.Wrap(err, "error listing DataUploads")
	}
	for i := range dus.Items {
		du := &dus.Items[i]
		cs.byName[du.Name] = du
		if !isDataUploadInFinalState(du) {
			cs.running = append(cs.running, du.Name)
			continue
		}
		if du.Status.Phase != velerov2alpha1api.DataUploadPhaseCompleted || du.Status.SnapshotStartTime == nil {
			continue
		}
		if cs.newest == nil || du.Status.SnapshotStartTime.After(cs.newest.Status.SnapshotStartTime.Time) {
			cs.newest = du
		}
	}
	dds := &velerov2alpha1api.DataDownloadList{}
	if err := r.reader().List(ctx, dds, client.InNamespace(r.namespace), client.MatchingLabels{retainedsnapshot.ChainLabel: chain}); err != nil {
		return nil, errors.Wrap(err, "error listing DataDownloads")
	}
	for i := range dds.Items {
		dd := &dds.Items[i]
		if restore := dd.Labels[velerov1api.RestoreNameLabel]; restore != "" {
			cs.restoresWithDataDownload[restore] = true
		}
		if !isDataDownloadInFinalState(dd) {
			cs.running = append(cs.running, "DataDownload "+dd.Name)
		}
	}
	return cs, nil
}

// recordAction is a deletion decided for a record.
type recordAction struct {
	retainedsnapshot.DeleteOptions
	// needsUnpaused means the deletion can remove the base of a running
	// DataUpload or DataDownload (rotation, volume gone), so the pause is checked
	// again right before it.
	needsUnpaused bool
}

// decide returns what to do with a record: a log line and the deletion options
// if its snapshot is to be deleted, and a requeue delay if it has to be looked
// at again later.
func (r *retainedSnapshotReconciler) decide(ctx context.Context, cm *corev1api.ConfigMap, record *retainedsnapshot.Record,
	byName map[string]*velerov2alpha1api.DataUpload, newest *velerov2alpha1api.DataUpload, cs *chainStatus, paused bool,
	s retainedSnapshotSettings, log logrus.FieldLogger) (string, recordAction, time.Duration) {
	none := recordAction{}
	// The Backup gone (e.g. removed by backup sync, or deleted without a
	// DeleteBackupRequest): delete live snapshots and remove tombstones after
	// the grace period, also while the chain is paused.
	if gone, wait := r.backupGone(ctx, cm, record, s, log); gone {
		if record.State == retainedsnapshot.StateDeleted {
			return "Removing tombstone, its Backup " + record.Backup + " is gone", recordAction{DeleteOptions: retainedsnapshot.DeleteOptions{Remove: true}}, 0
		}
		return "Deleting retained snapshot, its Backup " + record.Backup + " is gone", recordAction{DeleteOptions: retainedsnapshot.DeleteOptions{Remove: true, Reason: retainedsnapshot.ReasonBackupGone}}, 0
	} else if wait > 0 {
		return "", none, wait
	}

	switch record.State {
	case retainedsnapshot.StateDeleted:
		return "", none, 0
	case retainedsnapshot.StateDeleting:
		// A deletion another writer (backup deletion) owns is left to it while
		// its wait can still be running; after that it was interrupted or failed.
		if record.DeletingBy != deletionOwner && !record.DeletingSince.IsZero() {
			if left := record.LeaseEnd(s.deletionWait).Sub(r.clock.Now()); left > 0 {
				return "", none, left
			}
		}
		// Interrupted or pending deletion: finish it with the reason it started with.
		return "Finishing the deletion of retained snapshot " + record.SnapshotHandle + " (owner " + record.DeletingBy + ")", recordAction{DeleteOptions: retainedsnapshot.DeleteOptions{
			Reason: record.Reason, RotatedByDataUpload: record.RotatedByDataUpload, RotatedByBackup: record.RotatedByBackup,
		}}, 0
	}

	du := byName[record.DataUpload]
	if du != nil && (du.Status.Phase == velerov2alpha1api.DataUploadPhaseFailed || du.Status.Phase == velerov2alpha1api.DataUploadPhaseCanceled) {
		return "DataUpload ended " + string(du.Status.Phase) + ", deleting its retained snapshot", recordAction{DeleteOptions: retainedsnapshot.DeleteOptions{Reason: retainedsnapshot.ReasonDataUploadFailed}}, 0
	}
	if du != nil && du.Status.Phase == velerov2alpha1api.DataUploadPhaseCompleted &&
		(du.Status.RetainedSnapshot == nil || du.Status.RetainedSnapshot.SnapshotHandle != record.SnapshotHandle) {
		// Step 2 failed or retention was abandoned, so the cleanup deleted the
		// snapshot; deleting an absent handle succeeds.
		return "DataUpload completed without retaining its snapshot, dropping the record", recordAction{DeleteOptions: retainedsnapshot.DeleteOptions{Reason: retainedsnapshot.ReasonDataUploadFailed}}, 0
	}

	if paused {
		return "", none, 0
	}

	// An in-place restore holds the record between its decision and its
	// DataDownload; once the DataDownload exists, its pause applies instead.
	now := r.clock.Now()
	if record.IsHeld(now) && !cs.restoresWithDataDownload[record.InUseBy] {
		log.Debugf("Retained snapshot record is held by restore %s until %s", record.InUseBy, record.InUseUntil.UTC().Format(time.RFC3339))
		return "", none, record.InUseUntil.Sub(now) + time.Second
	}

	if newest != nil && !record.RepoSnapshotStartTime.IsZero() && newest.Status.SnapshotStartTime.Time.After(record.RepoSnapshotStartTime) {
		return "Rotating out retained snapshot, DataUpload " + newest.Name + " of backup " + newest.Labels[velerov1api.BackupNameLabel] +
				" has a later repository snapshot (" + newest.Status.SnapshotStartTime.UTC().Format(time.RFC3339Nano) + " > " +
				record.RepoSnapshotStartTime.UTC().Format(time.RFC3339Nano) + ")",
			recordAction{DeleteOptions: retainedsnapshot.DeleteOptions{
				Reason:              retainedsnapshot.ReasonRotated,
				RotatedByDataUpload: newest.Name,
				RotatedByBackup:     newest.Labels[velerov1api.BackupNameLabel],
			}, needsUnpaused: true}, 0
	}
	// Equal start times keep both snapshots; the next completion resolves it.

	if gone, why, wait := r.volumeGone(ctx, cm, record, s, log); gone {
		return "Deleting retained snapshot, its source volume is gone: " + why,
			recordAction{DeleteOptions: retainedsnapshot.DeleteOptions{Reason: retainedsnapshot.ReasonVolumeGone}, needsUnpaused: true}, 0
	} else if wait > 0 {
		return "", none, wait
	}
	return "", none, 0
}

// backupGone tells whether the record's Backup has been gone for the grace
// period. It marks the record with the time it first saw the Backup gone and
// returns the time left to wait.
func (r *retainedSnapshotReconciler) backupGone(ctx context.Context, cm *corev1api.ConfigMap, record *retainedsnapshot.Record, s retainedSnapshotSettings, log logrus.FieldLogger) (bool, time.Duration) {
	if record.Backup == "" {
		return false, 0
	}
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: record.Backup}, &velerov1api.Backup{})
	if err == nil || !apierrors.IsNotFound(err) {
		r.clearGone(ctx, cm, "backup", log)
		return false, 0
	}
	return r.goneFor(ctx, cm, "backup", s, log)
}

// volumeGone tells whether the record's source volume has been gone for the
// grace period: no PVC with the source name, or a PVC bound to a
// PersistentVolume whose CSI volume handle is not the record's volume ID. The
// volume, not the PVC UID, is compared, as an in-place restore recreates the
// PVC over the same PersistentVolume; it is also what the parent lookup checks
// (cbt-volume-id). A PVC not bound yet doesn't count as gone: an in-place
// restore rebinds the volume only when its DataDownload finishes.
func (r *retainedSnapshotReconciler) volumeGone(ctx context.Context, cm *corev1api.ConfigMap, record *retainedsnapshot.Record, s retainedSnapshotSettings, log logrus.FieldLogger) (bool, string, time.Duration) {
	if record.SourcePVC == "" {
		return false, "", 0
	}
	pvc := &corev1api.PersistentVolumeClaim{}
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: record.SourceNamespace, Name: record.SourcePVC}, pvc)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, "", 0
	}
	why := ""
	switch {
	case err != nil:
		why = "PVC " + record.SourceNamespace + "/" + record.SourcePVC + " doesn't exist"
	case pvc.Spec.VolumeName == "" || pvc.Status.Phase != corev1api.ClaimBound:
		// Not bound yet, e.g. recreated by an in-place restore.
		r.clearGone(ctx, cm, "volume", log)
		return false, "", 0
	case record.SourceVolumeID == "":
		// A record without the volume ID only counts a missing PVC.
		r.clearGone(ctx, cm, "volume", log)
		return false, "", 0
	default:
		pv := &corev1api.PersistentVolume{}
		if err := r.reader().Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
			if !apierrors.IsNotFound(err) {
				return false, "", 0
			}
			why = "PVC " + record.SourceNamespace + "/" + record.SourcePVC + " is bound to PersistentVolume " + pvc.Spec.VolumeName + ", which doesn't exist"
		} else if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == record.SourceVolumeID {
			r.clearGone(ctx, cm, "volume", log)
			return false, "", 0
		} else {
			why = "PVC " + record.SourceNamespace + "/" + record.SourcePVC + " is bound to volume " + pv.Spec.CSI.VolumeHandle + ", not " + record.SourceVolumeID
		}
	}
	gone, wait := r.goneFor(ctx, cm, "volume", s, log)
	if !gone && wait == s.gcGracePeriod {
		log.Infof("Source volume of retained snapshot record %s: %s", cm.Name, why)
	}
	return gone, why, wait
}

func (r *retainedSnapshotReconciler) goneFor(ctx context.Context, cm *corev1api.ConfigMap, what string, s retainedSnapshotSettings, log logrus.FieldLogger) (bool, time.Duration) {
	now := r.clock.Now()
	if cm.Annotations[goneWhatAnnotation] == what {
		if since, err := time.Parse(time.RFC3339, cm.Annotations[goneSinceAnnotation]); err == nil {
			if left := since.Add(s.gcGracePeriod).Sub(now); left > 0 {
				return false, left
			}
			return true, 0
		}
	}
	updated := cm.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[goneWhatAnnotation] = what
	updated.Annotations[goneSinceAnnotation] = now.UTC().Format(time.RFC3339)
	if err := r.client.Update(ctx, updated); err != nil {
		log.WithError(err).Warnf("Failed to mark the %s of retained snapshot record %s gone", what, cm.Name)
		return false, time.Second
	}
	*cm = *updated
	log.Infof("The %s of retained snapshot record %s is gone, deleting its snapshot after the grace period %v unless it comes back", what, cm.Name, s.gcGracePeriod)
	return false, s.gcGracePeriod
}

func (r *retainedSnapshotReconciler) clearGone(ctx context.Context, cm *corev1api.ConfigMap, what string, log logrus.FieldLogger) {
	if cm.Annotations[goneWhatAnnotation] != what {
		return
	}
	updated := cm.DeepCopy()
	delete(updated.Annotations, goneWhatAnnotation)
	delete(updated.Annotations, goneSinceAnnotation)
	if err := r.client.Update(ctx, updated); err != nil {
		log.WithError(err).Warnf("Failed to clear the gone mark of retained snapshot record %s", cm.Name)
		return
	}
	*cm = *updated
	log.Infof("The %s of retained snapshot record %s is back", what, cm.Name)
}

func (r *retainedSnapshotReconciler) reader() client.Reader {
	if r.apiReader == nil {
		return r.client
	}
	return r.apiReader
}

// deletionClient reads the temp VolumeSnapshotContents and their events from
// the API server, which the manager's cache doesn't cover.
func (r *retainedSnapshotReconciler) deletionClient() client.Client {
	if r.apiReader == nil {
		return r.client
	}
	return &apiReadClient{Client: r.client, reader: r.apiReader}
}
