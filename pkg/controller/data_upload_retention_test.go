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
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapshotv1fake "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/typed/volumesnapshot/v1/fake"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientTesting "k8s.io/client-go/testing"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/datapath"
	"github.com/velero-io/velero/pkg/exposer"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/csi"
)

const (
	retentionHandle = "0001-0009-rook-ceph-0000000000000001-snap"
	retentionDriver = "rbd.csi.ceph.com"
	baseHandle      = "0001-0009-rook-ceph-0000000000000000-snap"
)

// retentionDataUploadBuilder returns a DataUpload that opted into base snapshot
// retention through the volume policy, with the decision carried on
// spec.dataMoverConfig.
func retentionDataUploadBuilder() *builder.DataUploadBuilder {
	return dataUploadBuilder().
		DataMover("velero-block").
		DataMoverConfig(map[string]string{velerov2alpha1api.RetainSnapshotDataMoverConfigKey: "true"}).
		Labels(map[string]string{velerov1api.BackupUIDLabel: "backup-uid"})
}

// retentionBackupVSC returns the backup VolumeSnapshotContent the exposer
// creates for the DataUpload: named after it, statically provisioned from the
// source snapshot handle, with the deletion secret annotations the CSI
// snapshot controller sets.
func retentionBackupVSC() *snapshotv1api.VolumeSnapshotContent {
	handle := retentionHandle
	return &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: dataUploadName,
			Annotations: map[string]string{
				csi.VSCDeletionSecretNameAnnotation:      "rook-csi-rbd-provisioner",
				csi.VSCDeletionSecretNamespaceAnnotation: "rook-ceph",
			},
		},
		Spec: snapshotv1api.VolumeSnapshotContentSpec{
			DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
			Driver:         retentionDriver,
			Source:         snapshotv1api.VolumeSnapshotContentSource{SnapshotHandle: &handle},
		},
	}
}

func retentionRecord() *velerov2alpha1api.RetainedCSIStorageSnapshot {
	return &velerov2alpha1api.RetainedCSIStorageSnapshot{
		CSISnapshotHandle: retentionHandle,
		Driver:            retentionDriver,
		DeletionSecretRef: &corev1api.SecretReference{Name: "rook-csi-rbd-provisioner", Namespace: "rook-ceph"},
	}
}

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

// retentionHarness records, in order, every DataUpload update (phase and
// whether the record is set), every Retain patch of the backup content and
// every static release content created, so a test can assert the completion
// order. It also plays the CSI snapshotter for the release: a static content
// being deleted with a seeded handle is removed on the next read unless its
// handle is in failing.
type retentionHarness struct {
	steps       []string
	failing     map[string]bool
	deleteCalls []string
	pruneRecord bool
	// onRelease runs when the release creates its static content.
	onRelease func()
}

