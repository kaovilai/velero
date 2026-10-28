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

const (
	// RetainedSnapshotConfigMap optionally overrides the GC settings of the
	// retained snapshot controller (prototype): keys gcGracePeriod, gcInterval
	// and deletionWait, Go durations.
	RetainedSnapshotConfigMap = "retained-snapshot-config"

	// goneSinceAnnotation records on a record when the controller first saw its
	// source PVC or Backup gone; the grace period starts then.
	goneSinceAnnotation = "velero.io/retained-snapshot-gone-since"
	// goneWhatAnnotation records what is gone: volume or backup.
	goneWhatAnnotation = "velero.io/retained-snapshot-gone"

	defaultGCGracePeriod = 10 * time.Minute
	defaultGCInterval    = 2 * time.Minute
	// defaultDeletionWait is how long one reconcile waits for a temp
	// VolumeSnapshotContent to disappear. A pending deletion keeps the record
	// deleting and is requeued; the next attempt waits on the same object, so
	// the total wait is unbounded.
	defaultDeletionWait = time.Minute
	pauseRequeue        = 15 * time.Second
	pendingRequeue      = 30 * time.Second
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
	deletionWait  time.Duration
}

// settings reads the optional override ConfigMap.
func (r *retainedSnapshotReconciler) settings(ctx context.Context) retainedSnapshotSettings {
	s := retainedSnapshotSettings{gcGracePeriod: defaultGCGracePeriod, gcInterval: defaultGCInterval, deletionWait: defaultDeletionWait}
	cm := &corev1api.ConfigMap{}
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: RetainedSnapshotConfigMap}, cm); err != nil {
		return s
	}
	parse := func(key string, into *time.Duration) {
		if v, ok := cm.Data[key]; ok {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				*into = d
			} else {
				r.logger.Warnf("Ignoring invalid %s %q in configmap %s", key, v, RetainedSnapshotConfigMap)
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

	dus := &velerov2alpha1api.DataUploadList{}
	if err := r.client.List(ctx, dus, client.InNamespace(r.namespace), client.MatchingLabels{retainedsnapshot.ChainLabel: chain}); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "error listing DataUploads")
	}
	byName := map[string]*velerov2alpha1api.DataUpload{}
	var newest *velerov2alpha1api.DataUpload
	var running []string
	for i := range dus.Items {
		du := &dus.Items[i]
		byName[du.Name] = du
		if !isDataUploadInFinalState(du) {
			running = append(running, du.Name)
			continue
		}
		if du.Status.Phase != velerov2alpha1api.DataUploadPhaseCompleted || du.Status.SnapshotStartTime == nil {
			continue
		}
		if newest == nil || du.Status.SnapshotStartTime.After(newest.Status.SnapshotStartTime.Time) {
			newest = du
		}
	}

	if r.clusterID == "" {
		id, err := retainedsnapshot.GetClusterIDFromReader(ctx, r.reader())
		if err != nil {
			return reconcile.Result{}, err
		}
		r.clusterID = id
	}

	// Pause while the chain has a running DataUpload or in-place incremental
	// DataDownload: a DataUpload that started earlier may still be reading
	// changed blocks against a base, and an in-place restore may be between its
	// check and its GetMetadataDelta call.
	if len(running) == 0 {
		dds := &velerov2alpha1api.DataDownloadList{}
		if err := r.client.List(ctx, dds, client.InNamespace(r.namespace), client.MatchingLabels{retainedsnapshot.ChainLabel: chain}); err != nil {
			return reconcile.Result{}, errors.Wrap(err, "error listing DataDownloads")
		}
		for i := range dds.Items {
			if !isDataDownloadInFinalState(&dds.Items[i]) {
				running = append(running, "DataDownload "+dds.Items[i].Name)
			}
		}
	}
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

		action, opts, wait := r.decide(ctx, cm, record, byName, newest, paused, s, rlog)
		if wait > 0 {
			requeue(wait)
		}
		if action == "" {
			continue
		}
		rlog.Info(action)
		opts.Wait = s.deletionWait
		opts.Now = r.clock.Now
		if err := retainedsnapshot.DeleteSnapshot(ctx, r.client, r.deletionClient(), cm, opts, rlog); err != nil {
			switch {
			case errors.Is(err, retainedsnapshot.ErrConflict):
				rlog.Info("Retained snapshot record changed by another writer, re-reading it")
				requeue(time.Second)
			case errors.Is(err, csi.ErrTempVSCDeletionPending):
				rlog.Infof("Deletion of retained snapshot %s is pending, the record stays %s, retrying in %v", record.SnapshotHandle, retainedsnapshot.StateDeleting, pendingRequeue)
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

// decide returns what to do with a record: a log line and the deletion options
// if its snapshot is to be deleted, and a requeue delay if it has to be looked
// at again later.
func (r *retainedSnapshotReconciler) decide(ctx context.Context, cm *corev1api.ConfigMap, record *retainedsnapshot.Record,
	byName map[string]*velerov2alpha1api.DataUpload, newest *velerov2alpha1api.DataUpload, paused bool,
	s retainedSnapshotSettings, log logrus.FieldLogger) (string, retainedsnapshot.DeleteOptions, time.Duration) {
	// The Backup gone (e.g. removed by backup sync, or deleted without a
	// DeleteBackupRequest): delete live snapshots and remove tombstones after
	// the grace period, also while the chain is paused.
	if gone, wait := r.backupGone(ctx, cm, record, s, log); gone {
		if record.State == retainedsnapshot.StateDeleted {
			return "Removing tombstone, its Backup " + record.Backup + " is gone", retainedsnapshot.DeleteOptions{Remove: true}, 0
		}
		return "Deleting retained snapshot, its Backup " + record.Backup + " is gone", retainedsnapshot.DeleteOptions{Remove: true, Reason: retainedsnapshot.ReasonBackupGone}, 0
	} else if wait > 0 {
		return "", retainedsnapshot.DeleteOptions{}, wait
	}

	switch record.State {
	case retainedsnapshot.StateDeleted:
		return "", retainedsnapshot.DeleteOptions{}, 0
	case retainedsnapshot.StateDeleting:
		// Interrupted or pending deletion: finish it with the reason it started with.
		return "Finishing the deletion of retained snapshot " + record.SnapshotHandle, retainedsnapshot.DeleteOptions{
			Reason: record.Reason, RotatedByDataUpload: record.RotatedByDataUpload, RotatedByBackup: record.RotatedByBackup,
		}, 0
	}

	du := byName[record.DataUpload]
	if du != nil && (du.Status.Phase == velerov2alpha1api.DataUploadPhaseFailed || du.Status.Phase == velerov2alpha1api.DataUploadPhaseCanceled) {
		return "DataUpload ended " + string(du.Status.Phase) + ", deleting its retained snapshot", retainedsnapshot.DeleteOptions{Reason: retainedsnapshot.ReasonDataUploadFailed}, 0
	}
	if du != nil && du.Status.Phase == velerov2alpha1api.DataUploadPhaseCompleted &&
		(du.Status.RetainedSnapshot == nil || du.Status.RetainedSnapshot.SnapshotHandle != record.SnapshotHandle) {
		// The record was written but the commit point never passed, so the
		// cleanup deleted the snapshot; deleting an absent handle succeeds.
		return "DataUpload completed without retaining its snapshot, dropping the record", retainedsnapshot.DeleteOptions{Reason: retainedsnapshot.ReasonDataUploadFailed}, 0
	}

	if paused {
		return "", retainedsnapshot.DeleteOptions{}, 0
	}

	if newest != nil && !record.RepoSnapshotStartTime.IsZero() && newest.Status.SnapshotStartTime.Time.After(record.RepoSnapshotStartTime) {
		return "Rotating out retained snapshot, DataUpload " + newest.Name + " of backup " + newest.Labels[velerov1api.BackupNameLabel] +
				" has a later repository snapshot (" + newest.Status.SnapshotStartTime.UTC().Format(time.RFC3339Nano) + " > " +
				record.RepoSnapshotStartTime.UTC().Format(time.RFC3339Nano) + ")",
			retainedsnapshot.DeleteOptions{
				Reason:              retainedsnapshot.ReasonRotated,
				RotatedByDataUpload: newest.Name,
				RotatedByBackup:     newest.Labels[velerov1api.BackupNameLabel],
			}, 0
	}
	// Equal start times keep both snapshots; the next completion resolves it.

	if gone, wait := r.volumeGone(ctx, cm, record, s, log); gone {
		return "Deleting retained snapshot, its source PVC " + record.SourceNamespace + "/" + record.SourcePVC + " is gone or was recreated",
			retainedsnapshot.DeleteOptions{Reason: retainedsnapshot.ReasonVolumeGone}, 0
	} else if wait > 0 {
		return "", retainedsnapshot.DeleteOptions{}, wait
	}
	return "", retainedsnapshot.DeleteOptions{}, 0
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

// volumeGone tells whether the record's source PVC has been gone, or replaced
// by one with another UID, for the grace period.
func (r *retainedSnapshotReconciler) volumeGone(ctx context.Context, cm *corev1api.ConfigMap, record *retainedsnapshot.Record, s retainedSnapshotSettings, log logrus.FieldLogger) (bool, time.Duration) {
	if record.SourcePVC == "" || record.SourcePVCUID == "" {
		return false, 0
	}
	pvc := &corev1api.PersistentVolumeClaim{}
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: record.SourceNamespace, Name: record.SourcePVC}, pvc)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, 0
	}
	if err == nil && string(pvc.UID) == record.SourcePVCUID {
		r.clearGone(ctx, cm, "volume", log)
		return false, 0
	}
	return r.goneFor(ctx, cm, "volume", s, log)
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
