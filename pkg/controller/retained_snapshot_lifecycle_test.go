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
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/datamover"
	"github.com/velero-io/velero/pkg/plugin/velero"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/csi"
)

const (
	lifecycleDriver = "rbd.csi.ceph.com"
	lifecycleHandle = "0001-0009-rook-ceph-0000000000000001-snap"
)

// fakeCSIBackend plays the CSI snapshot controller, the CSI snapshotter and the
// storage. Deleting a VolumeSnapshotContent with deletionPolicy Delete deletes
// its snapshot from the storage, which succeeds when the snapshot is already
// gone as the CSI spec requires, and then removes the VolumeSnapshotContent.
type fakeCSIBackend struct {
	snapshots     map[string]bool
	deleteCallsTo []string
}

func (b *fakeCSIBackend) client(t *testing.T, objs ...runtime.Object) client.Client {
	t.Helper()
	fakeClient := velerotest.NewFakeControllerRuntimeClient(t, objs...)
	return interceptor.NewClient(fakeClient.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := c.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*snapshotv1api.VolumeSnapshotContent); !ok {
				return nil
			}
			vsc := &snapshotv1api.VolumeSnapshotContent{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), vsc); err != nil {
				return client.IgnoreNotFound(err)
			}
			if vsc.Spec.DeletionPolicy != snapshotv1api.VolumeSnapshotContentDelete || vsc.Spec.Source.SnapshotHandle == nil {
				return nil
			}
			b.deleteCallsTo = append(b.deleteCallsTo, *vsc.Spec.Source.SnapshotHandle)
			delete(b.snapshots, *vsc.Spec.Source.SnapshotHandle)
			vsc.Finalizers = nil
			return c.Update(ctx, vsc)
		},
	})
}

func lifecycleBackupVSC(policy snapshotv1api.DeletionPolicy) *snapshotv1api.VolumeSnapshotContent {
	handle := lifecycleHandle
	return &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo-du",
			Annotations: map[string]string{
				csi.VSCDeletionSecretNameAnnotation:      "rook-csi-rbd-provisioner",
				csi.VSCDeletionSecretNamespaceAnnotation: "rook-ceph",
			},
		},
		Spec: snapshotv1api.VolumeSnapshotContentSpec{
			DeletionPolicy: policy,
			Driver:         lifecycleDriver,
		},
		Status: &snapshotv1api.VolumeSnapshotContentStatus{SnapshotHandle: &handle},
	}
}

func lifecycleDataUpload(retained *velerov2alpha1api.RetainedSnapshot) *velerov2alpha1api.DataUpload {
	du := builder.ForDataUpload(velerov1api.DefaultNamespace, "foo-du").
		Labels(map[string]string{velerov1api.BackupNameLabel: "foo"}).
		SnapshotType(velerov2alpha1api.SnapshotTypeCSI).
		SourceNamespace("app").
		SourcePVC("data").
		DataMover("velero-block").
		Phase(velerov2alpha1api.DataUploadPhaseCompleted).
		SnapshotID("kopia-snapshot-1").
		Result()
	du.Status.RetainedSnapshot = retained
	return du
}

// executeDataUploadDeleteAction runs the DataUpload DeleteItemAction for the
// DataUpload as it is stored in the backup tarball.
func executeDataUploadDeleteAction(t *testing.T, c client.Client, backup *velerov1api.Backup, du *velerov2alpha1api.DataUpload) {
	t.Helper()
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(du)
	require.NoError(t, err)
	require.NoError(t, datamover.NewDataUploadDeleteAction(velerotest.NewLogger(), c).Execute(&velero.DeleteItemActionExecuteInput{
		Item:   &unstructured.Unstructured{Object: content},
		Backup: backup,
	}))
}

func skipTempVSCDeleteGap(t *testing.T) {
	t.Helper()
	originalSleep := csi.SleepBetweenTempVSCCreateAndDelete
	t.Cleanup(func() { csi.SleepBetweenTempVSCCreateAndDelete = originalSleep })
	csi.SleepBetweenTempVSCCreateAndDelete = func(time.Duration) {}
}

