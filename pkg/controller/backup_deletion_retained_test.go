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
	"testing"
	"time"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/label"
	"github.com/velero-io/velero/pkg/plugin/clientmgmt"
	pluginmocks "github.com/velero-io/velero/pkg/plugin/mocks"
	"github.com/velero-io/velero/pkg/plugin/velero"
	"github.com/velero-io/velero/pkg/plugin/velero/mocks"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/csi"
)

// releaseSidecar plays the CSI snapshotter for the static contents of the
// release: on every read of a content being deleted with a seeded handle it
// calls DeleteSnapshot, which succeeds unless the handle is in failing.
type releaseSidecar struct {
	snapshots   map[string]bool
	failing     map[string]bool
	deleteCalls []string
}

func (s *releaseSidecar) client(t *testing.T, objs ...runtime.Object) crclient.Client {
	t.Helper()
	base := velerotest.NewFakeControllerRuntimeClientBuilder(t).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&snapshotv1api.VolumeSnapshotContent{}).
		Build()
	return interceptor.NewClient(base.(crclient.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if _, ok := obj.(*snapshotv1api.VolumeSnapshotContent); ok {
				vsc := &snapshotv1api.VolumeSnapshotContent{}
				if err := c.Get(ctx, key, vsc); err == nil && vsc.DeletionTimestamp != nil && vsc.Status != nil && vsc.Status.SnapshotHandle != nil {
					handle := *vsc.Status.SnapshotHandle
					s.deleteCalls = append(s.deleteCalls, handle)
					if !s.failing[handle] {
						s.snapshots[handle] = false
						vsc.Finalizers = nil
						_ = c.Update(ctx, vsc)
					}
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

// chainDU returns a DataUpload of backup <name>-uid whose record belongs to
// the chain of retentionDriver and the volume handle, completed the given
// time ago.
func chainDU(name, volumeHandle, snapshotHandle string, phase velerov2alpha1api.DataUploadPhase, completedAgo time.Duration) *velerov2alpha1api.DataUpload {
	done := metav1.NewTime(time.Now().Add(-completedAgo))
	return &velerov2alpha1api.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: velerov1api.DefaultNamespace,
			Name:      name,
			Labels:    map[string]string{velerov1api.BackupUIDLabel: name + "-uid", velerov1api.PVCUIDLabel: "pvc-1"},
		},
		Status: velerov2alpha1api.DataUploadStatus{
			Phase:               phase,
			CompletionTimestamp: &done,
			RetainedCSIStorageSnapshot: &velerov2alpha1api.RetainedCSIStorageSnapshot{
				CSISnapshotHandle: snapshotHandle,
				Driver:            retentionDriver,
				CSIVolumeHandle:   volumeHandle,
			},
		},
	}
}

// TestBackupDeletionOfflineDeletesRetainedSnapshots proves the offline-cleanup
// branch: when the tarball is gone, the DataUpload delete action never runs, so
// the controller deletes the unreleased records of the local DataUploads
// itself, before deleting them; a released one is skipped; a timeout fails the
// request before the local DataUploads, the repository data and the tarball
// are touched, so the backup stays deletable on retry.
func TestBackupDeletionOfflineDeletesRetainedSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stuck    bool
		wantFail bool
	}{
		{name: "unreleased records are deleted, then the deletion completes"},
		{name: "a timeout fails the request with nothing deleted", stuck: true, wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := csi.RetainedSnapshotDeletionTimeout
			csi.RetainedSnapshotDeletionTimeout = 200 * time.Millisecond
			t.Cleanup(func() { csi.RetainedSnapshotDeletionTimeout = orig })

			backup := builder.ForBackup(velerov1api.DefaultNamespace, "foo").StorageLocation("default").SnapshotMoveData(true).Phase(velerov1api.BackupPhaseCompleted).Result()
			backup.UID = "foo-uid"
			location := builder.ForBackupStorageLocation(velerov1api.DefaultNamespace, "default").Provider("objStoreProvider").Bucket("bucket").Phase(velerov1api.BackupStorageLocationPhaseAvailable).Result()
			unreleased := chainDU("du-1", "vol-1", "snap-1", velerov2alpha1api.DataUploadPhaseCompleted, time.Hour)
			released := chainDU("du-2", "vol-2", "snap-2", velerov2alpha1api.DataUploadPhaseCompleted, time.Hour)
			released.Status.RetainedCSIStorageSnapshot.Released = true
			for _, du := range []*velerov2alpha1api.DataUpload{unreleased, released} {
				du.Labels[velerov1api.BackupNameLabel] = label.GetValidName(backup.Name)
			}

			dbr := defaultTestDbr()
			td := setupBackupDeletionControllerTest(t, dbr, backup, location)
			sidecar := &releaseSidecar{snapshots: map[string]bool{"snap-1": true, "snap-2": true}}
			if tc.stuck {
				sidecar.failing = map[string]bool{"snap-1": true}
			}
			c := sidecar.client(t, backup, location, dbr, unreleased, released)
			td.controller.Client = c

			pluginManager := &pluginmocks.Manager{}
			pluginManager.On("GetDeleteItemActions").Return([]velero.DeleteItemAction{new(mocks.DeleteItemAction)}, nil)
			pluginManager.On("CleanupClients")
			td.controller.newPluginManager = func(logrus.FieldLogger) clientmgmt.Manager { return pluginManager }
			td.backupStore.On("GetBackupContents", dbr.Spec.BackupName).Return(nil, fmt.Errorf("key not found"))
			td.backupStore.On("GetBackupVolumeSnapshots", dbr.Spec.BackupName).Return(nil, nil)
			td.backupStore.On("DeleteBackup", dbr.Spec.BackupName).Return(nil)

			_, err := td.controller.Reconcile(t.Context(), td.req)
			require.NoError(t, err)

			assert.Equal(t, []string{"snap-1"}, sidecar.deleteCalls, "only the unreleased record is deleted")
			local := &velerov2alpha1api.DataUpload{}
			localErr := c.Get(t.Context(), types.NamespacedName{Namespace: unreleased.Namespace, Name: unreleased.Name}, local)
			if tc.wantFail {
				res := &velerov1api.DeleteBackupRequest{}
				require.NoError(t, c.Get(t.Context(), td.req.NamespacedName, res))
				assert.Equal(t, velerov1api.DeleteBackupRequestPhaseProcessed, res.Status.Phase)
				require.Len(t, res.Status.Errors, 1)
				assert.Contains(t, res.Status.Errors[0], "error deleting the retained snapshots")
				require.NoError(t, localErr, "the local DataUploads are kept for the retry")
				td.backupStore.AssertNotCalled(t, "DeleteBackup", mock.Anything)
				require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: backup.Namespace, Name: backup.Name}, &velerov1api.Backup{}))
				return
			}
			assert.False(t, sidecar.snapshots["snap-1"])
			assert.True(t, sidecar.snapshots["snap-2"], "the released record is skipped")
			assert.True(t, apierrors.IsNotFound(localErr), "the local DataUploads are deleted after their snapshots")
			err = c.Get(t.Context(), types.NamespacedName{Namespace: backup.Namespace, Name: backup.Name}, &velerov1api.Backup{})
			assert.True(t, apierrors.IsNotFound(err), "the deletion completes")
		})
	}
}
