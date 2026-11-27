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
	"strings"
	"testing"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapshotv1fake "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/typed/volumesnapshot/v1/fake"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientTesting "k8s.io/client-go/testing"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// retentionHarness records, in order, every DataUpload update (phase and
// whether the record is set), every Retain patch of the backup content and
// every static release content created, so a test can assert the completion
// order. createErr fails the creation of the static release content.
type retentionHarness struct {
	steps       []string
	pruneRecord bool
	createErr   error
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
				if h.createErr != nil {
					return h.createErr
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	})
}

// newRetentionReconciler returns a reconciler whose DataUpload client is the
// harness's, with the DataUpload and its backup content created, the Retain
// patches recorded into the harness steps, patchErr returned for them, and
// the log captured.
func newRetentionReconciler(t *testing.T, h *retentionHarness, du *velerov2alpha1api.DataUpload, vsc *snapshotv1api.VolumeSnapshotContent, patchErr error) (*DataUploadReconciler, *logrustest.Hook) {
	t.Helper()
	r, err := initDataUploaderReconciler()
	require.NoError(t, err)
	r.client = h.client(t, du)
	logger, hook := logrustest.NewNullLogger()
	r.logger = logger
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
	return r, hook
}

func getDataUpload(t *testing.T, r *DataUploadReconciler, du *velerov2alpha1api.DataUpload) *velerov2alpha1api.DataUpload {
	t.Helper()
	current := &velerov2alpha1api.DataUpload{}
	require.NoError(t, r.client.Get(t.Context(), types.NamespacedName{Namespace: du.Namespace, Name: du.Name}, current))
	return current
}

// warned returns the Warn lines the hook captured.
func warned(hook *logrustest.Hook) []string {
	var lines []string
	for _, entry := range hook.AllEntries() {
		if entry.Level == logrus.WarnLevel {
			lines = append(lines, fmt.Sprintf("%s %v", entry.Message, entry.Data[logrus.ErrorKey]))
		}
	}
	return lines
}

// TestOnDataUploadCompletedRetentionOrder proves the completion order of a
// retaining DataUpload: one update writes Completed together with the record,
// then the backup content is switched to Retain, then the cleanup runs and
// deletes the content. Nothing retention-relevant happens before Completed
// wins: an update that declines leaves no record and no Retain, a response
// without the record skips Retain, and a failed Retain still leaves the
// DataUpload Completed. None of them fails the DataUpload, and nothing writes
// it after the Completed update; a skipped retention is logged.
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
		wantWarn    string
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
			wantWarn:    "dropped by the API server",
		},
		{
			// The Retain patch is one attempt.
			name:       "a failed Retain still leaves the DataUpload Completed",
			patchErr:   errors.New("throttled"),
			wantSteps:  []string{"update:Completed:record=true", "patch:Retain"},
			wantPhase:  velerov2alpha1api.DataUploadPhaseCompleted,
			wantRecord: true,
			wantWarn:   "throttled",
		},
		{
			name:      "no backup content to read the handle from: Completed without retention",
			noContent: true,
			wantSteps: []string{"update:Completed:record=false"},
			wantPhase: velerov2alpha1api.DataUploadPhaseCompleted,
			wantWarn:  "carries no snapshot handle",
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
			r, hook := newRetentionReconciler(t, h, du, vsc, tc.patchErr)

			r.OnDataUploadCompleted(t.Context(), du.Namespace, du.Name, datapath.Result{Backup: datapath.BackupResult{SnapshotID: "repo-snap"}})

			assert.Equal(t, tc.wantSteps, h.steps)
			updated := getDataUpload(t, r, du)
			assert.Equal(t, tc.wantPhase, updated.Status.Phase)
			if tc.wantRecord {
				assert.Equal(t, retentionRecord(), updated.Status.RetainedCSIStorageSnapshot)
			} else {
				assert.Nil(t, updated.Status.RetainedCSIStorageSnapshot)
			}
			if tc.wantWarn != "" {
				assert.Contains(t, strings.Join(warned(hook), "\n"), tc.wantWarn)
			}
			if !tc.noContent && tc.wantRecord && tc.patchErr == nil {
				_, err := r.csiSnapshotClient.VolumeSnapshotContents().Get(t.Context(), du.Name, metav1.GetOptions{})
				assert.True(t, apierrors.IsNotFound(err), "the retained content object is deleted after the cleanup, got %v", err)
			}
		})
	}
}

