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
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapshotter "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/typed/volumesnapshot/v1"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kwait "k8s.io/apimachinery/pkg/util/wait"
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

// RetainedSnapshotDeletionTimeout bounds the wait for the CSI snapshotter to
// delete a retained snapshot and remove the static VolumeSnapshotContent: the
// CSI operation timeout, --resource-timeout, ten minutes by default; the
// node-agent sets it from its flag. On a live cluster a CSI snapshotter leader
// handover was observed to delay DeleteSnapshot by roughly two and a half
// minutes, which a short fixed bound would turn into a spurious failure.
var RetainedSnapshotDeletionTimeout = 10 * time.Minute

// retainedSnapshotPollInterval is the fixed interval, without backoff, at which
// the deletion of a temporary VolumeSnapshotContent is polled.
var retainedSnapshotPollInterval = 2 * time.Second

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

// RetainedSnapshotDeletionPendingError reports a static content that is still
// being deleted when the caller stops looking: the bound expired, or the
// caller asked not to wait. The content is left in place, being deleted with
// its finalizer, so the CSI snapshotter keeps retrying DeleteSnapshot, and a
// later attempt finds it through the same name.
type RetainedSnapshotDeletionPendingError struct {
	Handle  string
	Content string
	Waited  time.Duration
}

func (e *RetainedSnapshotDeletionPendingError) Error() string {
	if e.Waited <= 0 {
		return fmt.Sprintf("snapshot %s is not deleted yet; static VolumeSnapshotContent %s is left being deleted and the CSI snapshotter keeps retrying, check its events and the CSI snapshotter logs",
			e.Handle, e.Content)
	}
	return fmt.Sprintf("snapshot %s is not deleted within %v; static VolumeSnapshotContent %s is left being deleted and the CSI snapshotter keeps retrying, check its events and the CSI snapshotter logs",
		e.Handle, e.Waited, e.Content)
}

// RetainedSnapshotDeletionOptions tunes how long DeleteRetainedSnapshot waits.
type RetainedSnapshotDeletionOptions struct {
	// Timeout bounds each wait; RetainedSnapshotDeletionTimeout when zero.
	Timeout time.Duration
	// NoWait creates, seeds and deletes the content but only checks once
	// whether it is gone: a backup deletion request waits out the bound at
	// most once, and the records after the first timeout are deleted in the
	// background by the sidecar.
	NoWait bool
	// CheckInProgressOnce checks a content an earlier attempt already left
	// being deleted once instead of waiting on it again, so a snapshot the
	// driver cannot delete does not add the bound to every later release.
	CheckInProgressOnce bool
}