func (h *retentionHarness) client(t *testing.T, objs ...runtime.Object) kbclient.Client {
	t.Helper()
	base := velerotest.NewFakeControllerRuntimeClientBuilder(t).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&snapshotv1api.VolumeSnapshotContent{}).
		Build()
	return interceptor.NewClient(base.(kbclient.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c kbclient.WithWatch, obj kbclient.Object, opts ...kbclient.UpdateOption) error {
			if du, ok := obj.(*velerov2alpha1api.DataUpload); ok {
				if h.pruneRecord {
					// A DataUpload CRD that predates the field prunes it on
					// write; the update still succeeds.
					du.Status.RetainedCSIStorageSnapshot = nil
				}
				h.steps = append(h.steps, fmt.Sprintf("update:%s:record=%v", du.Status.Phase, du.Status.RetainedCSIStorageSnapshot != nil))
			}
			return c.Update(ctx, obj, opts...)
		},
		Create: func(ctx context.Context, c kbclient.WithWatch, obj kbclient.Object, opts ...kbclient.CreateOption) error {
			if vsc, ok := obj.(*snapshotv1api.VolumeSnapshotContent); ok {
				h.steps = append(h.steps, "release:"+*vsc.Spec.Source.SnapshotHandle)
				if h.onRelease != nil {
					h.onRelease()
				}
			}
			return c.Create(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c kbclient.WithWatch, key kbclient.ObjectKey, obj kbclient.Object, opts ...kbclient.GetOption) error {
			if _, ok := obj.(*snapshotv1api.VolumeSnapshotContent); ok {
				vsc := &snapshotv1api.VolumeSnapshotContent{}
				if err := c.Get(ctx, key, vsc); err == nil && vsc.DeletionTimestamp != nil && vsc.Status != nil && vsc.Status.SnapshotHandle != nil {
					h.deleteCalls = append(h.deleteCalls, *vsc.Status.SnapshotHandle)
					if !h.failing[*vsc.Status.SnapshotHandle] {
						vsc.Finalizers = nil
						_ = c.Update(ctx, vsc)
					}
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

// newRetentionReconciler returns a reconciler whose DataUpload client is the
// harness's, with the DataUpload and its backup content created, the Retain
// patches recorded into the harness steps, and patchErr returned for them.
func newRetentionReconciler(t *testing.T, h *retentionHarness, du *velerov2alpha1api.DataUpload, vsc *snapshotv1api.VolumeSnapshotContent, patchErr error) (*DataUploadReconciler, *recordingEvents) {
	t.Helper()
	r, err := initDataUploaderReconciler()
	require.NoError(t, err)
	r.client = h.client(t, du)
	events := &recordingEvents{}
	r.eventRecorder = events
	if vsc != nil {
		_, err := r.csiSnapshotClient.VolumeSnapshotContents().Create(t.Context(), vsc, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	r.csiSnapshotClient.(*snapshotv1fake.FakeSnapshotV1).PrependReactor("patch", "volumesnapshotcontents", func(action clientTesting.Action) (bool, runtime.Object, error) {
		var payload map[string]any
		_ = json.Unmarshal(action.(clientTesting.PatchAction).GetPatch(), &payload)
		if spec, _ := payload["spec"].(map[string]any); spec != nil {
			h.steps = append(h.steps, fmt.Sprintf("patch:%v", spec["deletionPolicy"]))
		}
		if patchErr != nil {
			return true, nil, patchErr
		}
		return false, nil, nil
	})
	r.snapshotExposerList = map[velerov2alpha1api.SnapshotType]exposer.SnapshotExposer{
		velerov2alpha1api.SnapshotTypeCSI: exposer.NewCSISnapshotExposer(r.kubeClient, r.csiSnapshotClient, velerotest.NewLogger()),
	}
	return r, events
}

func getDataUpload(t *testing.T, r *DataUploadReconciler, du *velerov2alpha1api.DataUpload) *velerov2alpha1api.DataUpload {
	t.Helper()
	current := &velerov2alpha1api.DataUpload{}
	require.NoError(t, r.client.Get(t.Context(), types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, current))
	return current
}

func shortRelease(t *testing.T) {
	t.Helper()
	orig := csi.RetainedSnapshotDeletionTimeout
	csi.RetainedSnapshotDeletionTimeout = 300 * time.Millisecond
	t.Cleanup(func() { csi.RetainedSnapshotDeletionTimeout = orig })
}

// TestOnDataUploadCompletedRetentionOrder proves the completion order of a
// retaining DataUpload: one update writes Completed together with the record,
// then the backup content is switched to Retain, then the cleanup runs and
// deletes the content. Nothing retention-relevant happens before Completed
// wins: an update that declines leaves no record and no Retain, a response
// without the record skips Retain, and a failed Retain still leaves the
// DataUpload Completed. None of them fails the DataUpload.
func TestOnDataUploadCompletedRetentionOrder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		phase       velerov2alpha1api.DataUploadPhase
		noContent   bool
		pruneRecord bool
		patchErr    error
		wantSteps   []string
		wantPhase   velerov2alpha1api.DataUploadPhase
		wantRecord  bool
		wantEvents  []string
	}{
		{
			name:       "record and Completed in one update, then Retain, then cleanup",
			wantSteps:  []string{"update:Completed:record=true", "patch:Retain"},
			wantPhase:  velerov2alpha1api.DataUploadPhaseCompleted,
			wantRecord: true,
		},
		{
			name:      "a DataUpload already in a final phase: the update declines, no record, no Retain",
			phase:     velerov2alpha1api.DataUploadPhaseCanceled,
			wantPhase: velerov2alpha1api.DataUploadPhaseCanceled,
		},
		{
			name:        "a response without the record skips Retain and completes without retention",
			pruneRecord: true,
			wantSteps:   []string{"update:Completed:record=false"},
			wantPhase:   velerov2alpha1api.DataUploadPhaseCompleted,
			wantEvents:  []string{"RetainedSnapshotSkipped"},
		},
		{
			name:       "a failed Retain still leaves the DataUpload Completed",
			patchErr:   errors.New("throttled"),
			wantSteps:  []string{"update:Completed:record=true", "patch:Retain"},
			wantPhase:  velerov2alpha1api.DataUploadPhaseCompleted,
			wantRecord: true,
			wantEvents: []string{"RetainedSnapshotSkipped"},
		},
		{
			name:       "no backup content to read the handle from: Completed without retention",
			noContent:  true,
			wantSteps:  []string{"update:Completed:record=false"},
			wantPhase:  velerov2alpha1api.DataUploadPhaseCompleted,
			wantEvents: []string{"RetainedSnapshotSkipped"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &retentionHarness{pruneRecord: tc.pruneRecord}
			b := retentionDataUploadBuilder()
			if tc.phase != "" {
				b = b.Phase(tc.phase)
			}
			du := b.Result()
			vsc := retentionBackupVSC()
			if tc.noContent {
				vsc = nil
			}
			r, events := newRetentionReconciler(t, h, du, vsc, tc.patchErr)

			r.OnDataUploadCompleted(t.Context(), du.Namespace, du.Name, datapath.Result{Backup: datapath.BackupResult{SnapshotID: "repo-snap"}})

			assert.Equal(t, tc.wantSteps, h.steps)
			updated := getDataUpload(t, r, du)
			assert.Equal(t, tc.wantPhase, updated.Status.Phase)
			if tc.wantRecord {
				assert.Equal(t, retentionRecord(), updated.Status.RetainedCSIStorageSnapshot)
			} else {
				assert.Nil(t, updated.Status.RetainedCSIStorageSnapshot)
			}
			assert.Equal(t, tc.wantEvents, events.reasons)
			if !tc.noContent {
				_, err := r.csiSnapshotClient.VolumeSnapshotContents().Get(t.Context(), du.Name, metav1.GetOptions{})
				if tc.wantRecord && tc.patchErr == nil {
					assert.True(t, apierrors.IsNotFound(err), "the retained content object is deleted after the cleanup, got %v", err)
				}
			}
		})
	}
}

// TestOnDataUploadCompletedReleasesBase proves the release at completion: the
// base the data path reported is deleted through a static content after
// Completed and the Retain switch, with the driver and deletion secret of the
// DataUpload's own record; nothing is released after a full read, when the
// snapshot was not retained, or when the base is the snapshot itself; a failed
// release leaves the DataUpload Completed with RetainedSnapshotReleaseFailed.
func TestOnDataUploadCompletedReleasesBase(t *testing.T) {
	for _, tc := range []struct {
		name         string
		result       datapath.BackupResult
		pruneRecord  bool
		failing      bool
		wantSteps    []string
		wantReleased bool
		wantEvents   []string
	}{
		{
			name:         "the base of an incremental is released after Completed and Retain",
			result:       datapath.BackupResult{ParentChangeID: baseHandle},
			wantSteps:    []string{"update:Completed:record=true", "patch:Retain", "release:" + baseHandle},
			wantReleased: true,
			wantEvents:   []string{"RetainedSnapshotReleased"},
		},
		{
			name:      "a full read releases nothing",
			result:    datapath.BackupResult{FallbackFull: true, FallbackReason: "base snapshot gone: not found"},
			wantSteps: []string{"update:Completed:record=true", "patch:Retain"},
		},
		{
			name:        "a snapshot not retained releases nothing",
			result:      datapath.BackupResult{ParentChangeID: baseHandle},
			pruneRecord: true,
			wantSteps:   []string{"update:Completed:record=false"},
			wantEvents:  []string{"RetainedSnapshotSkipped"},
		},
		{
			name:      "the snapshot itself is never released",
			result:    datapath.BackupResult{ParentChangeID: retentionHandle},
			wantSteps: []string{"update:Completed:record=true", "patch:Retain"},
		},
		{
			name:       "a failed release leaves the DataUpload Completed",
			result:     datapath.BackupResult{ParentChangeID: baseHandle},
			failing:    true,
			wantSteps:  []string{"update:Completed:record=true", "patch:Retain", "release:" + baseHandle},
			wantEvents: []string{"RetainedSnapshotReleaseFailed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortRelease(t)
			h := &retentionHarness{pruneRecord: tc.pruneRecord, failing: map[string]bool{baseHandle: tc.failing}}
			du := retentionDataUploadBuilder().Result()
			r, events := newRetentionReconciler(t, h, du, retentionBackupVSC(), nil)

			r.OnDataUploadCompleted(t.Context(), du.Namespace, du.Name, datapath.Result{Backup: tc.result})

			assert.Equal(t, tc.wantSteps, h.steps)
			assert.Equal(t, velerov2alpha1api.DataUploadPhaseCompleted, getDataUpload(t, r, du).Status.Phase)
			assert.Equal(t, tc.wantEvents, events.reasons)
			static := &snapshotv1api.VolumeSnapshotContent{}
			err := r.client.Get(t.Context(), kbclient.ObjectKey{Name: csi.TempVSCName(retentionDriver, baseHandle)}, static)
			if tc.wantReleased {
				assert.Contains(t, h.deleteCalls, baseHandle)
				assert.True(t, apierrors.IsNotFound(err), "the static content is gone, got %v", err)
			}
			if tc.failing {
				require.NoError(t, err, "a release that does not finish leaves its content being deleted")
				assert.NotNil(t, static.DeletionTimestamp)
				assert.Equal(t, "rook-csi-rbd-provisioner", static.Annotations[csi.VSCDeletionSecretNameAnnotation], "the base is deleted with the record's deletion secret")
			}
		})
	}
}

// TestFailurePathsLeaveNoRecord proves an unfinished movement holds no record
// and no Retain content: the cancel, failure and prepare-timeout paths write
// their final phase and clean up without touching retention.
func TestFailurePathsLeaveNoRecord(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase velerov2alpha1api.DataUploadPhase
		run   func(r *DataUploadReconciler, du *velerov2alpha1api.DataUpload)
		want  velerov2alpha1api.DataUploadPhase
	}{
		{
			name:  "OnDataUploadFailed",
			phase: velerov2alpha1api.DataUploadPhaseInProgress,
			run: func(r *DataUploadReconciler, du *velerov2alpha1api.DataUpload) {
				r.OnDataUploadFailed(context.Background(), du.Namespace, du.Name, errors.New("data path failed"))
			},
			want: velerov2alpha1api.DataUploadPhaseFailed,
		},
		{
			name:  "OnDataUploadCancelled",
			phase: velerov2alpha1api.DataUploadPhaseInProgress,
			run: func(r *DataUploadReconciler, du *velerov2alpha1api.DataUpload) {
				r.OnDataUploadCancelled(context.Background(), du.Namespace, du.Name)
			},
			want: velerov2alpha1api.DataUploadPhaseCanceled,
		},
		{
			name:  "tryCancelDataUpload",
			phase: velerov2alpha1api.DataUploadPhaseAccepted,
			run: func(r *DataUploadReconciler, du *velerov2alpha1api.DataUpload) {
				r.tryCancelDataUpload(context.Background(), du, "canceled")
			},
			want: velerov2alpha1api.DataUploadPhaseCanceled,
		},
		{
			name:  "onPrepareTimeout",
			phase: velerov2alpha1api.DataUploadPhasePrepared,
			run: func(r *DataUploadReconciler, du *velerov2alpha1api.DataUpload) {
				r.onPrepareTimeout(context.Background(), du)
			},
			want: velerov2alpha1api.DataUploadPhaseFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &retentionHarness{}
			du := retentionDataUploadBuilder().Phase(tc.phase).Result()
			r, events := newRetentionReconciler(t, h, du, retentionBackupVSC(), nil)

			tc.run(r, getDataUpload(t, r, du))

			updated := getDataUpload(t, r, du)
			assert.Equal(t, tc.want, updated.Status.Phase)
			assert.Nil(t, updated.Status.RetainedCSIStorageSnapshot)
			assert.NotContains(t, h.steps, "patch:Retain")
			assert.Empty(t, events.reasons)
		})
	}
}

// TestCrashBetweenCompletedAndRetain proves the state a node-agent that dies
// between the Completed update and the Retain switch leaves: the DataUpload is
// Completed with its record, the content keeps Delete, and the restarted
// node-agent's resume leaves the Completed DataUpload alone. The snapshot is
// not retained; deleting it again at backup deletion succeeds.
func TestCrashBetweenCompletedAndRetain(t *testing.T) {
	h := &retentionHarness{}
	du := retentionDataUploadBuilder().Phase(velerov2alpha1api.DataUploadPhaseCompleted).Result()
	du.Status.RetainedCSIStorageSnapshot = retentionRecord()
	r, _ := newRetentionReconciler(t, h, du, retentionBackupVSC(), nil)

	require.NoError(t, r.AttemptDataUploadResume(t.Context(), r.logger.WithField("test", t.Name()), du.Namespace))

	updated := getDataUpload(t, r, du)
	assert.Equal(t, velerov2alpha1api.DataUploadPhaseCompleted, updated.Status.Phase)
	assert.Equal(t, retentionRecord(), updated.Status.RetainedCSIStorageSnapshot)
	assert.Empty(t, h.steps, "nothing is written after the restart")
	vsc, err := r.csiSnapshotClient.VolumeSnapshotContents().Get(t.Context(), du.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, snapshotv1api.VolumeSnapshotContentDelete, vsc.Spec.DeletionPolicy, "the snapshot was never retained")
}

// TestFallbackReasonOf proves the fallback reason is the uploader's
// classification of the driver error, nothing else.
func TestFallbackReasonOf(t *testing.T) {
	assert.Empty(t, fallbackReasonOf(datapath.BackupResult{}))
	assert.Equal(t, "base snapshot gone: not found", fallbackReasonOf(datapath.BackupResult{FallbackFull: true, FallbackReason: "base snapshot gone: not found"}))
	assert.Equal(t, "fallen back to a full read", fallbackReasonOf(datapath.BackupResult{FallbackFull: true}))
}

// TestReleaseRunsAfterTheDataPathSlotIsFreed proves the release runs after the
// node's data path slot is given back: with a concurrency of one, a second
// DataUpload acquires the slot while the release of the first one's base is
// still waiting on the CSI snapshotter.
func TestReleaseRunsAfterTheDataPathSlotIsFreed(t *testing.T) {
	shortRelease(t)
	origCreator := datapath.MicroServiceBRWatcherCreator
	datapath.MicroServiceBRWatcherCreator = func(kbclient.Client, kubernetes.Interface, manager.Manager, string, string, string, string, string, string, datapath.Callbacks, logrus.FieldLogger) datapath.AsyncBR {
		return nil
	}
	t.Cleanup(func() { datapath.MicroServiceBRWatcherCreator = origCreator })

	// The base never goes, so the release waits for its whole bound.
	h := &retentionHarness{failing: map[string]bool{baseHandle: true}}
	du := retentionDataUploadBuilder().Result()
	r, events := newRetentionReconciler(t, h, du, retentionBackupVSC(), nil)
	r.dataPathMgr = datapath.NewManager(1)
	_, err := r.dataPathMgr.CreateMicroServiceBRWatcher(t.Context(), r.client, nil, nil, datapath.TaskTypeBackup, du.Name, du.Namespace, "", "", "", datapath.Callbacks{}, false, velerotest.NewLogger())
	require.NoError(t, err)
	_, err = r.dataPathMgr.CreateMicroServiceBRWatcher(t.Context(), r.client, nil, nil, datapath.TaskTypeBackup, "other-du", du.Namespace, "", "", "", datapath.Callbacks{}, false, velerotest.NewLogger())
	require.ErrorIs(t, err, datapath.ConcurrentLimitExceed, "the data path holds the only slot")

	var acquired []error
	h.onRelease = func() {
		_, err := r.dataPathMgr.CreateMicroServiceBRWatcher(t.Context(), r.client, nil, nil, datapath.TaskTypeBackup, "other-du", du.Namespace, "", "", "", datapath.Callbacks{}, false, velerotest.NewLogger())
		acquired = append(acquired, err)
	}

	r.OnDataUploadCompleted(t.Context(), du.Namespace, du.Name, datapath.Result{Backup: datapath.BackupResult{ParentChangeID: baseHandle}})

	require.Len(t, acquired, 1, "the release ran")
	require.NoError(t, acquired[0], "another DataUpload acquires the slot while the release waits")
	assert.Equal(t, []string{"RetainedSnapshotReleaseFailed"}, events.reasons)
}
