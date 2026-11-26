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

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/datapath"
	"github.com/velero-io/velero/pkg/exposer"
	"github.com/velero-io/velero/pkg/util/csi"
	"github.com/velero-io/velero/pkg/util/datamover"
	"github.com/velero-io/velero/pkg/util/kube"
)

// shouldRetainSnapshot reports whether the DataUpload opted into base snapshot
// retention: a CSI snapshot moved by the velero-block data mover, with the
// retention decision the backup's volume policy wrote into DataMoverConfig.
func shouldRetainSnapshot(du *velerov2alpha1api.DataUpload) bool {
	return du.Spec.SnapshotType == velerov2alpha1api.SnapshotTypeCSI &&
		datamover.IsVeleroBlockDataMover(du.Spec.DataMover) &&
		du.Spec.DataMoverConfig[velerov2alpha1api.RetainSnapshotDataMoverConfigKey] == "true"
}

// completeWithRetention completes a DataUpload that opted into base snapshot
// retention. The order is:
//
//  1. One status update writes Completed together with the record of the
//     snapshot, read from the backup VolumeSnapshotContent, which still exists
//     because the cleanup has not run yet. The update declines when the
//     DataUpload is already in a final phase: a cancel, failure or timeout
//     path that won its final phase first leaves no record and no Retain. The
//     final phase write is the arbiter; nothing retention-relevant happens
//     before it.
//  2. Best effort: the backup content is switched to Retain, only when the
//     object the update returned carries the record. A response without it --
//     the field pruned by a DataUpload CRD that predates it -- skips the
//     switch.
//  3. The exposer cleanup, unchanged, and a best-effort deletion of the
//     content object, which a VolumeSnapshot deletion does not cascade to
//     under Retain.
//  4. Best effort: the base this backup's changed blocks were computed
//     against is released.
//
// Nothing after the data path fails the DataUpload: a step that does not
// succeed leaves the backup completed without retention, the content Delete,
// and the next backup of the volume reads in full. A record whose snapshot
// was not retained is harmless: deleting a gone snapshot succeeds.
func (r *DataUploadReconciler) completeWithRetention(ctx context.Context, du *velerov2alpha1api.DataUpload, result datapath.Result, log logrus.FieldLogger) {
	record := r.backupSnapshotRecord(ctx, du, log)

	var completed *velerov2alpha1api.DataUpload
	if err := UpdateDataUploadWithRetry(ctx, r.client, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, log, func(current *velerov2alpha1api.DataUpload) bool {
		completed = nil
		if isDataUploadInFinalState(current) {
			return false
		}
		r.setDataUploadCompleted(current, result)
		current.Status.RetainedCSIStorageSnapshot = record
		// The update writes into current the object the API server returned.
		completed = current
		return true
	}); err != nil {
		log.WithError(err).Error("error updating DataUpload status")
		completed = nil
	}

	retained := false
	switch {
	case completed == nil:
		log.Info("The DataUpload is already in a final phase, its snapshot is not retained")
	case record == nil:
		r.warnNotRetained(completed, "the backup VolumeSnapshotContent carries no snapshot handle")
	case completed.Status.RetainedCSIStorageSnapshot == nil:
		r.warnNotRetained(completed, "the record was dropped by the API server, the DataUpload CRD may predate the retainedCSIStorageSnapshot field")
	default:
		if err := csi.PatchVSCDeletionPolicy(ctx, r.csiSnapshotClient, du.Name, snapshotv1api.VolumeSnapshotContentRetain); err != nil {
			r.warnNotRetained(completed, err.Error())
		} else {
			retained = true
			log.Infof("Retained snapshot %s of driver %s", record.CSISnapshotHandle, record.Driver)
		}
	}

	r.cleanUpExposure(ctx, du, log)
	if retained {
		if err := r.csiSnapshotClient.VolumeSnapshotContents().Delete(ctx, du.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.WithError(err).Warnf("Failed to delete the retained backup VolumeSnapshotContent %s, the storage snapshot stays", du.Name)
		}
	}

	if completed == nil {
		return
	}
	log.Info("Data upload completed")
	r.metrics.RegisterDataUploadSuccess(r.nodeName)

	if retained && !result.Backup.FallbackFull && result.Backup.ParentChangeID != "" && result.Backup.ParentChangeID != record.CSISnapshotHandle {
		// The data movement is over: the node's data path slot is given back
		// before the release, which can wait up to the CSI operation timeout,
		// so the next DataUpload of the node does not wait on it. The
		// callback's deferred removal is then a no-op.
		r.dataPathMgr.RemoveAsyncBR(du.Name)
		r.releaseBase(ctx, completed, record, result.Backup.ParentChangeID, log)
	}
}

