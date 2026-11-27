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
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/kube"
)

const (
	retainedTestDriver = "rbd.csi.ceph.com"
	retainedTestHandle = "0001-0009-rook-ceph-0000000000000001-snap"
	// retainedTestBackupUID is the backup UID the temp content is labeled with.
	retainedTestBackupUID types.UID = "3f6d2a3c-0000-4000-8000-000000000001"
)

func retainedTestRecord() *velerov2alpha1api.RetainedCSIStorageSnapshot {
	return &velerov2alpha1api.RetainedCSIStorageSnapshot{
		CSISnapshotHandle: retainedTestHandle,
		Driver:            retainedTestDriver,
	}
}

func TestGetRetainedSnapshotFromVSC(t *testing.T) {
	tests := []struct {
		name string
		vsc  *snapshotv1api.VolumeSnapshotContent
		want *velerov2alpha1api.RetainedCSIStorageSnapshot
	}{
		{
			name: "nil content",
			vsc:  nil,
			want: nil,
		},
		{
			name: "no handle",
			vsc: &snapshotv1api.VolumeSnapshotContent{
				Spec: snapshotv1api.VolumeSnapshotContentSpec{
					Driver: retainedTestDriver,
				},
			},
			want: nil,
		},
		{
			name: "no driver",
			vsc: &snapshotv1api.VolumeSnapshotContent{
				Spec: snapshotv1api.VolumeSnapshotContentSpec{
					Source: snapshotv1api.VolumeSnapshotContentSource{
						SnapshotHandle: ptr.To(retainedTestHandle),
					},
				},
			},
			want: nil,
		},
		{
			name: "handle from spec",
			vsc: &snapshotv1api.VolumeSnapshotContent{
				Spec: snapshotv1api.VolumeSnapshotContentSpec{
					Driver: retainedTestDriver,
					Source: snapshotv1api.VolumeSnapshotContentSource{
						SnapshotHandle: ptr.To(retainedTestHandle),
					},
				},
			},
			want: &velerov2alpha1api.RetainedCSIStorageSnapshot{
				CSISnapshotHandle: retainedTestHandle,
				Driver:            retainedTestDriver,
			},
		},
		{
			name: "empty spec handle falls back to status handle",
			vsc: &snapshotv1api.VolumeSnapshotContent{
				Spec: snapshotv1api.VolumeSnapshotContentSpec{
					Driver: retainedTestDriver,
				},
				Status: &snapshotv1api.VolumeSnapshotContentStatus{
					SnapshotHandle: ptr.To(retainedTestHandle),
				},
			},
			want: &velerov2alpha1api.RetainedCSIStorageSnapshot{
				CSISnapshotHandle: retainedTestHandle,
				Driver:            retainedTestDriver,
			},
		},
		{
			name: "spec handle wins over status handle",
			vsc: &snapshotv1api.VolumeSnapshotContent{
				Spec: snapshotv1api.VolumeSnapshotContentSpec{
					Driver: retainedTestDriver,
					Source: snapshotv1api.VolumeSnapshotContentSource{
						SnapshotHandle: ptr.To("spec-handle"),
					},
				},
				Status: &snapshotv1api.VolumeSnapshotContentStatus{
					SnapshotHandle: ptr.To("status-handle"),
				},
			},
			want: &velerov2alpha1api.RetainedCSIStorageSnapshot{
				CSISnapshotHandle: "spec-handle",
				Driver:            retainedTestDriver,
			},
		},
		{
			name: "labels and secret annotations are carried",
			vsc: &snapshotv1api.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						kube.VolumeSnapshotContentManagedByLabel: "velero",
						"unrelated":                              "label",
					},
					Annotations: map[string]string{
						VSCDeletionSecretNameAnnotation:      "secret-name",
						VSCDeletionSecretNamespaceAnnotation: "secret-ns",
					},
				},
				Spec: snapshotv1api.VolumeSnapshotContentSpec{
					Driver: retainedTestDriver,
					Source: snapshotv1api.VolumeSnapshotContentSource{
						SnapshotHandle: ptr.To(retainedTestHandle),
					},
				},
			},
			want: &velerov2alpha1api.RetainedCSIStorageSnapshot{
				CSISnapshotHandle:   retainedTestHandle,
				Driver:              retainedTestDriver,
				ManagedByLabelValue: "velero",
				DeletionSecretRef: &corev1api.SecretReference{
					Name:      "secret-name",
					Namespace: "secret-ns",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := GetRetainedSnapshotFromVSC(tc.vsc)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.want.CSISnapshotHandle, got.CSISnapshotHandle)
			assert.Equal(t, tc.want.Driver, got.Driver)
			assert.Equal(t, tc.want.ManagedByLabelValue, got.ManagedByLabelValue)
			assert.Equal(t, tc.want.DeletionSecretRef, got.DeletionSecretRef)
		})
	}
}

