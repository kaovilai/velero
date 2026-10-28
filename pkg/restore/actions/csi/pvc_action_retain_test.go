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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/builder"
	"github.com/velero-io/velero/pkg/retainedsnapshot"
	velerotest "github.com/velero-io/velero/pkg/test"
)

func TestDecideInPlaceBase(t *testing.T) {
	const localID, otherID = "local-uid", "other-uid"
	retained := &velerov2alpha1.RetainedSnapshot{SnapshotHandle: "snap-1", Driver: "d", ClusterID: localID}
	du := builder.ForDataUpload(velerov1api.DefaultNamespace, "du-1").
		Labels(map[string]string{velerov1api.BackupNameLabel: "b1"}).
		BackupStorageLocation("bsl").SourceNamespace("app").SourcePVC("data").Result()

	tests := []struct {
		name       string
		state      string
		reason     string
		clusterID  string
		noRecord   bool
		retainedDU *velerov2alpha1.RetainedSnapshot
		wantReason velerov2alpha1.FallbackReason
		wantHeld   bool
	}{
		{name: "live record is held", state: retainedsnapshot.StateRetained, wantHeld: true},
		{name: "rotated out", state: retainedsnapshot.StateDeleted, reason: retainedsnapshot.ReasonRotated, wantReason: velerov2alpha1.FallbackReasonBaseGone},
		{name: "being deleted", state: retainedsnapshot.StateDeleting, wantReason: velerov2alpha1.FallbackReasonBaseGone},
		{name: "record of another cluster", state: retainedsnapshot.StateRetained, clusterID: otherID, wantReason: velerov2alpha1.FallbackReasonBaseInOtherCluster},
		{name: "no record, not retained", noRecord: true, wantReason: velerov2alpha1.FallbackReasonBaseNotRetained},
		{name: "no record, retained here (reinstall)", noRecord: true, retainedDU: retained},
		{name: "no record, retained elsewhere", noRecord: true, retainedDU: &velerov2alpha1.RetainedSnapshot{SnapshotHandle: "snap-1", Driver: "d", ClusterID: otherID}, wantReason: velerov2alpha1.FallbackReasonBaseInOtherCluster},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns := &corev1api.Namespace{ObjectMeta: metav1.ObjectMeta{Name: metav1.NamespaceSystem, UID: types.UID(localID)}}
			c := velerotest.NewFakeControllerRuntimeClient(t, ns)
			if !tc.noRecord {
				r := *retained
				if tc.clusterID != "" {
					r.ClusterID = tc.clusterID
				}
				rec := retainedsnapshot.NewRecordForDataUpload(du, &r, "repo-1", time.Now(), "vol-1")
				rec.State = tc.state
				rec.Reason = tc.reason
				rec.DeletedAt = time.Now()
				require.NoError(t, c.Create(t.Context(), rec.ToConfigMap(velerov1api.DefaultNamespace)))
			}
			p := &pvcRestoreItemAction{log: velerotest.NewLogger(), crClient: c}
			restore := builder.ForRestore(velerov1api.DefaultNamespace, "r1").Result()
			backup := builder.ForBackup(velerov1api.DefaultNamespace, "b1").Result()
			dur := &velerov2alpha1.DataUploadResult{DataUploadName: "du-1", SourcePVC: "data", RetainBaseSnapshot: true, RetainedSnapshot: tc.retainedDU}

			until := time.Now().Add(time.Minute)
			base := p.decideInPlaceBase(t.Context(), restore, backup, dur, until, velerotest.NewLogger())
			assert.Equal(t, tc.wantReason, base.reason)
			if tc.wantReason != "" {
				assert.Contains(t, base.message, "base snapshot of backup b1 ")
			}
			if !tc.wantHeld {
				assert.Empty(t, base.heldRecord)
				return
			}
			assert.Equal(t, retainedsnapshot.RecordName("du-1"), base.heldRecord)
			_, rec, err := retainedsnapshot.Get(t.Context(), c, velerov1api.DefaultNamespace, base.heldRecord)
			require.NoError(t, err)
			assert.Equal(t, "r1", rec.InUseBy)
			assert.WithinDuration(t, until, rec.InUseUntil, time.Second)

			// The renewal before the DataDownload sees a base rotated meanwhile.
			cm, rec, err := retainedsnapshot.Get(t.Context(), c, velerov1api.DefaultNamespace, base.heldRecord)
			require.NoError(t, err)
			rec.State = retainedsnapshot.StateDeleted
			rec.Reason = retainedsnapshot.ReasonRotated
			rec.RotatedByBackup = "b2"
			rec.WriteTo(cm)
			require.NoError(t, c.Update(t.Context(), cm))
			renewed := p.renewHold(t.Context(), restore, backup, base.heldRecord, until, velerotest.NewLogger())
			assert.Equal(t, velerov2alpha1.FallbackReasonBaseGone, renewed.reason)
			assert.Contains(t, renewed.message, "by backup b2")
		})
	}
}

func TestDecideWithoutRetention(t *testing.T) {
	sms := func(required *bool) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(retainedsnapshot.SnapshotMetadataServiceGVK)
		u.SetName("d")
		if required != nil {
			require.NoError(t, unstructured.SetNestedField(u.Object, *required, "spec", "requireBaseSnapshotForDelta"))
		}
		return u
	}
	yes, no := true, false
	for _, tc := range []struct {
		name       string
		sms        *unstructured.Unstructured
		wantReason velerov2alpha1.FallbackReason
	}{
		{name: "no SnapshotMetadataService: as today"},
		{name: "requireBaseSnapshotForDelta unset: as today", sms: sms(nil)},
		{name: "requireBaseSnapshotForDelta false: as today", sms: sms(&no)},
		{name: "requireBaseSnapshotForDelta true: full, baseNotRetained", sms: sms(&yes), wantReason: velerov2alpha1.FallbackReasonBaseNotRetained},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pv := builder.ForPersistentVolume("pv-1").Result()
			pv.Spec.CSI = &corev1api.CSIPersistentVolumeSource{Driver: "d", VolumeHandle: "vol-1"}
			c := velerotest.NewFakeControllerRuntimeClient(t, pv)
			if tc.sms != nil {
				require.NoError(t, c.Create(t.Context(), tc.sms))
			}
			p := &pvcRestoreItemAction{log: velerotest.NewLogger(), crClient: c}
			pvc := builder.ForPersistentVolumeClaim("app", "data").VolumeName("pv-1").Result()
			base := p.decideWithoutRetention(t.Context(), builder.ForBackup(velerov1api.DefaultNamespace, "b1").Result(), pvc, velerotest.NewLogger())
			assert.Equal(t, tc.wantReason, base.reason)
		})
	}
}
