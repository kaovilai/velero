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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/csi"
	retainedsnapshot "github.com/velero-io/velero/pkg/util/retainedsnapshot"
)

// chainDataUpload returns a completed DataUpload of backup <name>-uid whose
// record belongs to the chain of driver retainedTestDriver and the given
// volume handle, completed at the given offset from now.
func chainDataUpload(name, pvcUID, volumeHandle, snapshotHandle string, completedAgo time.Duration) *velerov2alpha1api.DataUpload {
	done := metav1.NewTime(time.Now().Add(-completedAgo))
	return &velerov2alpha1api.DataUpload{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: velerov1api.DefaultNamespace,
			Name:      name,
			Labels: map[string]string{
				velerov1api.PVCUIDLabel:    pvcUID,
				velerov1api.BackupUIDLabel: name + "-uid",
			},
		},
		Status: velerov2alpha1api.DataUploadStatus{
			Phase:               velerov2alpha1api.DataUploadPhaseCompleted,
			CompletionTimestamp: &done,
			RetainedCSIStorageSnapshot: &velerov2alpha1api.RetainedCSIStorageSnapshot{
				CSISnapshotHandle: snapshotHandle,
				Driver:            retainedTestDriver,
				CSIVolumeHandle:   volumeHandle,
			},
		},
	}
}

func reconcileRelease(t *testing.T, r *retainedSnapshotReleaser, du *velerov2alpha1api.DataUpload) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: du.Namespace, Name: du.Name}})
	require.NoError(t, err)
	return result
}

func getRecord(t *testing.T, c client.Client, du *velerov2alpha1api.DataUpload) *velerov2alpha1api.RetainedCSIStorageSnapshot {
	t.Helper()
	current := &velerov2alpha1api.DataUpload{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(du), current))
	return current.Status.RetainedCSIStorageSnapshot
}

// TestRetainedSnapshotReleaseRollsTheBase proves the rolling base: when a
// DataUpload of a chain completes, every older unreleased retained snapshot of
// the same driver and volume handle is deleted from the storage, oldest first,
// and its record marked released by the new backup; other chains, the holder
// itself and a newer DataUpload are left alone.
func TestRetainedSnapshotReleaseRollsTheBase(t *testing.T) {
	backend := &fakeSnapshotBackend{snapshots: map[string]bool{"snap-a": true, "snap-b": true, "snap-c": true, "snap-d": true, "snap-other": true}}
	a := chainDataUpload("du-a", "pvc-1", "vol-1", "snap-a", 3*time.Hour)
	b := chainDataUpload("du-b", "pvc-1", "vol-1", "snap-b", 2*time.Hour)
	c := chainDataUpload("du-c", "pvc-1", "vol-1", "snap-c", time.Hour)
	newer := chainDataUpload("du-d", "pvc-1", "vol-1", "snap-d", 0)
	other := chainDataUpload("du-other", "pvc-2", "vol-2", "snap-other", 4*time.Hour)
	kb := backend.client(t, a, b, c, newer, other)
	recorder := &fakeEventRecorder{}
	r := &retainedSnapshotReleaser{NewRetainedSnapshotReconcilerWithRecorder(velerotest.NewLogger(), kb, kb, clock.RealClock{}, recorder)}

	result := reconcileRelease(t, r, c)
	assert.Zero(t, result.RequeueAfter)

	assert.Equal(t, []string{"snap-a", "snap-b"}, backend.deleteCallsTo, "older snapshots of the chain are released oldest first")
	for _, du := range []*velerov2alpha1api.DataUpload{a, b} {
		record := getRecord(t, kb, du)
		assert.True(t, record.Released, "%s is released", du.Name)
		assert.Equal(t, "du-c-uid", record.ReleasedBy)
	}
	assert.False(t, getRecord(t, kb, c).Released, "the holder keeps its snapshot")
	assert.False(t, getRecord(t, kb, newer).Released, "an older holder never releases a newer snapshot")
	assert.False(t, getRecord(t, kb, other).Released, "another chain is untouched")
	assert.True(t, backend.snapshots["snap-c"])
	assert.True(t, backend.snapshots["snap-other"])
	assert.Equal(t, []string{"RetainedSnapshotReleased", "RetainedSnapshotReleased"}, recorder.reasons)

	// Idempotent: released records are no candidates anymore.
	reconcileRelease(t, r, c)
	assert.Len(t, backend.deleteCallsTo, 2)
}