func TestTempVSCName(t *testing.T) {
	name := TempVSCName(retainedTestDriver, retainedTestHandle)

	// Deterministic: a retry of an interrupted deletion derives the same name
	// and reuses the leftover content.
	assert.Equal(t, name, TempVSCName(retainedTestDriver, retainedTestHandle))
	assert.True(t, strings.HasPrefix(name, "vsc-"))

	// Different driver or handle must produce a different content name.
	assert.NotEqual(t, name, TempVSCName("other.driver", retainedTestHandle))
	assert.NotEqual(t, name, TempVSCName(retainedTestDriver, "other-handle"))
}

func TestBuildTempVSCForRetainedSnapshot(t *testing.T) {
	record := retainedTestRecord()
	record.ManagedByLabelValue = "velero"
	record.DeletionSecretRef = &corev1api.SecretReference{Name: "secret-name", Namespace: "secret-ns"}

	vsc := BuildTempVSCForRetainedSnapshot(record, retainedTestBackupUID)

	assert.Equal(t, TempVSCName(record.Driver, record.CSISnapshotHandle), vsc.Name)
	assert.Equal(t, []string{VSCBoundProtectionFinalizer}, vsc.Finalizers)
	assert.Equal(t, snapshotv1api.VolumeSnapshotContentDelete, vsc.Spec.DeletionPolicy)
	assert.Equal(t, record.Driver, vsc.Spec.Driver)
	require.NotNil(t, vsc.Spec.Source.SnapshotHandle)
	assert.Equal(t, record.CSISnapshotHandle, *vsc.Spec.Source.SnapshotHandle)
	assert.Nil(t, vsc.Spec.VolumeSnapshotClassName)

	// The synthetic reference must carry an empty uid: that routes the content
	// into the sidecar's unbound pre-provisioned deletion path.
	assert.Empty(t, vsc.Spec.VolumeSnapshotRef.UID)
	assert.Equal(t, snapshotv1api.SchemeGroupVersion.String(), vsc.Spec.VolumeSnapshotRef.APIVersion)
	assert.Equal(t, "VolumeSnapshot", vsc.Spec.VolumeSnapshotRef.Kind)
	assert.NotEmpty(t, vsc.Spec.VolumeSnapshotRef.Namespace)
	assert.NotEmpty(t, vsc.Spec.VolumeSnapshotRef.Name)

	// The backup UID label maps the content's delete event back to the
	// pending DeleteBackupRequest; the managed-by label selects the content
	// for a distributed snapshotter.
	assert.Equal(t, map[string]string{
		velerov1api.BackupUIDLabel:               string(retainedTestBackupUID),
		kube.VolumeSnapshotContentManagedByLabel: "velero",
	}, vsc.Labels)
	assert.Equal(t, map[string]string{
		VSCDeletionSecretNameAnnotation:      "secret-name",
		VSCDeletionSecretNamespaceAnnotation: "secret-ns",
	}, vsc.Annotations)

	// A minimal record carries only the backup UID label and no annotations.
	minimal := BuildTempVSCForRetainedSnapshot(retainedTestRecord(), retainedTestBackupUID)
	assert.Equal(t, map[string]string{velerov1api.BackupUIDLabel: string(retainedTestBackupUID)}, minimal.Labels)
	assert.Nil(t, minimal.Annotations)
}

