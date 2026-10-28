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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/retainedsnapshot"
	velerotest "github.com/velero-io/velero/pkg/test"
)

func rotationDataUpload(name string, phase velerov2alpha1api.DataUploadPhase, start time.Time, mover string) *velerov2alpha1api.DataUpload {
	du := builder.ForDataUpload(velerov1api.DefaultNamespace, name).
		Labels(map[string]string{velerov1api.BackupNameLabel: name}).
		SnapshotType(velerov2alpha1api.SnapshotTypeCSI).
		BackupStorageLocation("bsl").
		SourceNamespace("app").
		SourcePVC("data").
		DataMover(mover).
		Phase(phase).
		Result()
	du.Status.StartTimestamp = &metav1.Time{Time: start}
	return du
}

func rotationRecord(du *velerov2alpha1api.DataUpload, handle string) *corev1api.ConfigMap {
	record := retainedsnapshot.NewRecordForDataUpload(du, &velerov2alpha1api.RetainedSnapshot{SnapshotHandle: handle, Driver: lifecycleDriver}, "", "repo-"+du.Name)
	return record.ToConfigMap(du.Namespace)
}

func TestRetainedSnapshotRotation(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		dus  []*velerov2alpha1api.DataUpload
		// records maps DataUpload name to snapshot handle.
		records       map[string]string
		wantDeleted   []string
		wantTombstone []string
		wantRemoved   []string
		wantKept      []string
	}{
		{
			name: "the newer completed DataUpload rotates out the older snapshot",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", velerov2alpha1api.DataUploadPhaseCompleted, t0, "velero-block"),
				rotationDataUpload("b2", velerov2alpha1api.DataUploadPhaseCompleted, t0.Add(time.Hour), "velero-block"),
			},
			records:       map[string]string{"b1": "snap-1", "b2": "snap-2"},
			wantDeleted:   []string{"snap-1"},
			wantTombstone: []string{"b1"},
			wantKept:      []string{"b2"},
		},
		{
			name: "a newer DataUpload that didn't retain still rotates out the older snapshot",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", velerov2alpha1api.DataUploadPhaseCompleted, t0, "velero-block"),
				rotationDataUpload("b2", velerov2alpha1api.DataUploadPhaseCompleted, t0.Add(time.Hour), "velero-block"),
			},
			records:       map[string]string{"b1": "snap-1"},
			wantDeleted:   []string{"snap-1"},
			wantTombstone: []string{"b1"},
		},
		{
			name: "an in-progress newer DataUpload doesn't rotate",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", velerov2alpha1api.DataUploadPhaseCompleted, t0, "velero-block"),
				rotationDataUpload("b2", velerov2alpha1api.DataUploadPhaseInProgress, t0.Add(time.Hour), "velero-block"),
			},
			records:  map[string]string{"b1": "snap-1"},
			wantKept: []string{"b1"},
		},
		{
			name: "a newer file system DataUpload doesn't rotate",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", velerov2alpha1api.DataUploadPhaseCompleted, t0, "velero-block"),
				rotationDataUpload("b2", velerov2alpha1api.DataUploadPhaseCompleted, t0.Add(time.Hour), "velero"),
			},
			records:  map[string]string{"b1": "snap-1"},
			wantKept: []string{"b1"},
		},
		{
			name: "the record of a failed DataUpload is removed with its snapshot",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", velerov2alpha1api.DataUploadPhaseCompleted, t0, "velero-block"),
				rotationDataUpload("b2", velerov2alpha1api.DataUploadPhaseFailed, t0.Add(time.Hour), "velero-block"),
			},
			records:     map[string]string{"b1": "snap-1", "b2": "snap-2"},
			wantDeleted: []string{"snap-2"},
			wantRemoved: []string{"b2"},
			wantKept:    []string{"b1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortenTempVSCStatusWait(t)
			backend := &fakeCSIBackend{snapshots: map[string]bool{}}
			for _, h := range tc.records {
				backend.snapshots[h] = true
			}
			c := backend.client(t)
			for _, du := range tc.dus {
				require.NoError(t, c.Create(t.Context(), du))
				if h, ok := tc.records[du.Name]; ok {
					require.NoError(t, c.Create(t.Context(), rotationRecord(du, h)))
				}
			}

			r := NewRetainedSnapshotReconciler(c, velerov1api.DefaultNamespace, velerotest.NewLogger())
			_, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: velerov1api.DefaultNamespace, Name: retainedsnapshot.ChainHash("bsl", "app", "data"),
			}})
			require.NoError(t, err)

			assert.ElementsMatch(t, tc.wantDeleted, backend.deleteCallsTo)
			get := func(du string) (*corev1api.ConfigMap, error) {
				cm := &corev1api.ConfigMap{}
				err := c.Get(t.Context(), client.ObjectKey{Namespace: velerov1api.DefaultNamespace, Name: retainedsnapshot.RecordName(du)}, cm)
				return cm, err
			}
			for _, du := range tc.wantTombstone {
				cm, err := get(du)
				require.NoError(t, err)
				assert.Equal(t, retainedsnapshot.StateDeleted, cm.Labels[retainedsnapshot.StateLabel], du)
			}
			for _, du := range tc.wantRemoved {
				_, err := get(du)
				assert.True(t, apierrors.IsNotFound(err), du)
			}
			for _, du := range tc.wantKept {
				cm, err := get(du)
				require.NoError(t, err)
				assert.Equal(t, retainedsnapshot.StateRetained, cm.Labels[retainedsnapshot.StateLabel], du)
				assert.True(t, backend.snapshots[tc.records[du]], du)
			}
		})
	}
}