// TestRetainedSnapshotReleaseWaitsForDataMovement proves the release never runs
// during a data movement of the same volume: a running DataUpload may compute
// its delta against one of the snapshots, and a failed movement's retry must
// still find its base.
func TestRetainedSnapshotReleaseWaitsForDataMovement(t *testing.T) {
	backend := &fakeSnapshotBackend{snapshots: map[string]bool{"snap-a": true, "snap-b": true}}
	a := chainDataUpload("du-a", "pvc-1", "vol-1", "snap-a", 2*time.Hour)
	b := chainDataUpload("du-b", "pvc-1", "vol-1", "snap-b", time.Hour)
	running := chainDataUpload("du-running", "pvc-1", "", "", 0)
	running.Status.Phase = velerov2alpha1api.DataUploadPhaseInProgress
	running.Status.RetainedCSIStorageSnapshot = nil
	kb := backend.client(t, a, b, running)
	r := &retainedSnapshotReleaser{NewRetainedSnapshotReconciler(velerotest.NewLogger(), kb, kb, clock.RealClock{})}

	result := reconcileRelease(t, r, b)
	assert.Equal(t, retainedSnapshotInFlightRequeue, result.RequeueAfter)
	assert.Empty(t, backend.deleteCallsTo)
	assert.False(t, getRecord(t, kb, a).Released)
}

// TestRetainedSnapshotReleaseFailureKeepsRecord proves a failed release leaves
// the record unreleased -- its backup deletion still owns the snapshot -- and
// retries, with no escalation on this path.
func TestRetainedSnapshotReleaseFailureKeepsRecord(t *testing.T) {
	originalTimeout := csi.RetainedSnapshotDeletionTimeout
	t.Cleanup(func() { csi.RetainedSnapshotDeletionTimeout = originalTimeout })
	csi.RetainedSnapshotDeletionTimeout = 200 * time.Millisecond

	backend := &fakeSnapshotBackend{snapshots: map[string]bool{"snap-a": true, "snap-b": true}, deleteErrorMessage: "Failed to delete snapshot"}
	a := chainDataUpload("du-a", "pvc-1", "vol-1", "snap-a", 2*time.Hour)
	b := chainDataUpload("du-b", "pvc-1", "vol-1", "snap-b", time.Hour)
	kb := backend.client(t, a, b)
	recorder := &fakeEventRecorder{}
	r := &retainedSnapshotReleaser{NewRetainedSnapshotReconcilerWithRecorder(velerotest.NewLogger(), kb, kb, clock.RealClock{}, recorder)}

	result := reconcileRelease(t, r, b)
	assert.Equal(t, retainedSnapshotRetryDelay, result.RequeueAfter)
	assert.False(t, getRecord(t, kb, a).Released, "a failed release keeps the record owning the snapshot")
	assert.Contains(t, recorder.reasons, "RetainedSnapshotReleaseFailed")
}

// TestIsRollingBaseHolder proves only a completed DataUpload with an
// unreleased, chained record triggers a release.
func TestIsRollingBaseHolder(t *testing.T) {
	holder := chainDataUpload("du", "pvc", "vol", "snap", 0)
	assert.True(t, isRollingBaseHolder(holder))

	inProgress := holder.DeepCopy()
	inProgress.Status.Phase = velerov2alpha1api.DataUploadPhaseInProgress
	assert.False(t, isRollingBaseHolder(inProgress))

	released := holder.DeepCopy()
	released.Status.RetainedCSIStorageSnapshot.Released = true
	assert.False(t, isRollingBaseHolder(released))

	unchained := holder.DeepCopy()
	unchained.Status.RetainedCSIStorageSnapshot.CSIVolumeHandle = ""
	assert.False(t, isRollingBaseHolder(unchained))

	deleting := holder.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	assert.False(t, isRollingBaseHolder(deleting))
}

// TestGatherSkipsReleasedRecord proves the backup deletion skips a released
// record: the local DataUpload's released record drops the tarball's stale
// unreleased copy of the same entry through the local-wins merge, so the
// deletion never touches the snapshot the rolling base already released.
func TestGatherSkipsReleasedRecord(t *testing.T) {
	backup := retainedTestBackup()
	released := chainDataUpload("foo-du", "pvc-1", "vol-1", retainedTestHandle, time.Hour)
	released.Labels[velerov1api.BackupNameLabel] = backup.Name
	released.Status.RetainedCSIStorageSnapshot.Released = true
	released.Status.RetainedCSIStorageSnapshot.ReleasedBy = "later-uid"
	kb := velerotest.NewFakeControllerRuntimeClient(t, backup, released)

	// The delete action gathered the tarball's copy, serialized before the
	// release.
	require.NoError(t, retainedsnapshot.WriteEntries(t.Context(), kb, backup, map[string]*retainedsnapshot.Entry{
		"foo-du": {CSISnapshotHandle: retainedTestHandle, Driver: retainedTestDriver},
	}))

	r := &backupDeletionReconciler{Client: kb, logger: velerotest.NewLogger()}
	require.Empty(t, r.gatherRetainedSnapshotRecords(t.Context(), backup))

	family, err := retainedsnapshot.LoadFamily(t.Context(), kb, backup)
	require.NoError(t, err)
	assert.Zero(t, family.PendingEntries(), "the released record contributes nothing to delete")
}