// statusClient builds a client whose static contents have a status
// subresource; conflicts makes the first status writes fail with a conflict,
// as when the sidecar writes the status concurrently.
func statusClient(t *testing.T, conflicts int, objs ...runtime.Object) crclient.Client {
	t.Helper()
	base := velerotest.NewFakeControllerRuntimeClientBuilder(t).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&snapshotv1api.VolumeSnapshotContent{}).
		Build()
	return interceptor.NewClient(base.(crclient.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c crclient.Client, subResourceName string, obj crclient.Object, opts ...crclient.SubResourceUpdateOption) error {
			if conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(snapshotv1api.Resource("volumesnapshotcontents"), obj.GetName(), errors.New("the sidecar wrote first"))
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
}

func getTempVSC(t *testing.T, c crclient.Client, record *velerov2alpha1api.RetainedCSIStorageSnapshot) (*snapshotv1api.VolumeSnapshotContent, error) {
	t.Helper()
	vsc := &snapshotv1api.VolumeSnapshotContent{}
	err := c.Get(t.Context(), crclient.ObjectKey{Name: TempVSCName(record.Driver, record.CSISnapshotHandle)}, vsc)
	return vsc, err
}

// TestDeleteRetainedSnapshotIssuesWithoutWaiting proves the helper creates the
// static content, seeds status.snapshotHandle and deletes it, and returns
// without waiting for the CSI driver: the content is left being deleted with
// its handle seeded -- so the sidecar calls DeleteSnapshot -- and its finalizer.
// A status conflict, the sidecar writing first, is retried.
func TestDeleteRetainedSnapshotIssuesWithoutWaiting(t *testing.T) {
	for _, conflicts := range []int{0, 1} {
		t.Run(fmt.Sprintf("status conflicts %d", conflicts), func(t *testing.T) {
			record := retainedTestRecord()
			c := statusClient(t, conflicts)

			require.NoError(t, DeleteRetainedSnapshot(t.Context(), c, record, retainedTestBackupUID, velerotest.NewLogger()))
			left, err := getTempVSC(t, c, record)
			require.NoError(t, err, "the content is left to the sidecar")
			assert.NotNil(t, left.DeletionTimestamp)
			assert.Contains(t, left.Finalizers, VSCBoundProtectionFinalizer)
			require.NotNil(t, left.Status)
			require.NotNil(t, left.Status.SnapshotHandle)
			assert.Equal(t, retainedTestHandle, *left.Status.SnapshotHandle)
			assert.Equal(t, snapshotv1api.VolumeSnapshotContentDelete, left.Spec.DeletionPolicy)
		})
	}
}

// TestDeleteRetainedSnapshotInFlight proves a content an earlier request left
// being deleted means the deletion is in flight: it is treated as issued and
// left alone. One an earlier request created but did not get to delete is
// seeded and deleted.
func TestDeleteRetainedSnapshotInFlight(t *testing.T) {
	record := retainedTestRecord()

	t.Run("being deleted", func(t *testing.T) {
		c := statusClient(t, 0)
		require.NoError(t, DeleteRetainedSnapshot(t.Context(), c, record, retainedTestBackupUID, velerotest.NewLogger()))
		before, err := getTempVSC(t, c, record)
		require.NoError(t, err)
		require.NoError(t, DeleteRetainedSnapshot(t.Context(), c, record, retainedTestBackupUID, velerotest.NewLogger()))
		after, err := getTempVSC(t, c, record)
		require.NoError(t, err)
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "left alone")
	})

	t.Run("created but not deleted", func(t *testing.T) {
		leftover := BuildTempVSCForRetainedSnapshot(record, retainedTestBackupUID)
		c := statusClient(t, 0, leftover)
		require.NoError(t, DeleteRetainedSnapshot(t.Context(), c, record, retainedTestBackupUID, velerotest.NewLogger()))
		left, err := getTempVSC(t, c, record)
		require.NoError(t, err)
		assert.NotNil(t, left.DeletionTimestamp)
		require.NotNil(t, left.Status.SnapshotHandle)
		assert.Equal(t, retainedTestHandle, *left.Status.SnapshotHandle)
	})
}

// TestDeleteRetainedSnapshotNotIssued proves an API error creating, seeding or
// deleting the static content is returned: the deletion was not issued.
func TestDeleteRetainedSnapshotNotIssued(t *testing.T) {
	record := retainedTestRecord()
	for name, funcs := range map[string]interceptor.Funcs{
		"create": {Create: func(context.Context, crclient.WithWatch, crclient.Object, ...crclient.CreateOption) error {
			return errors.New("create refused")
		}},
		"seed": {SubResourceUpdate: func(context.Context, crclient.Client, string, crclient.Object, ...crclient.SubResourceUpdateOption) error {
			return errors.New("status refused")
		}},
		"delete": {Delete: func(context.Context, crclient.WithWatch, crclient.Object, ...crclient.DeleteOption) error {
			return errors.New("delete refused")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			base := velerotest.NewFakeControllerRuntimeClientBuilder(t).WithStatusSubresource(&snapshotv1api.VolumeSnapshotContent{}).Build()
			c := interceptor.NewClient(base.(crclient.WithWatch), funcs)
			require.ErrorContains(t, DeleteRetainedSnapshot(t.Context(), c, record, retainedTestBackupUID, velerotest.NewLogger()), "refused")
		})
	}
}

// TestDeleteRetainedSnapshotInvalidRecord proves a record without a handle or
// a driver is refused before any object is created.
func TestDeleteRetainedSnapshotInvalidRecord(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	for _, record := range []*velerov2alpha1api.RetainedCSIStorageSnapshot{nil, {Driver: "d"}, {CSISnapshotHandle: "h"}} {
		require.Error(t, DeleteRetainedSnapshot(t.Context(), c, record, retainedTestBackupUID, velerotest.NewLogger()))
	}
	list := &snapshotv1api.VolumeSnapshotContentList{}
	require.NoError(t, c.List(t.Context(), list))
	assert.Empty(t, list.Items)
}
