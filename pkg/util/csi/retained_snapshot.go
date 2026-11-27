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

package csi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/cockroachdb/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapshotter "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/typed/volumesnapshot/v1"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/util/kube"
)

const (
	// VSCDeletionSecretNameAnnotation and VSCDeletionSecretNamespaceAnnotation are
	// set by the CSI snapshot controller on a VolumeSnapshotContent and name the
	// secret the CSI driver requires to delete the snapshot.
	VSCDeletionSecretNameAnnotation      = "snapshot.storage.kubernetes.io/deletion-secret-name"      // #nosec G101
	VSCDeletionSecretNamespaceAnnotation = "snapshot.storage.kubernetes.io/deletion-secret-namespace" // #nosec G101

	// VSCBoundProtectionFinalizer is the CSI snapshot finalizer that keeps a
	// VolumeSnapshotContent alive until the CSI snapshotter has handled its
	// deletion. A statically provisioned content without it can be dropped with
	// no DeleteSnapshot call at all, so the temporary content rebuilt to delete a
	// retained snapshot must carry it.
	VSCBoundProtectionFinalizer = "snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"
)

// GetRetainedSnapshotFromVSC builds the retained snapshot record of a backup
// VolumeSnapshotContent. It returns nil when the content has no snapshot
// handle. The exposer creates the backup content itself, synchronously, with
// the handle already in spec.source.snapshotHandle; status.snapshotHandle is
// read as a fallback only, because it may still be unset.
func GetRetainedSnapshotFromVSC(vsc *snapshotv1api.VolumeSnapshotContent) *velerov2alpha1api.RetainedCSIStorageSnapshot {
	if vsc == nil {
		return nil
	}

	handle := ""
	if vsc.Spec.Source.SnapshotHandle != nil {
		handle = *vsc.Spec.Source.SnapshotHandle
	}
	if handle == "" && vsc.Status != nil && vsc.Status.SnapshotHandle != nil {
		handle = *vsc.Status.SnapshotHandle
	}
	if handle == "" || vsc.Spec.Driver == "" {
		return nil
	}

	record := &velerov2alpha1api.RetainedCSIStorageSnapshot{
		CSISnapshotHandle:   handle,
		Driver:              vsc.Spec.Driver,
		ManagedByLabelValue: vsc.Labels[kube.VolumeSnapshotContentManagedByLabel],
	}
	if name := vsc.Annotations[VSCDeletionSecretNameAnnotation]; name != "" {
		record.DeletionSecretRef = &corev1api.SecretReference{
			Name:      name,
			Namespace: vsc.Annotations[VSCDeletionSecretNamespaceAnnotation],
		}
	}
	return record
}

// TempVSCName returns the name of the temporary VolumeSnapshotContent rebuilt
// to delete a retained snapshot. It is derived from the driver and the
// snapshot handle, so a retry of an interrupted deletion gets AlreadyExists
// and reuses the leftover instead of stacking a second content for the same
// snapshot.
func TempVSCName(driver, handle string) string {
	sum := sha256.Sum256([]byte(driver + "/" + handle))
	return "vsc-" + hex.EncodeToString(sum[:16])
}

// BuildTempVSCForRetainedSnapshot returns the statically provisioned
// VolumeSnapshotContent through which the CSI snapshotter deletes a retained
// snapshot:
//   - named vsc-<hash> of driver and handle, so a retry reuses the leftover;
//   - labeled velero.io/backup-uid with the UID of the backup the snapshot
//     belongs to, so an operator can tell a content left being deleted after
//     a timeout apart from any other;
//   - deletionPolicy Delete and the retained handle in spec.source.snapshotHandle;
//   - no volumeSnapshotClassName, so the deletion is independent of the class;
//   - the existing legacy action's synthetic volumeSnapshotRef, which the
//     schema requires, with an empty uid: that is what makes the snapshotter's
//     shouldDelete take the unbound pre-provisioned path and delete
//     immediately, where a fabricated uid routes the content into the bound
//     path and expects the referenced VolumeSnapshot to exist;
//   - the bound-protection finalizer, without which the content can be
//     dropped with no DeleteSnapshot call at all;
//   - the managed-by label of the record, so distributed snapshots, which
//     select contents by that label, process it too;
//   - the deletion secret annotations of the record, when the driver needs one.
func BuildTempVSCForRetainedSnapshot(record *velerov2alpha1api.RetainedCSIStorageSnapshot, backupUID types.UID) *snapshotv1api.VolumeSnapshotContent {
	id := TempVSCName(record.Driver, record.CSISnapshotHandle)
	handle := record.CSISnapshotHandle

	vsc := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       id,
			Finalizers: []string{VSCBoundProtectionFinalizer},
			// The UID of the backup the snapshot belongs to, for the operator
			// looking at a content left being deleted after a timeout. The
			// UID, not the name: names can repeat, and the sanitized name label
			// cannot carry a long name.
			Labels: map[string]string{velerov1api.BackupUIDLabel: string(backupUID)},
		},
		Spec: snapshotv1api.VolumeSnapshotContentSpec{
			DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
			Driver:         record.Driver,
			Source: snapshotv1api.VolumeSnapshotContentSource{
				SnapshotHandle: &handle,
			},
			// An empty uid is required: see the comment above.
			VolumeSnapshotRef: corev1api.ObjectReference{
				APIVersion: snapshotv1api.SchemeGroupVersion.String(),
				Kind:       "VolumeSnapshot",
				Namespace:  "ns-" + id,
				Name:       "name-" + id,
			},
		},
	}
	if record.ManagedByLabelValue != "" {
		vsc.Labels[kube.VolumeSnapshotContentManagedByLabel] = record.ManagedByLabelValue
	}
	if record.DeletionSecretRef != nil {
		vsc.Annotations = map[string]string{
			VSCDeletionSecretNameAnnotation:      record.DeletionSecretRef.Name,
			VSCDeletionSecretNamespaceAnnotation: record.DeletionSecretRef.Namespace,
		}
	}
	return vsc
}

