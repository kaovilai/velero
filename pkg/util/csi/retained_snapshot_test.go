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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapshotFake "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientTesting "k8s.io/client-go/testing"
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

func TestPatchVSCDeletionPolicy(t *testing.T) {
	vscObj := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "fake-vsc",
			ResourceVersion: "999",
		},
		Spec: snapshotv1api.VolumeSnapshotContentSpec{
			DeletionPolicy: snapshotv1api.VolumeSnapshotContentRetain,
			Driver:         retainedTestDriver,
		},
	}

	tests := []struct {
		name            string
		clientObj       []runtime.Object
		vscName         string
		resourceVersion string
		patchErr        func(err error) bool
	}{
		{
			name:            "patch carries the resourceVersion under metadata",
			clientObj:       []runtime.Object{vscObj.DeepCopy()},
			vscName:         "fake-vsc",
			resourceVersion: "999",
		},
		{
			name:      "empty resourceVersion skips the precondition",
			clientObj: []runtime.Object{vscObj.DeepCopy()},
			vscName:   "fake-vsc",
		},
		{
			name:    "vsc not found",
			vscName: "missing-vsc",
			patchErr: func(err error) bool {
				return apierrors.IsNotFound(err)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeSnapshotClient := snapshotFake.NewSimpleClientset(tc.clientObj...)

			var patchBytes []byte
			fakeSnapshotClient.Fake.PrependReactor("patch", "volumesnapshotcontents", func(action clientTesting.Action) (bool, runtime.Object, error) {
				patchBytes = action.(clientTesting.PatchAction).GetPatch()
				return false, nil, nil
			})

			patched, err := PatchVSCDeletionPolicy(t.Context(), fakeSnapshotClient.SnapshotV1(), tc.vscName, snapshotv1api.VolumeSnapshotContentDelete, tc.resourceVersion)
			if tc.patchErr != nil {
				require.Error(t, err)
				assert.True(t, tc.patchErr(err), "unexpected error: %v", err)
				return
			}
			require.NoError(t, err)

			// The precondition must sit under metadata.resourceVersion: that is
			// the only placement the API server honors. A top-level
			// resourceVersion key is not part of the object's schema and is
			// silently dropped, which would make the patch unconditional.
			var patch map[string]any
			require.NoError(t, json.Unmarshal(patchBytes, &patch))
			_, topLevel := patch["resourceVersion"]
			assert.False(t, topLevel, "resourceVersion must not be a top-level patch key, patch: %s", patchBytes)
			if tc.resourceVersion != "" {
				metadata, ok := patch["metadata"].(map[string]any)
				require.True(t, ok, "patch must carry metadata.resourceVersion, patch: %s", patchBytes)
				assert.Equal(t, tc.resourceVersion, metadata["resourceVersion"])
			} else {
				assert.NotContains(t, string(patchBytes), "resourceVersion")
			}
			spec := patch["spec"].(map[string]any)
			assert.Equal(t, string(snapshotv1api.VolumeSnapshotContentDelete), spec["deletionPolicy"])

			// The post-write object is handed back with the policy applied, so
			// the caller's next preconditioned operation is built on it.
			require.NotNil(t, patched)
			assert.Equal(t, snapshotv1api.VolumeSnapshotContentDelete, patched.Spec.DeletionPolicy)
			assert.Equal(t, tc.vscName, patched.Name)
		})
	}
}