// TestOnDataUploadCompletedReleasesParent proves the release at completion:
// when this backup retained its own snapshot, the deletion of the parent's
// snapshot -- the change ID the data path reported, full read included -- is
// issued after Completed and Retain, with the driver and deletion secret of
// the DataUpload's own record, and the callback returns without waiting for
// the driver: the static content is left being deleted. Nothing is released
// without a parent, when the snapshot was not retained, or when the parent is
// the snapshot itself. A deletion that cannot be issued is logged with the
// handle and driver. The DataUpload is written once, by the Completed update.
func TestOnDataUploadCompletedReleasesParent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		result       datapath.BackupResult
		pruneRecord  bool
		patchErr     error
		createErr    error
		wantSteps    []string
		wantReleased bool
		wantWarn     string
	}{
		{
			name:         "an incremental releases its parent",
			result:       datapath.BackupResult{ParentChangeID: baseHandle},
			wantSteps:    []string{"update:Completed:record=true", "patch:Retain", "release:" + baseHandle},
			wantReleased: true,
		},
		{
			name:         "a full read with a parent releases it too",
			result:       datapath.BackupResult{FallbackFull: true, ParentChangeID: baseHandle},
			wantSteps:    []string{"update:Completed:record=true", "patch:Retain", "release:" + baseHandle},
			wantReleased: true,
		},
		{
			name:      "no parent, nothing to release",
			result:    datapath.BackupResult{},
			wantSteps: []string{"update:Completed:record=true", "patch:Retain"},
		},
		{
			name:        "a snapshot not retained releases nothing",
			result:      datapath.BackupResult{ParentChangeID: baseHandle},
			pruneRecord: true,
			wantSteps:   []string{"update:Completed:record=false"},
		},
		{
			name:      "a failed Retain releases nothing",
			result:    datapath.BackupResult{ParentChangeID: baseHandle},
			patchErr:  errors.New("throttled"),
			wantSteps: []string{"update:Completed:record=true", "patch:Retain"},
		},
		{
			name:      "the snapshot itself is never released",
			result:    datapath.BackupResult{ParentChangeID: retentionHandle},
			wantSteps: []string{"update:Completed:record=true", "patch:Retain"},
		},
		{
			name:      "a deletion that cannot be issued is logged",
			result:    datapath.BackupResult{ParentChangeID: baseHandle},
			createErr: errors.New("the API server is unavailable"),
			wantSteps: []string{"update:Completed:record=true", "patch:Retain", "release:" + baseHandle},
			wantWarn:  baseHandle + " of driver " + retentionDriver,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &retentionHarness{pruneRecord: tc.pruneRecord, createErr: tc.createErr}
			du := retentionDataUploadBuilder().Result()
			r, hook := newRetentionReconciler(t, h, du, retentionBackupVSC(), tc.patchErr)

			r.OnDataUploadCompleted(t.Context(), du.Namespace, du.Name, datapath.Result{Backup: tc.result})

			assert.Equal(t, tc.wantSteps, h.steps, "one DataUpload update; nothing written after Completed")
			assert.Equal(t, velerov2alpha1api.DataUploadPhaseCompleted, getDataUpload(t, r, du).Status.Phase)
			static := &snapshotv1api.VolumeSnapshotContent{}
			err := r.client.Get(t.Context(), kbclient.ObjectKey{Name: csi.TempVSCName(retentionDriver, baseHandle)}, static)
			if tc.wantReleased {
				require.NoError(t, err, "the static content is left to the sidecar")
				assert.NotNil(t, static.DeletionTimestamp, "its deletion is issued")
				require.NotNil(t, static.Status.SnapshotHandle)
				assert.Equal(t, baseHandle, *static.Status.SnapshotHandle)
				assert.Equal(t, "rook-csi-rbd-provisioner", static.Annotations[csi.VSCDeletionSecretNameAnnotation], "the parent is deleted with the record's deletion secret")
			} else {
				assert.True(t, apierrors.IsNotFound(err), "nothing is released, got %v", err)
			}
			if tc.wantWarn != "" {
				assert.Contains(t, strings.Join(warned(hook), "\n"), tc.wantWarn)
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
			r, _ := newRetentionReconciler(t, h, du, retentionBackupVSC(), nil)

			tc.run(r, getDataUpload(t, r, du))

			updated := getDataUpload(t, r, du)
			assert.Equal(t, tc.want, updated.Status.Phase)
			assert.Nil(t, updated.Status.RetainedCSIStorageSnapshot)
			assert.NotContains(t, h.steps, "patch:Retain")
		})
	}
}

// TestCrashBetweenCompletedAndRetain proves the state a node-agent that dies
// between the Completed update and the switch leaves: the DataUpload is
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
