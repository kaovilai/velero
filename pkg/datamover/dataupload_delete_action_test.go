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

	"github.com/cockroachdb/errors"
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
	a := NewDataUploadDeleteAction(logrus.StandardLogger(), nil)
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
			action := NewDataUploadDeleteAction(logger, crClient)

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
// VolumeSnapshotContent of a retained snapshot: process() handles every
// content being deleted with a seeded handle -- DeleteSnapshot, which deletes
// the snapshot and drops the finalizer unless the driver fails -- as the
// sidecar does after the action returned.
type fakeSidecar struct {
	snapshots   map[string]bool
	driverFails bool
	deleteCalls []string
	createErr   error
	c           crclient.Client
}

func (f *fakeSidecar) client(t *testing.T, objs ...runtime.Object) crclient.Client {
	t.Helper()
	objs = append(objs, &storagev1api.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "rbd.csi.ceph.com"}})
	base := velerotest.NewFakeControllerRuntimeClientBuilder(t).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&snapshotv1api.VolumeSnapshotContent{}).
		Build()
	f.c = interceptor.NewClient(base.(crclient.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if _, ok := obj.(*snapshotv1api.VolumeSnapshotContent); ok && f.createErr != nil {
				return f.createErr
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	return f.c
}

func (f *fakeSidecar) process(t *testing.T) {
	t.Helper()
	contents := &snapshotv1api.VolumeSnapshotContentList{}
	require.NoError(t, f.c.List(t.Context(), contents))
	for i := range contents.Items {
		vsc := &contents.Items[i]
		if vsc.DeletionTimestamp == nil || vsc.Status == nil || vsc.Status.SnapshotHandle == nil {
			continue
		}
		f.deleteCalls = append(f.deleteCalls, *vsc.Status.SnapshotHandle)
		if f.driverFails {
			continue
		}
		f.snapshots[*vsc.Status.SnapshotHandle] = false
		vsc.Finalizers = nil
		require.NoError(t, f.c.Update(t.Context(), vsc))
	}
}

func retainedRecord(handle, driver string) *velerov2alpha1.RetainedCSIStorageSnapshot {
	return &velerov2alpha1.RetainedCSIStorageSnapshot{CSISnapshotHandle: handle, Driver: driver}
}

func staticContents(t *testing.T, c crclient.Client) []snapshotv1api.VolumeSnapshotContent {
	t.Helper()
	contents := &snapshotv1api.VolumeSnapshotContentList{}
	require.NoError(t, c.List(t.Context(), contents))
	return contents.Items
}

// TestDataUploadDeleteActionIssuesRetainedSnapshotDeletion proves the action
// issues the deletion of the snapshot the record of every DataUpload in the
// tarball names and returns without waiting: the static content is seeded and
// being deleted when it returns, and the sidecar then deletes the snapshot,
// also one already gone. No DataUpload in the cluster is read.
func TestDataUploadDeleteActionIssuesRetainedSnapshotDeletion(t *testing.T) {
	backup := builder.ForBackup("velero", "daily-backup").StorageLocation("default").Result()
	backup.UID = "daily-uid"

	tests := []struct {
		name       string
		tarball    func(*velerov2alpha1.DataUpload)
		local      bool
		gone       bool
		wantIssued bool
	}{
		{
			// Also the Completed DataUpload deleted through the API: the
			// tarball still carries its record.
			name: "a record from the tarball",
			tarball: func(du *velerov2alpha1.DataUpload) {
				du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
			},
			wantIssued: true,
		},
		{
			name: "a failed DataUpload's record",
			tarball: func(du *velerov2alpha1.DataUpload) {
				du.Status.Phase = velerov2alpha1.DataUploadPhaseFailed
				du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
			},
			wantIssued: true,
		},
		{
			name: "the local DataUpload is not read: its missing record changes nothing",
			tarball: func(du *velerov2alpha1.DataUpload) {
				du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
			},
			local:      true,
			wantIssued: true,
		},
		{
			name: "a snapshot already gone",
			tarball: func(du *velerov2alpha1.DataUpload) {
				du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
			},
			gone:       true,
			wantIssued: true,
		},
		{
			name:    "a DataUpload without a record",
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
			action := NewDataUploadDeleteAction(velerotest.NewLogger(), crClient)

			require.NoError(t, action.Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, tarball), Backup: backup}))

			contents := staticContents(t, crClient)
			if !tc.wantIssued {
				assert.Empty(t, contents)
				return
			}
			require.Len(t, contents, 1, "the static content is left to the sidecar")
			assert.NotNil(t, contents[0].DeletionTimestamp, "its deletion is issued")
			require.NotNil(t, contents[0].Status.SnapshotHandle)
			assert.Equal(t, deleteTestHandle, *contents[0].Status.SnapshotHandle, "seeded, so the sidecar calls DeleteSnapshot")
			sidecar.process(t)
			assert.Equal(t, []string{deleteTestHandle}, sidecar.deleteCalls)
			assert.False(t, sidecar.snapshots[deleteTestHandle])
			assert.Empty(t, staticContents(t, crClient), "no static content is left behind")
		})
	}
}