func TestSeedVSCStatusSnapshotHandle(t *testing.T) {
	vscWithHandle := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-vsc", ResourceVersion: "999"},
		Status: &snapshotv1api.VolumeSnapshotContentStatus{
			SnapshotHandle: ptr.To("existing-handle"),
		},
	}
	vscWithoutStatus := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-vsc", ResourceVersion: "999"},
	}

	tests := []struct {
		name        string
		clientObj   []runtime.Object
		reactors    []reactor
		vsc         *snapshotv1api.VolumeSnapshotContent
		handle      string
		wantHandle  string
		wantSameObj bool
		err         string
	}{
		{
			name:        "empty handle returns the object unchanged",
			vsc:         vscWithoutStatus.DeepCopy(),
			handle:      "",
			wantSameObj: true,
		},
		{
			name:        "handle already set returns the object unchanged",
			clientObj:   []runtime.Object{vscWithHandle.DeepCopy()},
			vsc:         vscWithHandle.DeepCopy(),
			handle:      "new-handle",
			wantSameObj: true,
		},
		{
			name:       "status write seeds the handle",
			clientObj:  []runtime.Object{vscWithoutStatus.DeepCopy()},
			vsc:        vscWithoutStatus.DeepCopy(),
			handle:     "new-handle",
			wantHandle: "new-handle",
		},
		{
			name: "conflict is resolved by the fresh status",
			clientObj: []runtime.Object{
				&snapshotv1api.VolumeSnapshotContent{
					ObjectMeta: metav1.ObjectMeta{Name: "fake-vsc", ResourceVersion: "1001"},
					Status: &snapshotv1api.VolumeSnapshotContentStatus{
						SnapshotHandle: ptr.To("sidecar-handle"),
					},
				},
			},
			reactors: []reactor{
				{
					verb:     "update",
					resource: "volumesnapshotcontents/status",
					reactorFunc: func(action clientTesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewConflict(
							snapshotv1api.Resource("volumesnapshotcontents"), "fake-vsc", errors.New("fake-conflict"))
					},
				},
			},
			vsc:        vscWithoutStatus.DeepCopy(),
			handle:     "new-handle",
			wantHandle: "sidecar-handle",
		},
		{
			name: "conflict without a handle in the fresh status fails",
			clientObj: []runtime.Object{
				&snapshotv1api.VolumeSnapshotContent{
					ObjectMeta: metav1.ObjectMeta{Name: "fake-vsc", ResourceVersion: "1001"},
				},
			},
			reactors: []reactor{
				{
					verb:     "update",
					resource: "volumesnapshotcontents/status",
					reactorFunc: func(action clientTesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewConflict(
							snapshotv1api.Resource("volumesnapshotcontents"), "fake-vsc", errors.New("fake-conflict"))
					},
				},
			},
			vsc:    vscWithoutStatus.DeepCopy(),
			handle: "new-handle",
			err:    "error writing the status of VolumeSnapshotContent after a conflict: Operation cannot be fulfilled on volumesnapshotcontents.snapshot.storage.k8s.io \"fake-vsc\": fake-conflict",
		},
		{
			name: "conflict and the re-read fails",
			reactors: []reactor{
				{
					verb:     "update",
					resource: "volumesnapshotcontents/status",
					reactorFunc: func(action clientTesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewConflict(
							snapshotv1api.Resource("volumesnapshotcontents"), "fake-vsc", errors.New("fake-conflict"))
					},
				},
				{
					verb:     "get",
					resource: "volumesnapshotcontents",
					reactorFunc: func(action clientTesting.Action) (bool, runtime.Object, error) {
						return true, nil, errors.New("fake-get-error")
					},
				},
			},
			vsc:    vscWithoutStatus.DeepCopy(),
			handle: "new-handle",
			err:    "error re-reading the status of the VolumeSnapshotContent after a conflict: fake-get-error",
		},
		{
			name: "content vanished before the write",
			reactors: []reactor{
				{
					verb:     "update",
					resource: "volumesnapshotcontents/status",
					reactorFunc: func(action clientTesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewNotFound(
							snapshotv1api.Resource("volumesnapshotcontents"), "fake-vsc")
					},
				},
			},
			vsc:    vscWithoutStatus.DeepCopy(),
			handle: "new-handle",
			err:    "error writing the status of VolumeSnapshotContent fake-vsc: volumesnapshotcontents.snapshot.storage.k8s.io \"fake-vsc\" not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeSnapshotClient := snapshotFake.NewSimpleClientset(tc.clientObj...)

			for _, reactor := range tc.reactors {
				fakeSnapshotClient.Fake.PrependReactor(reactor.verb, reactor.resource, reactor.reactorFunc)
			}

			result, err := SeedVSCStatusSnapshotHandle(t.Context(), fakeSnapshotClient.SnapshotV1(), tc.vsc, tc.handle)
			if tc.err != "" {
				assert.EqualError(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)

			if tc.wantSameObj {
				// Early return hands the passed object back unchanged, with no
				// server write at all.
				assert.Same(t, tc.vsc, result)
				assert.Empty(t, fakeSnapshotClient.Fake.Actions())
				return
			}

			require.NotNil(t, result.Status)
			require.NotNil(t, result.Status.SnapshotHandle)
			assert.Equal(t, tc.wantHandle, *result.Status.SnapshotHandle)

			// The server-held object carries the handle too.
			server, serr := fakeSnapshotClient.SnapshotV1().VolumeSnapshotContents().Get(
				t.Context(), "fake-vsc", metav1.GetOptions{})
			if assert.NoError(t, serr) {
				require.NotNil(t, server.Status)
				require.NotNil(t, server.Status.SnapshotHandle)
				assert.Equal(t, tc.wantHandle, *server.Status.SnapshotHandle)
			}
		})
	}
}

