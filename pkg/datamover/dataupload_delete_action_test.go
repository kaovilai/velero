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

package datamover

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	storagev1api "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/plugin/velero"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/csi"
)

func toUnstructured(t *testing.T, du *velerov2alpha1.DataUpload) runtime.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(du)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: m}
}

func newCompletedDataUpload(name, ownerBackup string) *velerov2alpha1.DataUpload {
	du := &velerov2alpha1.DataUpload{
		TypeMeta: metav1.TypeMeta{
			APIVersion: velerov2alpha1.SchemeGroupVersion.String(),
			Kind:       "DataUpload",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "velero",
			Name:      name,
		},
		Spec: velerov2alpha1.DataUploadSpec{
			SnapshotType:          velerov2alpha1.SnapshotTypeCSI,
			SourcePVC:             "my-pvc",
			SourceNamespace:       "app",
			BackupStorageLocation: "default",
			DataMover:             "velero",
		},
		Status: velerov2alpha1.DataUploadStatus{
			Phase:      velerov2alpha1.DataUploadPhaseCompleted,
			SnapshotID: "kopia-snapshot-id",
		},
	}
	if ownerBackup != "" {
		du.Labels = map[string]string{velerov1.BackupNameLabel: ownerBackup}
	}
	return du
}

func TestDataUploadDeleteActionAppliesTo(t *testing.T) {
	a := NewDataUploadDeleteAction(logrus.StandardLogger(), nil, 0)
	selector, err := a.AppliesTo()
	require.NoError(t, err)
	require.Equal(t, velero.ResourceSelector{IncludedResources: []string{"datauploads.velero.io"}}, selector)
}

func TestDataUploadDeleteActionExecute(t *testing.T) {
	tests := []struct {
		name             string
		duName           string
		duOwnerBackup    string // value placed in velero.io/backup-name label on the DataUpload
		executingBackup  string // name of the Backup being deleted (input.Backup.Name)
		wantConfigMap    bool
		wantWarnContains string // substring expected in a warn-level log entry; empty means no warn expected
	}{
		{
			name:             "DataUpload owned by the executing backup creates a snapshot-info ConfigMap",
			duName:           "daily-backup-abcde",
			duOwnerBackup:    "daily-backup",
			executingBackup:  "daily-backup",
			wantConfigMap:    true,
			wantWarnContains: "",
		},
		{
			name:             "DataUpload owned by a different backup is skipped and a warning is logged",
			duName:           "daily-backup-abcde",
			duOwnerBackup:    "daily-backup",
			executingBackup:  "hourly-backup",
			wantConfigMap:    false,
			wantWarnContains: "velero namespace",
		},
		{
			name:             "DataUpload with no backup-name label is skipped and a warning is logged",
			duName:           "unlabeled-du",
			duOwnerBackup:    "",
			executingBackup:  "some-backup",
			wantConfigMap:    false,
			wantWarnContains: "cannot be verified",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			crClient := velerotest.NewFakeControllerRuntimeClient(t)
			logger, hook := logrustest.NewNullLogger()
			logger.SetLevel(logrus.DebugLevel)
			action := NewDataUploadDeleteAction(logger, crClient, 0)

			du := newCompletedDataUpload(tc.duName, tc.duOwnerBackup)
			backup := builder.ForBackup("velero", tc.executingBackup).StorageLocation("default").Result()

			err := action.Execute(&velero.DeleteItemActionExecuteInput{
				Item:   toUnstructured(t, du),
				Backup: backup,
			})
			require.NoError(t, err)

			cm := &corev1api.ConfigMap{}
			getErr := crClient.Get(t.Context(), crclient.ObjectKey{
				Namespace: backup.Namespace,
				Name:      fmt.Sprintf("%s-info", du.Name),
			}, cm)

			if tc.wantConfigMap {
				require.NoError(t, getErr, "expected snapshot-info ConfigMap to be created")
				assert.Equal(t, tc.executingBackup, cm.Labels[velerov1.BackupNameLabel])
				assert.Equal(t, "true", cm.Labels[velerov1.DataUploadSnapshotInfoLabel])
			} else {
				require.Error(t, getErr)
				assert.True(t, apierrors.IsNotFound(getErr),
					"expected no ConfigMap to be created for foreign DataUpload, but got: %v", getErr)
			}

			// The action must surface DataUploads it cannot generate a useful
			// snapshot-info ConfigMap for as warnings, so operators who
			// accidentally included the velero namespace in a backup (or
			// otherwise produced DataUploads without a verifiable owner) can
			// detect the misconfiguration from logs instead of having the
			// case silently swallowed.
			var sawWarn bool
			for _, entry := range hook.AllEntries() {
				if entry.Level == logrus.WarnLevel &&
					strings.Contains(entry.Message, tc.duName) &&
					(tc.wantWarnContains == "" || strings.Contains(entry.Message, tc.wantWarnContains)) {
					sawWarn = true
					break
				}
			}
			assert.Equal(t, tc.wantWarnContains != "", sawWarn,
				"unexpected warn log presence (wantContains=%q, got=%v); entries=%v",
				tc.wantWarnContains, sawWarn, hook.AllEntries())
		})
	}
}