// DeleteRetainedSnapshot issues the deletion of the storage snapshot the
// record identifies and returns without waiting for the CSI driver. It creates
// a static VolumeSnapshotContent -- vsc-<hash> of driver and handle,
// deletionPolicy Delete, the bound-protection finalizer, the synthetic
// volumeSnapshotRef with an empty uid, and the managed-by label and
// deletion-secret annotations of the record -- seeds status.snapshotHandle,
// and deletes it. The CSI snapshotter then calls DeleteSnapshot, which the CSI
// spec requires to succeed for a snapshot that is already gone, and removes
// the content. A content the driver cannot delete stays in deletion carrying
// the handle, the trace for manual cleanup.
//
// The snapshotter issues DeleteSnapshot only for a content whose
// status.snapshotHandle is set, and does not set it itself for a snapshot that
// is already gone, so the handle is seeded from the record. The seed is
// confirmed from the object the status write returned before the delete.
//
// A create that finds the content already being deleted is an earlier request
// still in flight; the deletion is treated as issued. One an earlier request
// created but did not get to delete is seeded and deleted here. An error means
// the deletion could not be issued.
func DeleteRetainedSnapshot(
	ctx context.Context,
	crClient crclient.Client,
	record *velerov2alpha1api.RetainedCSIStorageSnapshot,
	backupUID types.UID,
	log logrus.FieldLogger,
) error {
	if record == nil || record.CSISnapshotHandle == "" || record.Driver == "" {
		return errors.New("invalid retained snapshot record, handle and driver are required")
	}

	vsc := BuildTempVSCForRetainedSnapshot(record, backupUID)
	key := crclient.ObjectKey{Name: vsc.Name}
	if err := crClient.Create(ctx, vsc); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "error creating static VolumeSnapshotContent %s", vsc.Name)
		}
		existing := &snapshotv1api.VolumeSnapshotContent{}
		if err := crClient.Get(ctx, key, existing); err != nil {
			if apierrors.IsNotFound(err) {
				log.Infof("Static VolumeSnapshotContent %s of snapshot %s is gone, an earlier deletion request finished", vsc.Name, record.CSISnapshotHandle)
				return nil
			}
			return errors.Wrapf(err, "error getting static VolumeSnapshotContent %s", vsc.Name)
		}
		if existing.DeletionTimestamp != nil {
			log.Infof("Static VolumeSnapshotContent %s of snapshot %s already exists, an earlier deletion request is in flight", vsc.Name, record.CSISnapshotHandle)
			return nil
		}
		// An earlier request created the content and stopped before deleting
		// it; this one finishes it.
		vsc = existing
	}

	if vsc.Status == nil || vsc.Status.SnapshotHandle == nil || *vsc.Status.SnapshotHandle == "" {
		// A conflict means the sidecar wrote the status concurrently: the seed
		// is written again onto the fresh object.
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if vsc.Status == nil {
				vsc.Status = &snapshotv1api.VolumeSnapshotContentStatus{}
			}
			if vsc.Status.SnapshotHandle != nil && *vsc.Status.SnapshotHandle != "" {
				return nil
			}
			vsc.Status.SnapshotHandle = &record.CSISnapshotHandle
			err := crClient.Status().Update(ctx, vsc)
			if apierrors.IsConflict(err) {
				if getErr := crClient.Get(ctx, key, vsc); getErr != nil {
					return getErr
				}
			}
			return err
		}); err != nil {
			return errors.Wrapf(err, "error seeding the status of static VolumeSnapshotContent %s", vsc.Name)
		}
	}
	if vsc.Status == nil || vsc.Status.SnapshotHandle == nil || *vsc.Status.SnapshotHandle == "" {
		return errors.Errorf("the status of static VolumeSnapshotContent %s does not carry the snapshot handle", vsc.Name)
	}

	if err := crClient.Delete(ctx, vsc); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "error deleting static VolumeSnapshotContent %s", vsc.Name)
	}
	log.Infof("Issued the deletion of snapshot %s of driver %s through static VolumeSnapshotContent %s", record.CSISnapshotHandle, record.Driver, vsc.Name)
	return nil
}

// PatchVSCDeletionPolicy patches the deletion policy of the named
// VolumeSnapshotContent through the typed snapshot client.
func PatchVSCDeletionPolicy(ctx context.Context, snapshotClient snapshotter.SnapshotV1Interface, vscName string, policy snapshotv1api.DeletionPolicy) error {
	patchBytes, err := json.Marshal(map[string]any{"spec": map[string]any{"deletionPolicy": string(policy)}})
	if err != nil {
		return errors.Wrap(err, "error marshaling deletion policy patch")
	}
	if _, err := snapshotClient.VolumeSnapshotContents().Patch(ctx, vscName, types.MergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
		return errors.Wrapf(err, "error patching the deletion policy of VolumeSnapshotContent %s", vscName)
	}
	return nil
}