func TestDeleteVSWithResourceVersion(t *testing.T) {
	vsObj := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "fake-vs",
			Namespace:       "fake-ns",
			ResourceVersion: "999",
		},
	}

	tests := []struct {
		name            string
		clientObj       []runtime.Object
		reactors        []reactor
		resourceVersion string
		wantPrecond     bool
		err             string
	}{
		{
			name:            "delete carries the resourceVersion precondition",
			clientObj:       []runtime.Object{vsObj},
			resourceVersion: "999",
			wantPrecond:     true,
		},
		{
			name:        "empty resourceVersion deletes without a precondition",
			clientObj:   []runtime.Object{vsObj},
			wantPrecond: false,
		},
		{
			name: "not found is absorbed",
		},
		{
			name:      "other errors are returned",
			clientObj: []runtime.Object{vsObj},
			reactors: []reactor{
				{
					verb:     "delete",
					resource: "volumesnapshots",
					reactorFunc: func(action clientTesting.Action) (bool, runtime.Object, error) {
						return true, nil, errors.New("fake-delete-error")
					},
				},
			},
			err: "error deleting VolumeSnapshot fake-ns/fake-vs: fake-delete-error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeSnapshotClient := snapshotFake.NewSimpleClientset(tc.clientObj...)

			var deleteOptions metav1.DeleteOptions
			fakeSnapshotClient.Fake.PrependReactor("delete", "volumesnapshots", func(action clientTesting.Action) (bool, runtime.Object, error) {
				deleteOptions = action.(clientTesting.DeleteAction).GetDeleteOptions()
				return false, nil, nil
			})

			for _, reactor := range tc.reactors {
				fakeSnapshotClient.Fake.PrependReactor(reactor.verb, reactor.resource, reactor.reactorFunc)
			}

			err := DeleteVSWithResourceVersion(t.Context(), fakeSnapshotClient.SnapshotV1(), "fake-ns", "fake-vs", tc.resourceVersion)
			if tc.err != "" {
				assert.EqualError(t, err, tc.err)
				return
			}
			require.NoError(t, err)

			if tc.wantPrecond {
				require.NotNil(t, deleteOptions.Preconditions)
				require.NotNil(t, deleteOptions.Preconditions.ResourceVersion)
				assert.Equal(t, tc.resourceVersion, *deleteOptions.Preconditions.ResourceVersion)
			} else {
				assert.Nil(t, deleteOptions.Preconditions)
			}
		})
	}
}

func TestDeleteVSCWithResourceVersion(t *testing.T) {
	vscObj := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "fake-vsc",
			ResourceVersion: "999",
		},
	}

	tests := []struct {
		name            string
		clientObj       []runtime.Object
		reactors        []reactor
		resourceVersion string
		wantPrecond     bool
		err             string
	}{
		{
			name:            "delete carries the resourceVersion precondition",
			clientObj:       []runtime.Object{vscObj},
			resourceVersion: "999",
			wantPrecond:     true,
		},
		{
			name:        "empty resourceVersion deletes without a precondition",
			clientObj:   []runtime.Object{vscObj},
			wantPrecond: false,
		},
		{
			name: "not found is absorbed",
		},
		{
			name:      "other errors are returned",
			clientObj: []runtime.Object{vscObj},
			reactors: []reactor{
				{
					verb:     "delete",
					resource: "volumesnapshotcontents",
					reactorFunc: func(action clientTesting.Action) (bool, runtime.Object, error) {
						return true, nil, errors.New("fake-delete-error")
					},
				},
			},
			err: "error deleting VolumeSnapshotContent fake-vsc: fake-delete-error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeSnapshotClient := snapshotFake.NewSimpleClientset(tc.clientObj...)

			var deleteOptions metav1.DeleteOptions
			fakeSnapshotClient.Fake.PrependReactor("delete", "volumesnapshotcontents", func(action clientTesting.Action) (bool, runtime.Object, error) {
				deleteOptions = action.(clientTesting.DeleteAction).GetDeleteOptions()
				return false, nil, nil
			})

			for _, reactor := range tc.reactors {
				fakeSnapshotClient.Fake.PrependReactor(reactor.verb, reactor.resource, reactor.reactorFunc)
			}

			err := DeleteVSCWithResourceVersion(t.Context(), fakeSnapshotClient.SnapshotV1(), "fake-vsc", tc.resourceVersion)
			if tc.err != "" {
				assert.EqualError(t, err, tc.err)
				return
			}
			require.NoError(t, err)

			if tc.wantPrecond {
				require.NotNil(t, deleteOptions.Preconditions)
				require.NotNil(t, deleteOptions.Preconditions.ResourceVersion)
				assert.Equal(t, tc.resourceVersion, *deleteOptions.Preconditions.ResourceVersion)
			} else {
				assert.Nil(t, deleteOptions.Preconditions)
			}
		})
	}
}