// backupSnapshotRecord reads the record of the snapshot from the backup
// VolumeSnapshotContent, which the exposer named after the DataUpload; nil
// when it cannot be read.
func (r *DataUploadReconciler) backupSnapshotRecord(ctx context.Context, du *velerov2alpha1api.DataUpload, log logrus.FieldLogger) *velerov2alpha1api.RetainedCSIStorageSnapshot {
	vsc, err := r.csiSnapshotClient.VolumeSnapshotContents().Get(ctx, du.Name, metav1.GetOptions{})
	if err != nil {
		log.WithError(err).Warnf("Failed to get the backup VolumeSnapshotContent %s, its snapshot is not retained", du.Name)
		return nil
	}
	return csi.GetRetainedSnapshotFromVSC(vsc)
}

// releaseBase releases the base snapshot this backup's changed blocks were
// computed against: the handle the data path reported from the parent
// repository snapshot's change ID, deleted with the driver, deletion secret
// and managed-by label of this backup's own record, which the base shares.
// Best effort: a failure emits RetainedSnapshotReleaseFailed and leaves the
// snapshot to the deletion of the backup that recorded it.
func (r *DataUploadReconciler) releaseBase(ctx context.Context, du *velerov2alpha1api.DataUpload, record *velerov2alpha1api.RetainedCSIStorageSnapshot, baseHandle string, log logrus.FieldLogger) {
	base := &velerov2alpha1api.RetainedCSIStorageSnapshot{
		CSISnapshotHandle:   baseHandle,
		Driver:              record.Driver,
		DeletionSecretRef:   record.DeletionSecretRef,
		ManagedByLabelValue: record.ManagedByLabelValue,
	}
	baseLog := log.WithField("baseSnapshot", baseHandle)
	if err := exposer.ReleaseRetainedSnapshot(ctx, r.client, base, types.UID(du.Labels[velerov1api.BackupUIDLabel]), csi.RetainedSnapshotDeletionTimeout, baseLog); err != nil {
		baseLog.WithError(err).Warn("Failed to release the base snapshot, the deletion of the backup that recorded it deletes it")
		r.events().Event(du, true, "RetainedSnapshotReleaseFailed",
			"the base snapshot %s was not released: %v; the deletion of the backup that recorded it deletes it", baseHandle, err)
		return
	}
	baseLog.Info("Released the base snapshot")
	r.events().Event(du, false, "RetainedSnapshotReleased", "the base snapshot %s was released", baseHandle)
}

// warnNotRetained records why a completed DataUpload's snapshot was not
// retained; the next backup of the volume reads in full.
func (r *DataUploadReconciler) warnNotRetained(du *velerov2alpha1api.DataUpload, reason string) {
	r.logger.WithField("dataupload", du.Name).Warnf("The snapshot is not retained, the next backup of the volume reads in full: %s", reason)
	r.events().Event(du, true, "RetainedSnapshotSkipped", "the snapshot is not retained, the next backup of the volume reads in full: %s", reason)
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

// fallbackReasonOf returns why the incremental fell back to a full read, as the
// uploader classified it from the driver's error; empty when it did not.
func fallbackReasonOf(result datapath.BackupResult) string {
	if !result.FallbackFull {
		return ""
	}
	if result.FallbackReason == "" {
		return "fallen back to a full read"
	}
	return result.FallbackReason
}
