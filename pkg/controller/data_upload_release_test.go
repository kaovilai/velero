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
	snapshotv1fake "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/typed/volumesnapshot/v1/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientTesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/datapath"
	"github.com/velero-io/velero/pkg/exposer"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/uploader/block"
	"github.com/velero-io/velero/pkg/util/csi"
)

// recordingEvents is a kube.EventRecorder that records the event reasons.
type recordingEvents struct {
	reasons  []string
	warnings []bool
}

func (e *recordingEvents) Event(_ runtime.Object, warning bool, reason string, _ string, _ ...any) {
	e.reasons = append(e.reasons, reason)
	e.warnings = append(e.warnings, warning)
}
func (e *recordingEvents) EndingEvent(o runtime.Object, warning bool, reason string, message string, a ...any) {
	e.Event(o, warning, reason, message, a...)
}
func (e *recordingEvents) Shutdown() {}

func releaseReconciler(t *testing.T, c crclient.Client) (*DataUploadReconciler, *recordingEvents) {
	t.Helper()
	r, err := initDataUploaderReconciler()
	require.NoError(t, err)
	r.client = c
	r.apiReader = nil
	events := &recordingEvents{}
	r.eventRecorder = events
	return r, events
}

func duRecord(t *testing.T, c crclient.Client, name string) *velerov2alpha1api.RetainedCSIStorageSnapshot {
	t.Helper()
	du := &velerov2alpha1api.DataUpload{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: velerov1api.DefaultNamespace, Name: name}, du))
	return du.Status.RetainedCSIStorageSnapshot
}

func shortRelease(t *testing.T) {
	t.Helper()
	orig := csi.RetainedSnapshotDeletionTimeout
	csi.RetainedSnapshotDeletionTimeout = 300 * time.Millisecond
	t.Cleanup(func() { csi.RetainedSnapshotDeletionTimeout = orig })
}

// TestReleaseOlderRetainedSnapshotsCatchesUp proves the release at completion:
// every older unreleased record of the completed DataUpload's chain -- the
// direct parent and an older one an earlier release missed -- is deleted,
// oldest first, marked released by the completed backup, and reported with
// RetainedSnapshotReleased; another chain, the holder itself and a newer
// DataUpload are left alone.
func TestReleaseOlderRetainedSnapshotsCatchesUp(t *testing.T) {
	shortRelease(t)
	missed := chainDU("du-1", "vol-1", "snap-1", velerov2alpha1api.DataUploadPhaseCompleted, 3*time.Hour)
	parent := chainDU("du-2", "vol-1", "snap-2", velerov2alpha1api.DataUploadPhaseCompleted, 2*time.Hour)
	holder := chainDU("du-3", "vol-1", "snap-3", velerov2alpha1api.DataUploadPhaseCompleted, time.Hour)
	newer := chainDU("du-4", "vol-1", "snap-4", velerov2alpha1api.DataUploadPhaseCompleted, 0)
	other := chainDU("du-other", "vol-2", "snap-other", velerov2alpha1api.DataUploadPhaseCompleted, 4*time.Hour)
	sidecar := &releaseSidecar{snapshots: map[string]bool{"snap-1": true, "snap-2": true, "snap-3": true, "snap-4": true, "snap-other": true}}
	c := sidecar.client(t, missed, parent, holder, newer, other)
	r, events := releaseReconciler(t, c)

	r.releaseOlderRetainedSnapshots(t.Context(), types.NamespacedName{Namespace: holder.Namespace, Name: holder.Name}, velerotest.NewLogger())

	assert.Equal(t, []string{"snap-1", "snap-2"}, sidecar.deleteCalls, "oldest first, the missed one included")
	for _, name := range []string{"du-1", "du-2"} {
		record := duRecord(t, c, name)
		assert.True(t, record.Released, name)
		assert.Equal(t, "du-3-uid", record.ReleasedBy)
	}
	for _, name := range []string{"du-3", "du-4", "du-other"} {
		assert.False(t, duRecord(t, c, name).Released, name)
	}
	assert.Equal(t, []string{"RetainedSnapshotReleased", "RetainedSnapshotReleased"}, events.reasons)
	assert.Equal(t, []bool{false, false}, events.warnings, "Normal events")
}