// DeleteRetainedSnapshot has the CSI snapshotter delete the storage snapshot
// identified by record, through a static VolumeSnapshotContent: named
// vsc-<hash> of driver and handle, deletionPolicy Delete, the bound-protection
// finalizer, the synthetic volumeSnapshotRef with an empty uid, and the
// managed-by label and deletion-secret annotations of the record. It is the
// one helper of the exposer's release and the backup deletion's delete item
// action. All reads go through the given reader, which must be uncached when
// the writing client is manager-cached; a nil reader falls back to the writing
// client.
//
// In external-snapshotter v8.6.0, syncContent issues DeleteSnapshot only when
// deletionPolicy is Delete and status.snapshotHandle is set, and the sidecar's
// own status update runs ListSnapshots first, which fails for an already-gone
// snapshot. So the handle is seeded from the record through the status
// subresource, the seed is confirmed from the object the write returned -- a
// conflict means the sidecar wrote status concurrently, and any handle
// satisfies it -- and only then is the content deleted, with no readyToUse
// wait. The deletion
// is polled every two seconds until the content is gone or the bound expires.
//
// A create that finds the content already there is an earlier attempt: a
// content not yet deleting is seeded and deleted as above; one already being
// deleted is waited on again, or checked once with CheckInProgressOnce. At
// the bound, or when not waiting, a content still present returns a
// *RetainedSnapshotDeletionPendingError and is left being deleted.
func DeleteRetainedSnapshot(
	ctx context.Context,
	crClient crclient.Client,
	reader crclient.Reader,
	record *velerov2alpha1api.RetainedCSIStorageSnapshot,
	backupUID types.UID,
	opts RetainedSnapshotDeletionOptions,
	log logrus.FieldLogger,
) error {
	if reader == nil {
		reader = crClient
	}
	if record == nil || record.CSISnapshotHandle == "" || record.Driver == "" {
		return errors.New("invalid retained snapshot record, handle and driver are required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = RetainedSnapshotDeletionTimeout
	}

	tempVSC := BuildTempVSCForRetainedSnapshot(record, backupUID)
	key := crclient.ObjectKey{Name: tempVSC.Name}
	pending := &RetainedSnapshotDeletionPendingError{Handle: record.CSISnapshotHandle, Content: tempVSC.Name}
	wait := !opts.NoWait

	current := tempVSC
	if err := crClient.Create(ctx, tempVSC); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "error creating static VolumeSnapshotContent %s", tempVSC.Name)
		}
		existing := &snapshotv1api.VolumeSnapshotContent{}
		if err := reader.Get(ctx, key, existing); err != nil {
			if apierrors.IsNotFound(err) {
				log.Infof("Static VolumeSnapshotContent %s of snapshot %s is gone, the snapshot is deleted", tempVSC.Name, record.CSISnapshotHandle)
				return nil
			}
			return errors.Wrapf(err, "error getting static VolumeSnapshotContent %s", tempVSC.Name)
		}
		current = existing
		if current.DeletionTimestamp != nil {
			log.Infof("Static VolumeSnapshotContent %s of snapshot %s is already being deleted by an earlier attempt", tempVSC.Name, record.CSISnapshotHandle)
			if opts.CheckInProgressOnce {
				wait = false
			}
			return waitGone(ctx, reader, key, timeout, wait, pending, record, log)
		}
		log.Infof("Static VolumeSnapshotContent %s for snapshot %s already exists, reusing it", tempVSC.Name, record.CSISnapshotHandle)
	} else {
		log.Infof("Created static VolumeSnapshotContent %s with DeletionPolicy=Delete for snapshot %s", tempVSC.Name, record.CSISnapshotHandle)
	}

	// Seed status.snapshotHandle. The seed is confirmed before the delete --
	// deleting the content before the sidecar sees status.snapshotHandle would
	// remove it with no DeleteSnapshot call -- from the object the status write
	// returned, not from a read, which a cached reader can answer with the
	// object as it was before the write. A conflict means the sidecar wrote the
	// status concurrently; then the content is read, and any handle in it
	// satisfies the seed.
	confirmed := current.DeepCopy()
	if confirmed.Status == nil {
		confirmed.Status = &snapshotv1api.VolumeSnapshotContentStatus{}
	}
	if confirmed.Status.SnapshotHandle == nil || *confirmed.Status.SnapshotHandle == "" {
		confirmed.Status.SnapshotHandle = &record.CSISnapshotHandle
		if err := crClient.Status().Update(ctx, confirmed); err != nil {
			if !apierrors.IsConflict(err) {
				return errors.Wrapf(err, "error writing the status of static VolumeSnapshotContent %s", tempVSC.Name)
			}
			confirmed = &snapshotv1api.VolumeSnapshotContent{}
			if err := reader.Get(ctx, key, confirmed); err != nil {
				return errors.Wrapf(err, "error reading static VolumeSnapshotContent %s after a status conflict", tempVSC.Name)
			}
		}
	}
	if confirmed.Status == nil || confirmed.Status.SnapshotHandle == nil || *confirmed.Status.SnapshotHandle == "" {
		return errors.Errorf("the status of static VolumeSnapshotContent %s does not carry the snapshot handle", tempVSC.Name)
	}

	if err := crClient.Delete(ctx, confirmed); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "error deleting static VolumeSnapshotContent %s", tempVSC.Name)
	}
	return waitGone(ctx, reader, key, timeout, wait, pending, record, log)
}

// waitGone polls the static content every two seconds until it is gone or the
// bound expires; without wait it checks once.
func waitGone(ctx context.Context, reader crclient.Reader, key crclient.ObjectKey, timeout time.Duration, wait bool,
	pending *RetainedSnapshotDeletionPendingError, record *velerov2alpha1api.RetainedCSIStorageSnapshot, log logrus.FieldLogger) error {
	gone := func(ctx context.Context) (bool, error) {
		err := reader.Get(ctx, key, &snapshotv1api.VolumeSnapshotContent{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, nil
	}
	if !wait {
		if done, _ := gone(ctx); done {
			log.Infof("Static VolumeSnapshotContent %s is gone, the CSI driver deleted snapshot %s", key.Name, record.CSISnapshotHandle)
			return nil
		}
		return pending
	}
	if err := kwait.PollUntilContextTimeout(ctx, retainedSnapshotPollInterval, timeout, true, gone); err != nil {
		pending.Waited = timeout
		return pending
	}
	log.Infof("Deleted static VolumeSnapshotContent %s, the CSI driver deleted snapshot %s", key.Name, record.CSISnapshotHandle)
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
