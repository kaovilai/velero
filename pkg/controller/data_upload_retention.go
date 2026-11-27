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
)

// shouldRetainSnapshot reports whether the DataUpload opted into base snapshot
// retention: a CSI snapshot moved by the velero-block data mover, with the
// retention decision the backup's volume policy wrote into DataMoverConfig.
func shouldRetainSnapshot(du *velerov2alpha1api.DataUpload) bool {
	return du.Spec.SnapshotType == velerov2alpha1api.SnapshotTypeCSI &&
		datamover.IsVeleroBlockDataMover(du.Spec.DataMover) &&
		du.Spec.DataMoverConfig[velerov2alpha1api.RequireBaseSnapshotForDeltaDataMoverConfigKey] == "true"
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
//  4. Best effort: when this backup retained its snapshot, the deletion of
//     the parent's snapshot is issued, without waiting for the driver.
//
// Nothing after the data path fails the DataUpload: a step that does not
// succeed leaves the backup completed without retention, the content Delete,
// and the next backup of the volume reads in full. A record whose snapshot
// was not retained is harmless: deleting a gone snapshot succeeds. Nothing
// writes the DataUpload after the Completed update; the outcome is logged.
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

	// The parent is released only when this backup retained its own
	// snapshot, which becomes the next backup's base.
	if retained && result.Backup.ParentChangeID != "" && result.Backup.ParentChangeID != record.CSISnapshotHandle {
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

// releaseBase issues the deletion of the parent's snapshot: the handle the
// data path reported from the parent repository snapshot's change ID, deleted
// with the driver, deletion secret and managed-by label of this backup's own
// record, which the parent shares. Best effort and logged only: a snapshot
// whose deletion is not issued stays until the deletion of the backup that
// recorded it.
func (r *DataUploadReconciler) releaseBase(ctx context.Context, du *velerov2alpha1api.DataUpload, record *velerov2alpha1api.RetainedCSIStorageSnapshot, parentHandle string, log logrus.FieldLogger) {
	parent := &velerov2alpha1api.RetainedCSIStorageSnapshot{
		CSISnapshotHandle:   parentHandle,
		Driver:              record.Driver,
		DeletionSecretRef:   record.DeletionSecretRef,
		ManagedByLabelValue: record.ManagedByLabelValue,
	}
	if err := exposer.ReleaseRetainedSnapshot(ctx, r.client, parent, types.UID(du.Labels[velerov1api.BackupUIDLabel]), log); err != nil {
		log.WithError(err).Warnf("Failed to issue the deletion of the parent's snapshot %s of driver %s; the deletion of the backup that recorded it deletes it", parentHandle, record.Driver)
		return
	}
	log.Infof("Issued the deletion of the parent's snapshot %s of driver %s", parentHandle, record.Driver)
}

// warnNotRetained logs why a completed DataUpload's snapshot was not retained;
// the next backup of the volume reads in full.
func (r *DataUploadReconciler) warnNotRetained(du *velerov2alpha1api.DataUpload, reason string) {
	r.logger.WithField("dataupload", du.Name).Warnf("The snapshot is not retained, the next backup of the volume reads in full: %s", reason)
}