// TestRetainedSnapshotReleaseEscalatesAtTheBound proves a release that keeps
// failing escalates at the same bound as a deletion: counted failures are
// persisted on the released DataUpload, and at the bound the record moves to
// the orphan-snapshot ConfigMap, is marked released so the backup deletion
// skips it, and RetainedSnapshotReleaseEscalated is emitted. An uncounted
// failure never counts.
func TestRetainedSnapshotReleaseEscalatesAtTheBound(t *testing.T) {
	originalTimeout := csi.RetainedSnapshotDeletionTimeout
	t.Cleanup(func() { csi.RetainedSnapshotDeletionTimeout = originalTimeout })
	csi.RetainedSnapshotDeletionTimeout = 200 * time.Millisecond

	t.Run("credential failure never counts", func(t *testing.T) {
		backend := &fakeSnapshotBackend{snapshots: map[string]bool{"snap-a": true, "snap-b": true}, deleteErrorMessage: "Failed to get snapshot credentials"}
		a := chainDataUpload("du-a", "pvc-1", "vol-1", "snap-a", 2*time.Hour)
		b := chainDataUpload("du-b", "pvc-1", "vol-1", "snap-b", time.Hour)
		kb := backend.client(t, a, b)
		r := &retainedSnapshotReleaser{NewRetainedSnapshotReconciler(velerotest.NewLogger(), kb, kb, clock.RealClock{})}

		for i := 0; i < retainedsnapshot.MaxAttempts+1; i++ {
			assert.Equal(t, retainedSnapshotRetryDelay, reconcileRelease(t, r, b).RequeueAfter)
		}
		current := &velerov2alpha1api.DataUpload{}
		require.NoError(t, kb.Get(t.Context(), client.ObjectKeyFromObject(a), current))
		assert.Empty(t, current.Annotations[retainedSnapshotReleaseAttemptsAnnotation])
		assert.False(t, current.Status.RetainedCSIStorageSnapshot.Released)
	})

	t.Run("call failure escalates at the bound", func(t *testing.T) {
		backend := &fakeSnapshotBackend{snapshots: map[string]bool{"snap-a": true, "snap-b": true}, deleteErrorMessage: "Failed to delete snapshot"}
		a := chainDataUpload("du-a", "pvc-1", "vol-1", "snap-a", 2*time.Hour)
		a.Labels[velerov1api.BackupNameLabel] = "backup-a"
		b := chainDataUpload("du-b", "pvc-1", "vol-1", "snap-b", time.Hour)
		kb := backend.client(t, a, b)
		recorder := &fakeEventRecorder{}
		r := &retainedSnapshotReleaser{NewRetainedSnapshotReconcilerWithRecorder(velerotest.NewLogger(), kb, kb, clock.RealClock{}, recorder)}

		for i := 1; i < retainedsnapshot.MaxAttempts; i++ {
			assert.Equal(t, retainedSnapshotRetryDelay, reconcileRelease(t, r, b).RequeueAfter)
			assert.False(t, getRecord(t, kb, a).Released, "under the bound the record stays unreleased")
		}
		assert.Zero(t, reconcileRelease(t, r, b).RequeueAfter, "the bound escalates instead of retrying")

		record := getRecord(t, kb, a)
		assert.True(t, record.Released, "an escalated record is skipped by the backup deletion")
		assert.Equal(t, "du-b-uid", record.ReleasedBy)
		orphans, err := retainedsnapshot.LoadOrphanEntries(t.Context(), kb, velerov1api.DefaultNamespace)
		require.NoError(t, err)
		require.Len(t, orphans, 1)
		assert.Equal(t, "du-a-uid", orphans[0].BackupUID)
		assert.Equal(t, "du-a", orphans[0].DataUpload)
		assert.Equal(t, "snap-a", orphans[0].Entry.CSISnapshotHandle)
		assert.Contains(t, recorder.reasons, "RetainedSnapshotReleaseEscalated")
	})
}