const deleteTestHandle = "0001-0009-rook-ceph-0000000000000001-snap"

// fakeSidecar plays the CSI snapshotter sidecar for the static
// VolumeSnapshotContent of a retained snapshot: on every read of a content
// being deleted with a seeded handle it calls DeleteSnapshot, which deletes the
// snapshot and drops the finalizer unless the driver fails.
type fakeSidecar struct {
	snapshots   map[string]bool
	driverFails bool
	deleteCalls []string
}

func (f *fakeSidecar) client(t *testing.T, objs ...runtime.Object) crclient.Client {
	t.Helper()
	objs = append(objs, &storagev1api.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "rbd.csi.ceph.com"}})
	base := velerotest.NewFakeControllerRuntimeClientBuilder(t).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&snapshotv1api.VolumeSnapshotContent{}).
		Build()
	return interceptor.NewClient(base.(crclient.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if _, ok := obj.(*snapshotv1api.VolumeSnapshotContent); ok {
				vsc := &snapshotv1api.VolumeSnapshotContent{}
				if err := c.Get(ctx, key, vsc); err == nil && vsc.DeletionTimestamp != nil && vsc.Status != nil && vsc.Status.SnapshotHandle != nil {
					f.deleteCalls = append(f.deleteCalls, *vsc.Status.SnapshotHandle)
					if !f.driverFails {
						f.snapshots[*vsc.Status.SnapshotHandle] = false
						vsc.Finalizers = nil
						_ = c.Update(ctx, vsc)
					}
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

func retainedRecord(handle, driver string) *velerov2alpha1.RetainedCSIStorageSnapshot {
	return &velerov2alpha1.RetainedCSIStorageSnapshot{CSISnapshotHandle: handle, Driver: driver}
}

func shortenDeletionBound(t *testing.T) {
	t.Helper()
	orig := csi.RetainedSnapshotDeletionTimeout
	csi.RetainedSnapshotDeletionTimeout = 300 * time.Millisecond
	t.Cleanup(func() { csi.RetainedSnapshotDeletionTimeout = orig })
}

// TestDataUploadDeleteActionDeletesRetainedSnapshot proves the synchronous
// deletion of a retained snapshot with its backup: the record of every
// DataUpload in the tarball is deleted through the static content before the
// repository data; no DataUpload in the cluster is read; a snapshot that is
// already gone -- released by a later backup, or never retained -- deletes
// successfully.
func TestDataUploadDeleteActionDeletesRetainedSnapshot(t *testing.T) {
	shortenDeletionBound(t)
	backup := builder.ForBackup("velero", "daily-backup").StorageLocation("default").Result()
	backup.UID = "daily-uid"

	tests := []struct {
		name        string
		tarball     func(*velerov2alpha1.DataUpload)
		local       bool
		gone        bool
		wantDeleted bool
	}{
		{
			// Also the Completed DataUpload deleted through the API: the
			// tarball still carries its record.
			name: "a record from the tarball is deleted",
			tarball: func(du *velerov2alpha1.DataUpload) {
				du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
			},
			wantDeleted: true,
		},
		{
			name: "a failed DataUpload's record is deleted too",
			tarball: func(du *velerov2alpha1.DataUpload) {
				du.Status.Phase = velerov2alpha1.DataUploadPhaseFailed
				du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
			},
			wantDeleted: true,
		},
		{
			name: "the local DataUpload is not read: its missing record changes nothing",
			tarball: func(du *velerov2alpha1.DataUpload) {
				du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
			},
			local:       true,
			wantDeleted: true,
		},
		{
			name: "a snapshot already gone deletes successfully",
			tarball: func(du *velerov2alpha1.DataUpload) {
				du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
			},
			gone:        true,
			wantDeleted: true,
		},
		{
			name:    "a DataUpload without a record deletes nothing",
			tarball: func(*velerov2alpha1.DataUpload) {},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tarball := newCompletedDataUpload("daily-backup-abcde", "daily-backup")
			tarball.Spec.DataMover = "velero-block"
			tc.tarball(tarball)
			var objs []runtime.Object
			if tc.local {
				local := tarball.DeepCopy()
				local.Status.RetainedCSIStorageSnapshot = nil
				objs = append(objs, local)
			}
			sidecar := &fakeSidecar{snapshots: map[string]bool{deleteTestHandle: !tc.gone}}
			crClient := sidecar.client(t, objs...)
			action := NewDataUploadDeleteAction(velerotest.NewLogger(), crClient, 0)

			require.NoError(t, action.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, tarball), Backup: backup}))

			if tc.wantDeleted {
				assert.Equal(t, []string{deleteTestHandle}, sidecar.deleteCalls)
				assert.False(t, sidecar.snapshots[deleteTestHandle])
			} else {
				assert.Empty(t, sidecar.deleteCalls)
				assert.True(t, sidecar.snapshots[deleteTestHandle])
			}
			contents := &snapshotv1api.VolumeSnapshotContentList{}
			require.NoError(t, crClient.List(t.Context(), contents))
			assert.Empty(t, contents.Items, "no static content is left behind")
		})
	}
}