// TestRetainedSnapshotLifecycle follows a snapshot from the end of the data
// movement to the backup deletion: the node-agent records the snapshot of a
// completed DataUpload, the DataUpload DeleteItemAction passes it on, and the
// backup deletion has the CSI driver delete it from the storage.
func TestRetainedSnapshotLifecycle(t *testing.T) {
	tests := []struct {
		name          string
		policy        snapshotv1api.DeletionPolicy
		wantRecorded  bool
		wantDeleteRPC []string
	}{
		{
			name:          "Retain: the snapshot is kept after the backup and deleted from the storage with the backup",
			policy:        snapshotv1api.VolumeSnapshotContentRetain,
			wantRecorded:  true,
			wantDeleteRPC: []string{lifecycleHandle},
		},
		{
			// The CSI driver deletes the snapshot when the node-agent cleans up
			// the backup VolumeSnapshotContent, so nothing is left to delete.
			name:   "Delete: nothing is recorded and the backup deletion doesn't touch the storage",
			policy: snapshotv1api.VolumeSnapshotContentDelete,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			skipTempVSCDeleteGap(t)
			backend := &fakeCSIBackend{snapshots: map[string]bool{lifecycleHandle: true, "snapshot-of-another-backup": true}}
			c := backend.client(t)
			backup := builder.ForBackup(velerov1api.DefaultNamespace, "foo").StorageLocation("default").Result()

			// The node-agent records the snapshot when the DataUpload completes.
			retained := csi.GetRetainedSnapshot(lifecycleBackupVSC(tc.policy))
			assert.Equal(t, tc.wantRecorded, retained != nil)

			// The backup is deleted.
			executeDataUploadDeleteAction(t, c, backup, lifecycleDataUpload(retained))
			r := &backupDeletionReconciler{Client: c, logger: velerotest.NewLogger()}
			assert.Empty(t, r.deleteRetainedSnapshots(t.Context(), backup, velerotest.NewLogger()))

			assert.Equal(t, tc.wantDeleteRPC, backend.deleteCallsTo)
			assert.Equal(t, !tc.wantRecorded, backend.snapshots[lifecycleHandle], "snapshot of the deleted backup in the storage")
			assert.True(t, backend.snapshots["snapshot-of-another-backup"], "snapshots of other backups must stay")

			vscs := &snapshotv1api.VolumeSnapshotContentList{}
			require.NoError(t, c.List(t.Context(), vscs))
			assert.Empty(t, vscs.Items)
			cms := &corev1api.ConfigMapList{}
			require.NoError(t, c.List(t.Context(), cms, client.MatchingLabels{velerov1api.DataUploadRetainedSnapshotLabel: "true"}))
			assert.Empty(t, cms.Items)
		})
	}
}

// TestRetainedSnapshotDeletionIdempotent checks that the retained snapshot
// deletion succeeds however much of it an earlier attempt already did.
func TestRetainedSnapshotDeletionIdempotent(t *testing.T) {
	retained := &velerov2alpha1api.RetainedSnapshot{SnapshotHandle: lifecycleHandle, Driver: lifecycleDriver}
	sum := sha256.Sum256([]byte(lifecycleDriver + "/" + lifecycleHandle))
	tempVSCName := "vsc-" + hex.EncodeToString(sum[:16])

	tests := []struct {
		name string
		// retained is the DataUpload's status.retainedSnapshot in the backup tarball.
		retained *velerov2alpha1api.RetainedSnapshot
		// snapshotInStorage is whether the storage still has the snapshot.
		snapshotInStorage bool
		// tempVSCLeft is whether an interrupted attempt left the temp VSC.
		tempVSCLeft bool
		// skipDeleteAction leaves out the DeleteItemAction, so no configmap records the snapshot.
		skipDeleteAction bool
		attempts         int
		wantDeleteRPC    []string
	}{
		{
			// The second attempt asks the CSI driver again, and the driver
			// reports success for the snapshot it already deleted.
			name:              "deleting again after a successful deletion",
			retained:          retained,
			snapshotInStorage: true,
			attempts:          2,
			wantDeleteRPC:     []string{lifecycleHandle, lifecycleHandle},
		},
		{
			name:          "snapshot already gone from the storage",
			retained:      retained,
			attempts:      1,
			wantDeleteRPC: []string{lifecycleHandle},
		},
		{
			name:              "temp VSC left by an interrupted attempt",
			retained:          retained,
			snapshotInStorage: true,
			tempVSCLeft:       true,
			attempts:          1,
			wantDeleteRPC:     []string{lifecycleHandle},
		},
		{
			name:              "no configmap records the snapshot",
			retained:          retained,
			snapshotInStorage: true,
			skipDeleteAction:  true,
			attempts:          1,
		},
		{
			name:     "DataUpload without retainedSnapshot",
			attempts: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			skipTempVSCDeleteGap(t)
			backend := &fakeCSIBackend{snapshots: map[string]bool{lifecycleHandle: tc.snapshotInStorage}}
			var objs []runtime.Object
			if tc.tempVSCLeft {
				handle := lifecycleHandle
				objs = append(objs, &snapshotv1api.VolumeSnapshotContent{
					ObjectMeta: metav1.ObjectMeta{Name: tempVSCName, Finalizers: []string{csi.VSCBoundProtectionFinalizer}},
					Spec: snapshotv1api.VolumeSnapshotContentSpec{
						DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
						Driver:         lifecycleDriver,
						Source:         snapshotv1api.VolumeSnapshotContentSource{SnapshotHandle: &handle},
					},
				})
			}
			c := backend.client(t, objs...)
			backup := builder.ForBackup(velerov1api.DefaultNamespace, "foo").StorageLocation("default").Result()
			r := &backupDeletionReconciler{Client: c, logger: velerotest.NewLogger()}

			for i := 0; i < tc.attempts; i++ {
				if !tc.skipDeleteAction {
					executeDataUploadDeleteAction(t, c, backup, lifecycleDataUpload(tc.retained))
				}
				assert.Empty(t, r.deleteRetainedSnapshots(t.Context(), backup, velerotest.NewLogger()), "attempt %d", i+1)
			}

			assert.Equal(t, tc.wantDeleteRPC, backend.deleteCallsTo)
			vscs := &snapshotv1api.VolumeSnapshotContentList{}
			require.NoError(t, c.List(t.Context(), vscs))
			assert.Empty(t, vscs.Items)
			cms := &corev1api.ConfigMapList{}
			require.NoError(t, c.List(t.Context(), cms, client.MatchingLabels{velerov1api.DataUploadRetainedSnapshotLabel: "true"}))
			assert.Empty(t, cms.Items)
		})
	}
}
