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

	testclocks "k8s.io/utils/clock/testing"

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
	du.Labels[retainedsnapshot.ChainLabel] = retainedsnapshot.ChainOfDataUpload(du)
	// The DataUpload start timestamp is deliberately the reverse of the
	// repository snapshot order, which is the one rotation follows.
	du.Status.StartTimestamp = &metav1.Time{Time: start.Add(-time.Duration(start.Unix()%7) * time.Hour)}
	if phase == velerov2alpha1api.DataUploadPhaseCompleted {
		du.Status.SnapshotStartTime = &metav1.MicroTime{Time: start}
	}
	return du
}

func rotationRecord(du *velerov2alpha1api.DataUpload, handle string, start time.Time) *corev1api.ConfigMap {
	record := retainedsnapshot.NewRecordForDataUpload(du, &velerov2alpha1api.RetainedSnapshot{SnapshotHandle: handle, Driver: lifecycleDriver}, "repo-"+du.Name, start)
	return record.ToConfigMap(du.Namespace)
}

func TestRetainedSnapshotRotation(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	completed := velerov2alpha1api.DataUploadPhaseCompleted
	tests := []struct {
		name string
		dus  []*velerov2alpha1api.DataUpload
		// records maps DataUpload name to snapshot handle; the record start time
		// is the DataUpload's repository snapshot start time, or starts[name].
		records       map[string]string
		starts        map[string]time.Time
		backups       []string
		dds           []*velerov2alpha1api.DataDownload
		wantDeleted   []string
		wantTombstone map[string]string
		wantKept      []string
		wantRequeue   bool
	}{
		{
			name: "the later repository snapshot rotates out the earlier one",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", completed, t0, "velero-block"),
				rotationDataUpload("b2", completed, t0.Add(500*time.Millisecond), "velero-block"),
			},
			records:       map[string]string{"b1": "snap-1", "b2": "snap-2"},
			wantDeleted:   []string{"snap-1"},
			wantTombstone: map[string]string{"b1": retainedsnapshot.ReasonRotated},
			wantKept:      []string{"b2"},
		},
		{
			name: "a later DataUpload that didn't retain still rotates out the earlier snapshot",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", completed, t0, "velero-block"),
				rotationDataUpload("b2", completed, t0.Add(time.Hour), "velero-block"),
			},
			records:       map[string]string{"b1": "snap-1"},
			wantDeleted:   []string{"snap-1"},
			wantTombstone: map[string]string{"b1": retainedsnapshot.ReasonRotated},
		},
		{
			name: "equal repository snapshot start times keep both",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", completed, t0, "velero-block"),
				rotationDataUpload("b2", completed, t0, "velero-block"),
			},
			records:  map[string]string{"b1": "snap-1", "b2": "snap-2"},
			wantKept: []string{"b1", "b2"},
		},
		{
			name: "a running DataUpload of the chain pauses rotation",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", completed, t0, "velero-block"),
				rotationDataUpload("b2", completed, t0.Add(time.Hour), "velero-block"),
				rotationDataUpload("b3", velerov2alpha1api.DataUploadPhaseInProgress, t0.Add(2*time.Hour), "velero-block"),
			},
			records:     map[string]string{"b1": "snap-1", "b2": "snap-2"},
			wantKept:    []string{"b1", "b2"},
			wantRequeue: true,
		},
		{
			name: "a running in-place DataDownload of the chain pauses rotation",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", completed, t0, "velero-block"),
				rotationDataUpload("b2", completed, t0.Add(time.Hour), "velero-block"),
			},
			dds: []*velerov2alpha1api.DataDownload{
				builder.ForDataDownload(velerov1api.DefaultNamespace, "r1").Labels(map[string]string{retainedsnapshot.ChainLabel: retainedsnapshot.ChainHash("bsl", "app", "data")}).Phase(velerov2alpha1api.DataDownloadPhaseInProgress).Result(),
			},
			records:     map[string]string{"b1": "snap-1", "b2": "snap-2"},
			wantKept:    []string{"b1", "b2"},
			wantRequeue: true,
		},
		{
			name: "the record of a failed DataUpload becomes a tombstone with its snapshot deleted",
			dus: []*velerov2alpha1api.DataUpload{
				rotationDataUpload("b1", completed, t0, "velero-block"),
				rotationDataUpload("b2", velerov2alpha1api.DataUploadPhaseFailed, t0.Add(time.Hour), "velero-block"),
			},
			records:       map[string]string{"b1": "snap-1", "b2": "snap-2"},
			starts:        map[string]time.Time{"b2": t0.Add(time.Hour)},
			wantDeleted:   []string{"snap-2"},
			wantTombstone: map[string]string{"b2": retainedsnapshot.ReasonDataUploadFailed},
			wantKept:      []string{"b1"},
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
				if h, ok := tc.records[du.Name]; ok {
					du.Status.RetainedSnapshot = &velerov2alpha1api.RetainedSnapshot{SnapshotHandle: h, Driver: lifecycleDriver}
				}
				require.NoError(t, c.Create(t.Context(), du))
				require.NoError(t, c.Create(t.Context(), builder.ForBackup(velerov1api.DefaultNamespace, du.Name).Result()))
				if h, ok := tc.records[du.Name]; ok {
					start := tc.starts[du.Name]
					if du.Status.SnapshotStartTime != nil {
						start = du.Status.SnapshotStartTime.Time
					}
					require.NoError(t, c.Create(t.Context(), rotationRecord(du, h, start)))
				}
			}
			for _, dd := range tc.dds {
				require.NoError(t, c.Create(t.Context(), dd))
			}

			r := NewRetainedSnapshotReconciler(c, velerov1api.DefaultNamespace, velerotest.NewLogger())
			result, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: velerov1api.DefaultNamespace, Name: retainedsnapshot.ChainHash("bsl", "app", "data"),
			}})
			require.NoError(t, err)
			assert.Equal(t, tc.wantRequeue, result.RequeueAfter > 0)

			assert.ElementsMatch(t, tc.wantDeleted, backend.deleteCallsTo)
			get := func(du string) *retainedsnapshot.Record {
				cm := &corev1api.ConfigMap{}
				require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: velerov1api.DefaultNamespace, Name: retainedsnapshot.RecordName(du)}, cm))
				rec, err := retainedsnapshot.FromConfigMap(cm)
				require.NoError(t, err)
				return rec
			}
			for du, reason := range tc.wantTombstone {
				rec := get(du)
				assert.Equal(t, retainedsnapshot.StateDeleted, rec.State, du)
				assert.Equal(t, reason, rec.Reason, du)
				assert.False(t, rec.DeletedAt.IsZero(), du)
				if reason == retainedsnapshot.ReasonRotated {
					assert.NotEmpty(t, rec.RotatedByDataUpload, du)
					assert.NotEmpty(t, rec.RotatedByBackup, du)
				}
			}
			for _, du := range tc.wantKept {
				assert.Equal(t, retainedsnapshot.StateRetained, get(du).State, du)
				assert.True(t, backend.snapshots[tc.records[du]], du)
			}

			// A second reconcile on the stored state deletes nothing again.
			before := len(backend.deleteCallsTo)
			_, err = r.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: velerov1api.DefaultNamespace, Name: retainedsnapshot.ChainHash("bsl", "app", "data"),
			}})
			require.NoError(t, err)
			assert.Len(t, backend.deleteCallsTo, before)
		})
	}
}