// TestDataUploadDeleteActionUnknownDriver proves a record naming a driver the
// cluster does not run is skipped with a Warning event on the Backup: no
// static content is created and the deletion proceeds.
func TestDataUploadDeleteActionUnknownDriver(t *testing.T) {
	backup := builder.ForBackup("velero", "daily-backup").StorageLocation("default").Result()
	backup.UID = "daily-uid"
	du := newCompletedDataUpload("daily-backup-abcde", "daily-backup")
	du.Spec.DataMover = "velero-block"
	du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "unknown.csi.example.com")

	sidecar := &fakeSidecar{snapshots: map[string]bool{deleteTestHandle: true}}
	crClient := sidecar.client(t)
	action := NewDataUploadDeleteAction(velerotest.NewLogger(), crClient, 0)

	require.NoError(t, action.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, du), Backup: backup}))
	contents := &snapshotv1api.VolumeSnapshotContentList{}
	require.NoError(t, crClient.List(t.Context(), contents))
	assert.Empty(t, contents.Items)
	events := &corev1api.EventList{}
	require.NoError(t, crClient.List(t.Context(), events))
	require.Len(t, events.Items, 1)
	assert.Equal(t, "RetainedSnapshotDriverUnknown", events.Items[0].Reason)
	assert.Equal(t, corev1api.EventTypeWarning, events.Items[0].Type)
	assert.Equal(t, "Backup", events.Items[0].InvolvedObject.Kind)
}