// TestReleaseOlderRetainedSnapshotsTimeout proves a release that does not
// finish never fails the backup: RetainedSnapshotReleaseTimedOut is emitted,
// the record stays unreleased for the next backup's catch-up, the static
// content is left being deleted, and the release continues with the next
// record.
func TestReleaseOlderRetainedSnapshotsTimeout(t *testing.T) {
	shortRelease(t)
	stuck := chainDU("du-1", "vol-1", "snap-1", velerov2alpha1api.DataUploadPhaseCompleted, 3*time.Hour)
	parent := chainDU("du-2", "vol-1", "snap-2", velerov2alpha1api.DataUploadPhaseCompleted, 2*time.Hour)
	holder := chainDU("du-3", "vol-1", "snap-3", velerov2alpha1api.DataUploadPhaseCompleted, time.Hour)
	sidecar := &releaseSidecar{snapshots: map[string]bool{"snap-1": true, "snap-2": true, "snap-3": true}, failing: map[string]bool{"snap-1": true}}
	c := sidecar.client(t, stuck, parent, holder)
	r, events := releaseReconciler(t, c)

	r.releaseOlderRetainedSnapshots(t.Context(), types.NamespacedName{Namespace: holder.Namespace, Name: holder.Name}, velerotest.NewLogger())

	assert.False(t, duRecord(t, c, "du-1").Released, "a timed-out release keeps the record unreleased")
	assert.True(t, duRecord(t, c, "du-2").Released, "the release continues with the next record")
	assert.Equal(t, []string{"RetainedSnapshotReleaseTimedOut", "RetainedSnapshotReleased"}, events.reasons)
	assert.Equal(t, []bool{true, false}, events.warnings)
	left := &snapshotv1api.VolumeSnapshotContent{}
	require.NoError(t, c.Get(t.Context(), crclient.ObjectKey{Name: csi.TempVSCName(retentionDriver, "snap-1")}, left))
	assert.NotNil(t, left.DeletionTimestamp, "the content is left being deleted")

	// The next backup's catch-up checks the content still in deletion once
	// instead of waiting the bound again.
	csi.RetainedSnapshotDeletionTimeout = time.Hour
	next := chainDU("du-4", "vol-1", "snap-4", velerov2alpha1api.DataUploadPhaseCompleted, 0)
	require.NoError(t, c.Create(t.Context(), next))
	start := time.Now()
	r.releaseOlderRetainedSnapshots(t.Context(), types.NamespacedName{Namespace: next.Namespace, Name: next.Name}, velerotest.NewLogger())
	assert.Less(t, time.Since(start), 5*time.Second, "a content already in deletion is not waited on again")
	assert.False(t, duRecord(t, c, "du-1").Released)
	assert.True(t, duRecord(t, c, "du-3").Released, "the newer parent is released")

	// Once the driver recovers, a later catch-up releases it.
	sidecar.failing = nil
	last := chainDU("du-5", "vol-1", "snap-5", velerov2alpha1api.DataUploadPhaseCompleted, 0)
	last.Status.CompletionTimestamp = &metav1.Time{Time: time.Now().Add(time.Minute)}
	require.NoError(t, c.Create(t.Context(), last))
	r.releaseOlderRetainedSnapshots(t.Context(), types.NamespacedName{Namespace: last.Namespace, Name: last.Name}, velerotest.NewLogger())
	assert.True(t, duRecord(t, c, "du-1").Released)
}