// TestDataUploadDeleteActionDoesNotWaitOnTheDriver proves the backup deletion
// does not wait for a snapshot the driver cannot delete: the action succeeds,
// and the static content stays in deletion carrying the handle -- the trace
// for manual cleanup. A re-issued deletion finds it in flight and succeeds.
func TestDataUploadDeleteActionDoesNotWaitOnTheDriver(t *testing.T) {
	backup := builder.ForBackup("velero", "daily-backup").StorageLocation("default").Result()
	backup.UID = "daily-uid"
	du := newCompletedDataUpload("daily-backup-aaaaa", "daily-backup")
	du.Spec.DataMover = "velero-block"
	du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
	sidecar := &fakeSidecar{snapshots: map[string]bool{deleteTestHandle: true}, driverFails: true}
	crClient := sidecar.client(t)

	require.NoError(t, NewDataUploadDeleteAction(velerotest.NewLogger(), crClient).Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, du), Backup: backup}))
	sidecar.process(t)
	contents := staticContents(t, crClient)
	require.Len(t, contents, 1)
	assert.NotNil(t, contents[0].DeletionTimestamp)
	require.NotNil(t, contents[0].Spec.Source.SnapshotHandle)
	assert.Equal(t, deleteTestHandle, *contents[0].Spec.Source.SnapshotHandle, "the content left in deletion names the snapshot")
	assert.True(t, sidecar.snapshots[deleteTestHandle])

	require.NoError(t, NewDataUploadDeleteAction(velerotest.NewLogger(), crClient).Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, du), Backup: backup}),
		"a re-issued deletion finds the request in flight")
	assert.Len(t, staticContents(t, crClient), 1)
}

// TestDataUploadDeleteActionIssueFailure proves a deletion that cannot be
// issued fails the action, so the backup deletion request fails before the
// tarball goes and a retry still has the handle.
func TestDataUploadDeleteActionIssueFailure(t *testing.T) {
	backup := builder.ForBackup("velero", "daily-backup").StorageLocation("default").Result()
	backup.UID = "daily-uid"
	du := newCompletedDataUpload("daily-backup-aaaaa", "daily-backup")
	du.Spec.DataMover = "velero-block"
	du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "rbd.csi.ceph.com")
	sidecar := &fakeSidecar{snapshots: map[string]bool{deleteTestHandle: true}, createErr: errors.New("the API server is unavailable")}
	crClient := sidecar.client(t)

	err := NewDataUploadDeleteAction(velerotest.NewLogger(), crClient).Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, du), Backup: backup})
	require.ErrorContains(t, err, "the API server is unavailable")
}

// TestDataUploadDeleteActionUnknownDriver proves a record naming a driver the
// cluster does not run is skipped with a warning: no static content is
// created and the deletion proceeds.
func TestDataUploadDeleteActionUnknownDriver(t *testing.T) {
	backup := builder.ForBackup("velero", "daily-backup").StorageLocation("default").Result()
	backup.UID = "daily-uid"
	du := newCompletedDataUpload("daily-backup-abcde", "daily-backup")
	du.Spec.DataMover = "velero-block"
	du.Status.RetainedCSIStorageSnapshot = retainedRecord(deleteTestHandle, "unknown.csi.example.com")
	sidecar := &fakeSidecar{snapshots: map[string]bool{deleteTestHandle: true}}
	crClient := sidecar.client(t)
	logger, hook := logrustest.NewNullLogger()

	require.NoError(t, NewDataUploadDeleteAction(logger, crClient).Execute(&velero.DeleteItemActionExecuteInput{Item: toUnstructured(t, du), Backup: backup}))
	assert.Empty(t, staticContents(t, crClient))
	warned := false
	for _, entry := range hook.AllEntries() {
		warned = warned || entry.Level == logrus.WarnLevel && strings.Contains(entry.Message, "unknown.csi.example.com")
	}
	assert.True(t, warned, "the skip is logged as a warning")
}