// TestDataUploadDeleteActionRetainedSnapshotFailure proves the failure path
// and the one-wait rule: a deletion that does not finish within the bound
// returns the helper's error unchanged, which fails the request before the
// repository data, the DataUploads and the tarball are touched, and leaves the
// static content being deleted; the next record of the same request is
// deleted without waiting and returns an error at once; a retry of the backup
// deletion resumes the deletions in progress.
func TestDataUploadDeleteActionRetainedSnapshotFailure(t *testing.T) {
	shortenDeletionBound(t)
	backup := builder.ForBackup("velero", "daily-backup").StorageLocation("default").Result()
	backup.UID = "daily-uid"
	first := newCompletedDataUpload("daily-backup-aaaaa", "daily-backup")
	first.Spec.DataMover = "velero-block"
	first.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
	second := newCompletedDataUpload("daily-backup-bbbbb", "daily-backup")
	second.Spec.DataMover = "velero-block"
	second.Status.RetainedCSIStorageSnapshot = retainedRecord("second-snap", "rbd.csi.ceph.com")

	sidecar := &fakeSidecar{snapshots: map[string]bool{deleteTestHandle: true, "second-snap": true}, driverFails: true}
	crClient := sidecar.client(t)
	action := NewDataUploadDeleteAction(velerotest.NewLogger(), crClient, 0)

	var pending *csi.RetainedSnapshotDeletionPendingError
	err := action.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, first), Backup: backup})
	require.ErrorAs(t, err, &pending, "the helper's error is returned unchanged")
	assert.Positive(t, pending.Waited, "the first timeout waits out the bound")

	start := time.Now()
	err = action.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, second), Backup: backup})
	require.ErrorAs(t, err, &pending)
	assert.Zero(t, pending.Waited, "after the first timeout the request does not wait again")
	assert.Less(t, time.Since(start), csi.RetainedSnapshotDeletionTimeout)
	contents := &snapshotv1api.VolumeSnapshotContentList{}
	require.NoError(t, crClient.List(t.Context(), contents))
	require.Len(t, contents.Items, 2, "both static contents are left being deleted for the sidecar")
	for i := range contents.Items {
		assert.NotNil(t, contents.Items[i].DeletionTimestamp)
	}

	// A retry of the backup deletion is a new request, a new action.
	sidecar.driverFails = false
	retry := NewDataUploadDeleteAction(velerotest.NewLogger(), crClient, 0)
	require.NoError(t, retry.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, first), Backup: backup}))
	require.NoError(t, retry.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, second), Backup: backup}))
	assert.False(t, sidecar.snapshots[deleteTestHandle])
	assert.False(t, sidecar.snapshots["second-snap"])
}

// TestDataUploadDeleteActionBoundIsTheResourceTimeout proves the wait for a
// retained snapshot's deletion is bounded by the server's --resource-timeout
// handed to the action, not the helper's ten-minute default.
func TestDataUploadDeleteActionBoundIsTheResourceTimeout(t *testing.T) {
	backup := builder.ForBackup("velero", "daily-backup").StorageLocation("default").Result()
	backup.UID = "daily-uid"
	du := newCompletedDataUpload("daily-backup-aaaaa", "daily-backup")
	du.Spec.DataMover = "velero-block"
	du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
	sidecar := &fakeSidecar{snapshots: map[string]bool{deleteTestHandle: true}, driverFails: true}
	resourceTimeout := 300 * time.Millisecond
	action := NewDataUploadDeleteAction(velerotest.NewLogger(), sidecar.client(t), resourceTimeout)

	start := time.Now()
	err := action.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, du), Backup: backup})
	var pending *csi.RetainedSnapshotDeletionPendingError
	require.ErrorAs(t, err, &pending)
	assert.Equal(t, resourceTimeout, pending.Waited)
	assert.Less(t, time.Since(start), 30*time.Second)
	assert.Contains(t, err.Error(), "300ms")
}

// TestDataUploadDeleteActionWaitsAgainOnContentInDeletion proves a re-issued
// backup deletion waits again on a static content an earlier request left
// being deleted -- unlike the release, which checks it once -- so the retry
// confirms the snapshot is gone, and succeeds once the driver deletes it.
func TestDataUploadDeleteActionWaitsAgainOnContentInDeletion(t *testing.T) {
	backup := builder.ForBackup("velero", "daily-backup").StorageLocation("default").Result()
	backup.UID = "daily-uid"
	du := newCompletedDataUpload("daily-backup-aaaaa", "daily-backup")
	du.Spec.DataMover = "velero-block"
	du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
	sidecar := &fakeSidecar{snapshots: map[string]bool{deleteTestHandle: true}, driverFails: true}
	crClient := sidecar.client(t)
	resourceTimeout := 300 * time.Millisecond
	var pending *csi.RetainedSnapshotDeletionPendingError

	first := NewDataUploadDeleteAction(velerotest.NewLogger(), crClient, resourceTimeout)
	require.ErrorAs(t, first.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, du), Backup: backup}), &pending)

	// A re-issued deletion is a new request, a new action.
	retry := NewDataUploadDeleteAction(velerotest.NewLogger(), crClient, resourceTimeout)
	require.ErrorAs(t, retry.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, du), Backup: backup}), &pending)
	assert.Equal(t, resourceTimeout, pending.Waited, "the content left in deletion is waited on again")

	sidecar.driverFails = false
	again := NewDataUploadDeleteAction(velerotest.NewLogger(), crClient, resourceTimeout)
	require.NoError(t, again.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, du), Backup: backup}))
	assert.False(t, sidecar.snapshots[deleteTestHandle])
}