// TestReleaseOlderRetainedSnapshotsOnlyAfterCompleted proves a failed or
// canceled backup, or one not yet Completed, releases nothing: its parent stays
// the chain's base for the retry.
func TestReleaseOlderRetainedSnapshotsOnlyAfterCompleted(t *testing.T) {
	for _, phase := range []velerov2alpha1api.DataUploadPhase{velerov2alpha1api.DataUploadPhaseInProgress, velerov2alpha1api.DataUploadPhaseFailed, velerov2alpha1api.DataUploadPhaseCanceled} {
		t.Run(string(phase), func(t *testing.T) {
			parent := chainDU("du-1", "vol-1", "snap-1", velerov2alpha1api.DataUploadPhaseCompleted, time.Hour)
			current := chainDU("du-2", "vol-1", "snap-2", phase, 0)
			sidecar := &releaseSidecar{snapshots: map[string]bool{"snap-1": true, "snap-2": true}}
			c := sidecar.client(t, parent, current)
			r, events := releaseReconciler(t, c)

			r.releaseOlderRetainedSnapshots(t.Context(), types.NamespacedName{Namespace: current.Namespace, Name: current.Name}, velerotest.NewLogger())

			assert.Empty(t, sidecar.deleteCalls)
			assert.False(t, duRecord(t, c, "du-1").Released)
			assert.Empty(t, events.reasons)
		})
	}
}

// TestReleaseCandidatesOrder proves the candidates are the unreleased records
// of the same driver and volume handle completed before the holder, oldest
// first.
func TestReleaseCandidatesOrder(t *testing.T) {
	holder := chainDU("holder", "vol-1", "snap-h", velerov2alpha1api.DataUploadPhaseCompleted, time.Hour)
	var all []velerov2alpha1api.DataUpload
	for i, ago := range []time.Duration{2 * time.Hour, 5 * time.Hour, 3 * time.Hour} {
		all = append(all, *chainDU(fmt.Sprintf("du-%d", i), "vol-1", fmt.Sprintf("snap-%d", i), velerov2alpha1api.DataUploadPhaseCompleted, ago))
	}
	released := chainDU("released", "vol-1", "snap-r", velerov2alpha1api.DataUploadPhaseCompleted, 6*time.Hour)
	released.Status.RetainedCSIStorageSnapshot.Released = true
	otherDriver := chainDU("other-driver", "vol-1", "snap-o", velerov2alpha1api.DataUploadPhaseCompleted, 6*time.Hour)
	otherDriver.Status.RetainedCSIStorageSnapshot.Driver = "other.csi.example.com"
	all = append(all, *released, *otherDriver, *holder)

	var names []string
	for _, du := range releaseCandidates(holder, all) {
		names = append(names, du.Name)
	}
	assert.Equal(t, []string{"du-1", "du-2", "du-0"}, names)
}

// phaseAtVSCCreate records the phase of a DataUpload when a static
// VolumeSnapshotContent is created, and refuses the create so the release
// stops there.
type phaseAtVSCCreate struct {
	crclient.Client
	watched types.NamespacedName
	phases  []velerov2alpha1api.DataUploadPhase
}

func (p *phaseAtVSCCreate) Create(ctx context.Context, obj crclient.Object, opts ...crclient.CreateOption) error {
	if vsc, ok := obj.(*snapshotv1api.VolumeSnapshotContent); ok && vsc.Name == csi.TempVSCName(retentionDriver, "parent-snap") {
		du := &velerov2alpha1api.DataUpload{}
		if err := p.Client.Get(ctx, p.watched, du); err == nil {
			p.phases = append(p.phases, du.Status.Phase)
		}
		return fmt.Errorf("stop the release here")
	}
	return p.Client.Create(ctx, obj, opts...)
}