// TestRetainedSnapshotStaleRead checks that a writer acting on a stale copy of a
// record loses the conditional update and doesn't delete the snapshot.
func TestRetainedSnapshotStaleRead(t *testing.T) {
	shortenTempVSCStatusWait(t)
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	backend := &fakeCSIBackend{snapshots: map[string]bool{"snap-1": true}}
	c := backend.client(t)
	du := rotationDataUpload("b1", velerov2alpha1api.DataUploadPhaseCompleted, t0, "velero-block")
	require.NoError(t, c.Create(t.Context(), du))
	require.NoError(t, c.Create(t.Context(), rotationRecord(du, "snap-1", t0)))

	stale := &corev1api.ConfigMap{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: du.Namespace, Name: retainedsnapshot.RecordName(du.Name)}, stale))
	staleCopy := stale.DeepCopy()

	opts := retainedsnapshot.DeleteOptions{Reason: retainedsnapshot.ReasonRotated, Now: time.Now, Wait: time.Second}
	require.NoError(t, retainedsnapshot.DeleteSnapshot(t.Context(), c, c, stale, opts, velerotest.NewLogger()))
	require.Equal(t, []string{"snap-1"}, backend.deleteCallsTo)

	err := retainedsnapshot.DeleteSnapshot(t.Context(), c, c, staleCopy, opts, velerotest.NewLogger())
	require.ErrorIs(t, err, retainedsnapshot.ErrConflict)
	assert.Equal(t, []string{"snap-1"}, backend.deleteCallsTo)
}

// TestRetainedSnapshotBackupGone checks the GC of records whose Backup is gone:
// nothing happens within the grace period, then the snapshot is deleted and
// the record removed.
func TestRetainedSnapshotBackupGone(t *testing.T) {
	shortenTempVSCStatusWait(t)
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	backend := &fakeCSIBackend{snapshots: map[string]bool{"snap-1": true}}
	c := backend.client(t)
	du := rotationDataUpload("b1", velerov2alpha1api.DataUploadPhaseCompleted, t0, "velero-block")
	du.Status.RetainedSnapshot = &velerov2alpha1api.RetainedSnapshot{SnapshotHandle: "snap-1", Driver: lifecycleDriver}
	require.NoError(t, c.Create(t.Context(), du))
	require.NoError(t, c.Create(t.Context(), rotationRecord(du, "snap-1", t0)))

	r := NewRetainedSnapshotReconciler(c, velerov1api.DefaultNamespace, velerotest.NewLogger())
	fakeClock := testclocks.NewFakeClock(t0)
	r.clock = fakeClock
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: velerov1api.DefaultNamespace, Name: retainedsnapshot.ChainHash("bsl", "app", "data")}}

	result, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, defaultGCGracePeriod, result.RequeueAfter)
	assert.Empty(t, backend.deleteCallsTo)

	fakeClock.Step(defaultGCGracePeriod + time.Second)
	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, []string{"snap-1"}, backend.deleteCallsTo)
	err = c.Get(t.Context(), client.ObjectKey{Namespace: du.Namespace, Name: retainedsnapshot.RecordName(du.Name)}, &corev1api.ConfigMap{})
	assert.True(t, apierrors.IsNotFound(err))
}
