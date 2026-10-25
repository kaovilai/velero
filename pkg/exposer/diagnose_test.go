/*
Copyright The Velero Contributors.

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

package exposer

import (
	"strings"
	"testing"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/velero-io/velero/pkg/util/csi"
	"github.com/velero-io/velero/pkg/util/kube"
)

func TestSummarizeDiagnose(t *testing.T) {
	pod := &corev1api.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "fake-dd", UID: "pod-uid"},
		Status: corev1api.PodStatus{
			Phase: corev1api.PodPending,
			Conditions: []corev1api.PodCondition{
				{
					Type:    corev1api.PodScheduled,
					Status:  corev1api.ConditionFalse,
					Reason:  "Unschedulable",
					Message: "0/6 nodes are available: pod has unbound immediate PersistentVolumeClaims",
				},
			},
		},
	}

	pvc := &corev1api.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "fake-dd", UID: "pvc-uid"},
		Status:     corev1api.PersistentVolumeClaimStatus{Phase: corev1api.ClaimPending},
	}

	events := &corev1api.EventList{Items: []corev1api.Event{
		{
			InvolvedObject: corev1api.ObjectReference{UID: "pvc-uid"},
			Type:           corev1api.EventTypeWarning,
			Reason:         "ProvisioningFailed",
			Message:        "failed to provision volume with StorageClass \"gp3-csi\": multi-attach is only supported for io2 volumes",
		},
		{
			InvolvedObject: corev1api.ObjectReference{UID: "pvc-uid"},
			Type:           corev1api.EventTypeNormal,
			Reason:         "Provisioning",
			Message:        "External provisioner is provisioning volume",
		},
	}}

	vscName := "fake-vsc"
	vsErr := "failed to create snapshot: quota exceeded"
	vs := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "fake-du"},
		Status: &snapshotv1api.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: &vscName,
			Error:                          &snapshotv1api.VolumeSnapshotError{Message: &vsErr},
		},
	}
	healthyVS := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "fake-du"},
		Status:     &snapshotv1api.VolumeSnapshotStatus{BoundVolumeSnapshotContentName: &vscName},
	}

	handle := "fake-handle"
	vscErr := "rpc error: code = ResourceExhausted"
	vsc := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: vscName},
		Status: &snapshotv1api.VolumeSnapshotContentStatus{
			SnapshotHandle: &handle,
			Error:          &snapshotv1api.VolumeSnapshotError{Message: &vscErr},
		},
	}
	healthyVSC := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: vscName},
		Status:     &snapshotv1api.VolumeSnapshotContentStatus{SnapshotHandle: &handle},
	}
	emptyVSC := &snapshotv1api.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: vscName}}

	tests := []struct {
		name     string
		diag     string
		expected string
	}{
		{
			name:     "empty",
			diag:     "",
			expected: "",
		},
		{
			name:     "nothing notable",
			diag:     "begin diagnose restore exposer\n" + kube.DiagnosePVC(&corev1api.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: "fake-dd"}}, nil) + "end diagnose restore exposer",
			expected: "",
		},
		{
			name: "restore PVC can't be provisioned",
			diag: "begin diagnose restore exposer\n" + kube.DiagnosePod(pod, events) + kube.DiagnosePVC(pvc, events) + "end diagnose restore exposer",
			expected: "Pod condition PodScheduled, status False, reason Unschedulable, message 0/6 nodes are available: pod has unbound immediate PersistentVolumeClaims; " +
				"PVC event reason ProvisioningFailed, message failed to provision volume with StorageClass \"gp3-csi\": multi-attach is only supported for io2 volumes",
		},
		{
			name: "expose stopped before the backup pod was created",
			diag: "begin diagnose CSI exposer\n" +
				"error getting backup pod fake-du, err: pods \"fake-du\" not found\n" +
				"error getting backup pvc fake-du, err: persistentvolumeclaims \"fake-du\" not found\n" +
				csi.DiagnoseVS(healthyVS, nil) + csi.DiagnoseVSC(healthyVSC) +
				"end diagnose CSI exposer",
			expected: "error getting backup pod fake-du, err: pods \"fake-du\" not found; error getting backup pvc fake-du, err: persistentvolumeclaims \"fake-du\" not found",
		},
		{
			name:     "VS and VSC errors",
			diag:     "begin diagnose CSI exposer\n" + csi.DiagnoseVS(vs, nil) + csi.DiagnoseVSC(vsc) + csi.DiagnoseVSC(emptyVSC) + "end diagnose CSI exposer",
			expected: "VS velero/fake-du, bind to fake-vsc, readyToUse false, errMessage failed to create snapshot: quota exceeded; VSC fake-vsc, readyToUse false, errMessage rpc error: code = ResourceExhausted, handle fake-handle",
		},
		{
			name: "causes before get errors",
			diag: "begin diagnose CSI exposer\n" +
				"error getting backup pvc fake-du, err: persistentvolumeclaims \"fake-du\" not found\n" +
				kube.DiagnosePod(pod, nil) +
				"end diagnose CSI exposer",
			expected: "Pod condition PodScheduled, status False, reason Unschedulable, message 0/6 nodes are available: pod has unbound immediate PersistentVolumeClaims; " +
				"error getting backup pvc fake-du, err: persistentvolumeclaims \"fake-du\" not found",
		},
		{
			name:     "node-agent not running",
			diag:     "node-agent is not running in node fake-node, err: daemonset pod not found in running state in node fake-node\n",
			expected: "node-agent is not running in node fake-node, err: daemonset pod not found in running state in node fake-node",
		},
		{
			name:     "duplicated lines",
			diag:     "Pod event reason FailedScheduling, message x\nPod event reason FailedScheduling, message x\n",
			expected: "Pod event reason FailedScheduling, message x",
		},
		{
			name:     "too many lines",
			diag:     "error a\nerror b\nerror c\nerror d\nerror e\nerror f\n",
			expected: "error a; error b; error c; error d; error e",
		},
		{
			name:     "too long",
			diag:     "error " + strings.Repeat("x", 2000),
			expected: "error " + strings.Repeat("x", maxDiagnoseSummaryLen-len("error ")) + "...",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, SummarizeDiagnose(test.diag))
		})
	}
}