// TestOnDataUploadCompletedReleasesAfterCompleted proves the completion order:
// the record is persisted with the chain key, the content is retained and
// cleaned up, Completed is written, and only then does the release of the
// parent start; a release that fails does not fail the completed backup.
func TestOnDataUploadCompletedReleasesAfterCompleted(t *testing.T) {
	r, err := initDataUploaderReconciler()
	require.NoError(t, err)
	r.eventRecorder = &recordingEvents{}
	ctx := t.Context()

	_, err = r.kubeClient.CoreV1().PersistentVolumeClaims("fake-ns").Create(ctx, &corev1api.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "fake-ns", Name: "test-pvc"},
		Spec:       corev1api.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = r.kubeClient.CoreV1().PersistentVolumes().Create(ctx, &corev1api.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1api.PersistentVolumeSpec{PersistentVolumeSource: corev1api.PersistentVolumeSource{
			CSI: &corev1api.CSIPersistentVolumeSource{Driver: retentionDriver, VolumeHandle: "vol-1"},
		}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	parent := chainDU("parent-du", "vol-1", "parent-snap", velerov2alpha1api.DataUploadPhaseCompleted, time.Hour)
	// The completion is stamped with the reconciler's clock.
	parent.Status.CompletionTimestamp = &metav1.Time{Time: r.Clock.Now().Add(-time.Hour)}
	require.NoError(t, r.client.Create(ctx, parent))

	du := retentionDataUploadBuilder().Result()
	du.Labels = map[string]string{velerov1api.BackupUIDLabel: "holder-uid", velerov1api.PVCUIDLabel: "pvc-1"}
	createRetainedSnapshotObjects(t, r, du, retentionBackupVSC())
	watcher := &phaseAtVSCCreate{Client: r.client, watched: types.NamespacedName{Namespace: du.Namespace, Name: du.Name}}
	r.client = watcher
	r.snapshotExposerList = map[velerov2alpha1api.SnapshotType]exposer.SnapshotExposer{
		velerov2alpha1api.SnapshotTypeCSI: exposer.NewCSISnapshotExposer(r.kubeClient, r.csiSnapshotClient, velerotest.NewLogger()),
	}

	r.OnDataUploadCompleted(ctx, du.Namespace, du.Name, datapath.Result{Backup: datapath.BackupResult{SnapshotID: "fake-id"}})

	require.Equal(t, []velerov2alpha1api.DataUploadPhase{velerov2alpha1api.DataUploadPhaseCompleted}, watcher.phases,
		"the release of the parent starts only after Completed is written")
	updated := getDataUpload(t, r, du)
	assert.Equal(t, velerov2alpha1api.DataUploadPhaseCompleted, updated.Status.Phase, "a failed release does not fail the backup")
	require.NotNil(t, updated.Status.RetainedCSIStorageSnapshot)
	assert.Equal(t, "vol-1", updated.Status.RetainedCSIStorageSnapshot.CSIVolumeHandle)
	assert.False(t, duRecord(t, r.client, "parent-du").Released, "the failed release keeps the parent unreleased for the next backup")
}

// TestReconcileDeletedDataUploadRunsPatchBack proves a DataUpload deleted
// through the API runs the patch-back before its finalizer goes: the snapshot
// is released and the finalizer dropped; at the bound the finalizer is dropped
// too, with RetainedSnapshotReleaseTimedOut, and the content stays in deletion.
func TestReconcileDeletedDataUploadRunsPatchBack(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stuck     bool
		wantEvent []string
	}{
		{name: "released, then the finalizer goes"},
		{name: "at the bound the finalizer goes too", stuck: true, wantEvent: []string{"RetainedSnapshotReleaseTimedOut"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := csi.RetainedSnapshotDeletionTimeout
			csi.RetainedSnapshotDeletionTimeout = 200 * time.Millisecond
			t.Cleanup(func() { csi.RetainedSnapshotDeletionTimeout = orig })

			r, err := initDataUploaderReconciler()
			require.NoError(t, err)
			events := &recordingEvents{}
			r.eventRecorder = events
			ctx := t.Context()

			du := retentionDataUploadBuilder().Phase(velerov2alpha1api.DataUploadPhaseFailed).Result()
			du.Status.RetainedCSIStorageSnapshot = retentionRecord()
			du.Finalizers = []string{DataUploadDownloadFinalizer}
			vsc := retentionBackupVSC()
			vsc.Spec.DeletionPolicy = snapshotv1api.VolumeSnapshotContentRetain
			createRetainedSnapshotObjects(t, r, du, vsc)
			if tc.stuck {
				// The sidecar never removes the content.
				r.csiSnapshotClient.(*snapshotv1fake.FakeSnapshotV1).PrependReactor("delete", "volumesnapshotcontents", func(clientTesting.Action) (bool, runtime.Object, error) {
					return true, nil, nil
				})
			}
			require.NoError(t, r.client.Delete(ctx, getDataUpload(t, r, du)))

			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: du.Namespace, Name: du.Name}})
			require.NoError(t, err)

			gone := &velerov2alpha1api.DataUpload{}
			getErr := r.client.Get(ctx, types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, gone)
			assert.True(t, apierrors.IsNotFound(getErr), "the finalizer is dropped and the DataUpload goes, got %v", getErr)
			current, vscErr := getBackupVSC(t, r, du)
			if tc.stuck {
				require.NoError(t, vscErr, "the content stays in deletion as the visible trace")
				assert.Equal(t, snapshotv1api.VolumeSnapshotContentDelete, current.Spec.DeletionPolicy)
			} else {
				assert.True(t, apierrors.IsNotFound(vscErr), "the patch-back deleted the content, so the driver deletes the snapshot")
			}
			assert.Equal(t, tc.wantEvent, events.reasons)
		})
	}
}