// TestEnsureDeleteVSCTimeoutErrorIsTyped pins the timeout of EnsureDeleteVSC
// to its typed error, on top of the message-based cases of
// TestEnsureDeleteVSC: the DataUpload patch-back branches on the type.
func TestEnsureDeleteVSCTimeoutErrorIsTyped(t *testing.T) {
	vscObj := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "fake-vsc",
			Finalizers: []string{"fake-finalizer"},
		},
	}

	fakeSnapshotClient := snapshotFake.NewSimpleClientset(vscObj)
	// The delete is accepted but the content stays behind its finalizer, as on
	// a live cluster while the snapshotter has not released it yet.
	fakeSnapshotClient.Fake.PrependReactor("delete", "volumesnapshotcontents", func(action clientTesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	err := EnsureDeleteVSC(t.Context(), fakeSnapshotClient.SnapshotV1(), "fake-vsc", time.Millisecond)
	require.Error(t, err)

	var timeoutErr *VSCDeletionTimeoutError
	require.ErrorAs(t, err, &timeoutErr)
	assert.Equal(t, "fake-vsc", timeoutErr.VSCName)
	assert.True(t, timeoutErr.FinalizersRead)
	assert.Equal(t, []string{"fake-finalizer"}, timeoutErr.Finalizers)
}

// fakeSnapshotterBackend plays the CSI snapshotter sidecar and the storage
// for a static VolumeSnapshotContent, following external-snapshotter v8.6:
// every read of the content runs one sidecar sync. A content being deleted
// with policy Delete and a seeded handle has DeleteSnapshot called: a failing
// driver keeps the finalizer and retries, a working one deletes the snapshot
// (a gone one counts as deleted) and drops the finalizer, which makes the
// content disappear. Without a seeded handle the sidecar drops the finalizer
// with no DeleteSnapshot call, the silent leak the seed prevents.
type fakeSnapshotterBackend struct {
	// snapshots maps handle -> exists.
	snapshots map[string]bool
	// driverFails makes every DeleteSnapshot call fail.
	driverFails bool
	// deleteCallsTo records the handles DeleteSnapshot was called for.
	deleteCallsTo []string
	// silentDrops counts contents dropped without a DeleteSnapshot call.
	silentDrops int
	// reads counts the reads of static contents.
	reads int
}

func (b *fakeSnapshotterBackend) sync(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey) {
	vsc := &snapshotv1api.VolumeSnapshotContent{}
	if err := c.Get(ctx, key, vsc); err != nil || vsc.DeletionTimestamp == nil {
		return
	}
	if vsc.Spec.DeletionPolicy != snapshotv1api.VolumeSnapshotContentDelete || vsc.Status == nil || vsc.Status.SnapshotHandle == nil {
		b.silentDrops++
		vsc.Finalizers = nil
		_ = c.Update(ctx, vsc)
		return
	}
	b.deleteCallsTo = append(b.deleteCallsTo, *vsc.Status.SnapshotHandle)
	if b.driverFails {
		return
	}
	b.snapshots[*vsc.Status.SnapshotHandle] = false
	vsc.Finalizers = nil
	_ = c.Update(ctx, vsc)
}

func (b *fakeSnapshotterBackend) client(t *testing.T, objs ...runtime.Object) crclient.Client {
	t.Helper()
	fakeClient := velerotest.NewFakeControllerRuntimeClientBuilder(t).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&snapshotv1api.VolumeSnapshotContent{}).
		Build()
	return interceptor.NewClient(fakeClient.(crclient.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if _, ok := obj.(*snapshotv1api.VolumeSnapshotContent); ok {
				b.reads++
				b.sync(ctx, c, key)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

// shortenRetainedSnapshotWaits shortens the poll interval of a test and
// returns a short bound.
func shortenRetainedSnapshotWaits(t *testing.T) time.Duration {
	t.Helper()
	orig := retainedSnapshotPollInterval
	retainedSnapshotPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { retainedSnapshotPollInterval = orig })
	return 300 * time.Millisecond
}

func getTempVSC(t *testing.T, c crclient.Client, record *velerov2alpha1api.RetainedCSIStorageSnapshot) (*snapshotv1api.VolumeSnapshotContent, error) {
	t.Helper()
	vsc := &snapshotv1api.VolumeSnapshotContent{}
	err := c.Get(t.Context(), crclient.ObjectKey{Name: TempVSCName(record.Driver, record.CSISnapshotHandle)}, vsc)
	return vsc, err
}

// leaveDeleting runs a deletion against a failing driver, leaving the static
// content being deleted as an earlier attempt would.
func leaveDeleting(t *testing.T, backend *fakeSnapshotterBackend, c crclient.Client, record *velerov2alpha1api.RetainedCSIStorageSnapshot, timeout time.Duration) {
	t.Helper()
	backend.driverFails = true
	var pending *RetainedSnapshotDeletionPendingError
	require.ErrorAs(t, DeleteRetainedSnapshot(t.Context(), c, c, record, retainedTestBackupUID, RetainedSnapshotDeletionOptions{Timeout: timeout}, velerotest.NewLogger()), &pending)
	left, err := getTempVSC(t, c, record)
	require.NoError(t, err)
	require.NotNil(t, left.DeletionTimestamp)
}

// TestDeleteRetainedSnapshotSeedsThenDeletes proves the order: the handle is
// seeded from the record and confirmed by a read before the content is
// deleted, so the sidecar calls DeleteSnapshot -- also for a snapshot already
// gone, whose ListSnapshots check would keep the sidecar from seeding it -- and
// the content is gone.
func TestDeleteRetainedSnapshotSeedsThenDeletes(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(fmt.Sprintf("snapshot exists %t", exists), func(t *testing.T) {
			timeout := shortenRetainedSnapshotWaits(t)
			record := retainedTestRecord()
			backend := &fakeSnapshotterBackend{snapshots: map[string]bool{retainedTestHandle: exists, "other": true}}
			c := backend.client(t)

			require.NoError(t, DeleteRetainedSnapshot(t.Context(), c, c, record, retainedTestBackupUID, RetainedSnapshotDeletionOptions{Timeout: timeout}, velerotest.NewLogger()))
			assert.Equal(t, []string{retainedTestHandle}, backend.deleteCallsTo)
			assert.Zero(t, backend.silentDrops, "the content is never deleted without a seeded handle")
			assert.False(t, backend.snapshots[retainedTestHandle])
			assert.True(t, backend.snapshots["other"], "other snapshots stay")
			_, err := getTempVSC(t, c, record)
			assert.True(t, apierrors.IsNotFound(err))
		})
	}
}

// TestDeleteRetainedSnapshotReusesExisting proves AlreadyExists is reuse: a
// leftover content an interrupted attempt created but did not delete is seeded
// and deleted, not stacked with a second one.
func TestDeleteRetainedSnapshotReusesExisting(t *testing.T) {
	timeout := shortenRetainedSnapshotWaits(t)
	record := retainedTestRecord()
	leftover := BuildTempVSCForRetainedSnapshot(record, retainedTestBackupUID)
	backend := &fakeSnapshotterBackend{snapshots: map[string]bool{retainedTestHandle: true}}
	c := backend.client(t, leftover)

	require.NoError(t, DeleteRetainedSnapshot(t.Context(), c, c, record, retainedTestBackupUID, RetainedSnapshotDeletionOptions{Timeout: timeout}, velerotest.NewLogger()))
	assert.Equal(t, []string{retainedTestHandle}, backend.deleteCallsTo)
	_, err := getTempVSC(t, c, record)
	assert.True(t, apierrors.IsNotFound(err))
}

// TestDeleteRetainedSnapshotTimeoutLeavesContent proves the bound: a content
// still present returns the pending error and is left being deleted with its
// finalizer, so the sidecar keeps retrying; a later attempt finds it through
// the same name and completes once the driver recovers.
func TestDeleteRetainedSnapshotTimeoutLeavesContent(t *testing.T) {
	timeout := shortenRetainedSnapshotWaits(t)
	record := retainedTestRecord()
	backend := &fakeSnapshotterBackend{snapshots: map[string]bool{retainedTestHandle: true}}
	c := backend.client(t)

	leaveDeleting(t, backend, c, record, timeout)
	left, err := getTempVSC(t, c, record)
	require.NoError(t, err)
	assert.Contains(t, left.Finalizers, VSCBoundProtectionFinalizer)
	assert.True(t, backend.snapshots[retainedTestHandle])

	backend.driverFails = false
	require.NoError(t, DeleteRetainedSnapshot(t.Context(), c, c, record, retainedTestBackupUID, RetainedSnapshotDeletionOptions{Timeout: timeout}, velerotest.NewLogger()),
		"a retry resumes the deletion in progress")
	assert.False(t, backend.snapshots[retainedTestHandle])
}

// TestDeleteRetainedSnapshotChecksInProgressOnce proves the release's rule: a
// content an earlier attempt left being deleted is checked once and not waited
// on again, so a snapshot the driver cannot delete does not add the bound to
// every later release; a content gone meanwhile is success.
func TestDeleteRetainedSnapshotChecksInProgressOnce(t *testing.T) {
	timeout := shortenRetainedSnapshotWaits(t)
	record := retainedTestRecord()
	backend := &fakeSnapshotterBackend{snapshots: map[string]bool{retainedTestHandle: true}}
	c := backend.client(t)
	leaveDeleting(t, backend, c, record, timeout)

	backend.reads = 0
	start := time.Now()
	err := DeleteRetainedSnapshot(t.Context(), c, c, record, retainedTestBackupUID, RetainedSnapshotDeletionOptions{Timeout: time.Hour, CheckInProgressOnce: true}, velerotest.NewLogger())
	var pending *RetainedSnapshotDeletionPendingError
	require.ErrorAs(t, err, &pending)
	assert.Zero(t, pending.Waited, "the content in deletion is not waited on")
	assert.Less(t, time.Since(start), time.Second, "no wait up to the one-hour bound")
	assert.LessOrEqual(t, backend.reads, 2, "checked once")

	backend.driverFails = false
	require.NoError(t, DeleteRetainedSnapshot(t.Context(), c, c, record, retainedTestBackupUID, RetainedSnapshotDeletionOptions{Timeout: time.Hour, CheckInProgressOnce: true}, velerotest.NewLogger()),
		"a content the sidecar removed meanwhile is success")
}

// TestDeleteRetainedSnapshotNoWait proves the deletion request's rule after a
// first timeout: the content is created, seeded and deleted, checked once, and
// a content still present returns the pending error at once.
func TestDeleteRetainedSnapshotNoWait(t *testing.T) {
	shortenRetainedSnapshotWaits(t)
	record := retainedTestRecord()
	backend := &fakeSnapshotterBackend{snapshots: map[string]bool{retainedTestHandle: true}, driverFails: true}
	c := backend.client(t)

	start := time.Now()
	err := DeleteRetainedSnapshot(t.Context(), c, c, record, retainedTestBackupUID, RetainedSnapshotDeletionOptions{Timeout: time.Hour, NoWait: true}, velerotest.NewLogger())
	var pending *RetainedSnapshotDeletionPendingError
	require.ErrorAs(t, err, &pending)
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, []string{retainedTestHandle}, backend.deleteCallsTo, "the deletion is requested; the sidecar works on it in the background")
	left, getErr := getTempVSC(t, c, record)
	require.NoError(t, getErr)
	assert.NotNil(t, left.DeletionTimestamp)
}

// TestDeleteRetainedSnapshotInvalidRecord proves a record without a handle or
// a driver is refused before any object is created.
func TestDeleteRetainedSnapshotInvalidRecord(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	for _, record := range []*velerov2alpha1api.RetainedCSIStorageSnapshot{nil, {Driver: "d"}, {CSISnapshotHandle: "h"}} {
		require.Error(t, DeleteRetainedSnapshot(t.Context(), c, c, record, retainedTestBackupUID, RetainedSnapshotDeletionOptions{Timeout: time.Second}, velerotest.NewLogger()))
	}
	list := &snapshotv1api.VolumeSnapshotContentList{}
	require.NoError(t, c.List(t.Context(), list))
	assert.Empty(t, list.Items)
}