// TestOnDataUploadCompletedTakesOverDeadHolderLease proves the restart
// interleaving: a node-agent died inside the completion callback right after
// it acquired the retaining mark, before the record was written, and its
// restarted successor runs the callback again. The successor waits out the
// dead holder's mark instead of failing the DataUpload, then finishes the
// completion: record, Retain, cleanup, Completed, and the release of the
// parent after Completed.
func TestOnDataUploadCompletedTakesOverDeadHolderLease(t *testing.T) {
	r, err := initDataUploaderReconciler()
	require.NoError(t, err)
	r.eventRecorder = &recordingEvents{}
	ctx := t.Context()

	_, err = r.kubeClient.CoreV1().PersistentVolumeClaims("fake-ns").Create(ctx, &corev1api.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "fake-ns", Name: "test-pvc"},
		Spec:       corev1api.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = r.kubeClient.CoreV1().PersistentVolumes().Create(ctx, &corev1api.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1api.PersistentVolumeSpec{PersistentVolumeSource: corev1api.PersistentVolumeSource{
			CSI: &corev1api.CSIPersistentVolumeSource{Driver: retentionDriver, VolumeHandle: "vol-1"},
		}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	parent := chainDU("parent-du", "vol-1", "parent-snap", velerov2alpha1api.DataUploadPhaseCompleted, time.Hour)
	parent.Status.CompletionTimestamp = &metav1.Time{Time: r.Clock.Now().Add(-time.Hour)}
	require.NoError(t, r.client.Create(ctx, parent))

	// The dead process's mark: acquired with the lease duration, never renewed.
	start := r.Clock.Now()
	du := retentionDataUploadBuilder().Phase(velerov2alpha1api.DataUploadPhaseInProgress).Result()
	du.Labels = map[string]string{velerov1api.BackupUIDLabel: "holder-uid", velerov1api.PVCUIDLabel: "pvc-1"}
	du.Annotations = map[string]string{dataUploadRetainLeaseAnnotation: (&dataUploadLease{
		Holder:    "node-1-dead",
		ExpiresAt: start.Add(dataUploadLeaseDuration),
	}).String()}
	createRetainedSnapshotObjects(t, r, du, retentionBackupVSC())
	patches := &vscPatchRecorder{}
	r.csiSnapshotClient.(*snapshotv1fake.FakeSnapshotV1).PrependReactor("patch", "volumesnapshotcontents", func(action clientTesting.Action) (bool, runtime.Object, error) {
		patches.patches = append(patches.patches, string(action.(clientTesting.PatchAction).GetPatch()))
		return false, nil, nil
	})
	watcher := &phaseAtVSCCreate{Client: r.client, watched: types.NamespacedName{Namespace: du.Namespace, Name: du.Name}}
	r.client = watcher
	r.snapshotExposerList = map[velerov2alpha1api.SnapshotType]exposer.SnapshotExposer{
		velerov2alpha1api.SnapshotTypeCSI: exposer.NewCSISnapshotExposer(r.kubeClient, r.csiSnapshotClient, velerotest.NewLogger()),
	}

	r.OnDataUploadCompleted(ctx, du.Namespace, du.Name, datapath.Result{Backup: datapath.BackupResult{SnapshotID: "fake-id"}})

	updated := getDataUpload(t, r, du)
	assert.Equal(t, velerov2alpha1api.DataUploadPhaseCompleted, updated.Status.Phase, "the dead holder's mark never fails the DataUpload")
	require.NotNil(t, updated.Status.RetainedCSIStorageSnapshot)
	assert.Equal(t, retentionHandle, updated.Status.RetainedCSIStorageSnapshot.CSISnapshotHandle)
	assert.True(t, patches.hasDeletionPolicy("Retain"), "the successor retained the snapshot")
	assert.NotContains(t, updated.Annotations, dataUploadRetainLeaseAnnotation, "Completed clears the marks")
	assert.False(t, r.Clock.Now().Before(start.Add(dataUploadLeaseDuration)), "the successor waited the dead holder's mark out")
	assert.Equal(t, []velerov2alpha1api.DataUploadPhase{velerov2alpha1api.DataUploadPhaseCompleted}, watcher.phases,
		"the release of the parent runs, after Completed")
}

// TestFullReadAfterUnfinishedParentReleasesOlderBase proves that the full read
// forced by an unfinished parent leaks nothing: a movement failed after its
// repository snapshot was committed, so it is the next backup's parent while
// its own storage snapshot is gone and the older completed backup's base sits
// unused. The next backup reads in full, its reason names the unfinished
// DataUpload, and its completion releases the older retained base through the
// catch-up, leaving only its own snapshot on the storage.
func TestFullReadAfterUnfinishedParentReleasesOlderBase(t *testing.T) {
	shortRelease(t)
	now := time.Now()
	retainedParent := chainDU("du-a", "vol-1", "snap-a", velerov2alpha1api.DataUploadPhaseCompleted, 2*time.Hour)
	retainedParent.Spec.DataMover = "velero-block"
	retainedParent.Status.SnapshotID = "repo-a"
	retainedParent.Status.StartTimestamp = ptr.To(metav1.NewTime(now.Add(-3 * time.Hour)))
	// The patch-back removed the failed movement's snapshot; its record was
	// never written, as when the node-agent died recording it.
	unfinished := chainDU("du-b", "vol-1", "", velerov2alpha1api.DataUploadPhaseFailed, time.Hour)
	unfinished.Spec.DataMover = "velero-block"
	unfinished.Status.RetainedCSIStorageSnapshot = nil
	unfinished.Status.SnapshotID = "repo-b"
	unfinished.Status.StartTimestamp = ptr.To(metav1.NewTime(now.Add(-90 * time.Minute)))
	full := chainDU("du-c", "vol-1", "snap-c", velerov2alpha1api.DataUploadPhaseCompleted, 0)
	full.Spec.DataMover = "velero-block"
	full.Status.StartTimestamp = ptr.To(metav1.NewTime(now.Add(-10 * time.Minute)))
	sidecar := &releaseSidecar{snapshots: map[string]bool{"snap-a": true, "snap-c": true}}
	c := sidecar.client(t, retainedParent, unfinished, full)
	r, events := releaseReconciler(t, c)

	reason := r.fallbackReasonOf(t.Context(), full, datapath.BackupResult{FallbackFull: true, FallbackReason: block.FallbackReasonDriverError + ": rbd"}, velerotest.NewLogger())
	assert.Equal(t, "parent repository snapshot repo-b belongs to DataUpload du-b (Failed), whose snapshot was not retained; "+block.FallbackReasonDriverError+": rbd", reason)
	assert.NotContains(t, reason, "was retained but could not be used")

	r.releaseOlderRetainedSnapshots(t.Context(), types.NamespacedName{Namespace: full.Namespace, Name: full.Name}, velerotest.NewLogger())

	assert.Equal(t, []string{"snap-a"}, sidecar.deleteCalls, "the older retained base is released by the full backup's catch-up")
	record := duRecord(t, c, "du-a")
	assert.True(t, record.Released)
	assert.Equal(t, "du-c-uid", record.ReleasedBy)
	assert.False(t, duRecord(t, c, "du-c").Released, "the full backup's own snapshot is the new base")
	assert.Equal(t, map[string]bool{"snap-a": false, "snap-c": true}, sidecar.snapshots, "only the new base is left on the storage")
	assert.Equal(t, []string{"RetainedSnapshotReleased"}, events.reasons)
}
