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
package resourcepolicies

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestPVCPhaseMatch(t *testing.T) {
	tests := []struct {
		name          string
		condition     *pvcPhaseCondition
		volume        *structuredVolume
		expectedMatch bool
	}{
		{
			name: "match Pending phase",
			condition: &pvcPhaseCondition{
				phase: "Pending",
			},
			volume: setStructuredVolumeWithPhase(
				*resource.NewQuantity(0, resource.BinarySI),
				"",
				nil,
				nil,
				nil,
				"Pending",
			),
			expectedMatch: true,
		},
		{
			name: "match Bound phase",
			condition: &pvcPhaseCondition{
				phase: "Bound",
			},
			volume: setStructuredVolumeWithPhase(
				*resource.NewQuantity(0, resource.BinarySI),
				"",
				nil,
				nil,
				nil,
				"Bound",
			),
			expectedMatch: true,
		},
		{
			name: "match Lost phase",
			condition: &pvcPhaseCondition{
				phase: "Lost",
			},
			volume: setStructuredVolumeWithPhase(
				*resource.NewQuantity(0, resource.BinarySI),
				"",
				nil,
				nil,
				nil,
				"Lost",
			),
			expectedMatch: true,
		},
		{
			name: "mismatch phase",
			condition: &pvcPhaseCondition{
				phase: "Pending",
			},
			volume: setStructuredVolumeWithPhase(
				*resource.NewQuantity(0, resource.BinarySI),
				"",
				nil,
				nil,
				nil,
				"Bound",
			),
			expectedMatch: false,
		},
		{
			name: "empty phase condition matches any",
			condition: &pvcPhaseCondition{
				phase: "",
			},
			volume: setStructuredVolumeWithPhase(
				*resource.NewQuantity(0, resource.BinarySI),
				"",
				nil,
				nil,
				nil,
				"Pending",
			),
			expectedMatch: true,
		},
		{
			name: "empty phase condition matches empty phase",
			condition: &pvcPhaseCondition{
				phase: "",
			},
			volume: setStructuredVolumeWithPhase(
				*resource.NewQuantity(0, resource.BinarySI),
				"",
				nil,
				nil,
				nil,
				"",
			),
			expectedMatch: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.condition.match(tc.volume)
			assert.Equal(t, tc.expectedMatch, result, "Expected match result to be %v", tc.expectedMatch)
		})
	}
}

func TestParsePVC(t *testing.T) {
	tests := []struct {
		name           string
		pvc            *corev1api.PersistentVolumeClaim
		expectedPhase  string
		expectedLabels map[string]string
	}{
		{
			name: "PVC with Pending phase",
			pvc: &corev1api.PersistentVolumeClaim{
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimPending,
				},
			},
			expectedPhase:  "Pending",
			expectedLabels: nil,
		},
		{
			name: "PVC with Bound phase",
			pvc: &corev1api.PersistentVolumeClaim{
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimBound,
				},
			},
			expectedPhase:  "Bound",
			expectedLabels: nil,
		},
		{
			name: "PVC with Lost phase",
			pvc: &corev1api.PersistentVolumeClaim{
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimLost,
				},
			},
			expectedPhase:  "Lost",
			expectedLabels: nil,
		},
		{
			name: "PVC with phase and labels",
			pvc: &corev1api.PersistentVolumeClaim{
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimBound,
				},
				Labels: map[string]string{
					"environment": "production",
					"app":         "database",
				},
			},
			expectedPhase: "Bound",
			expectedLabels: map[string]string{
				"environment": "production",
				"app":         "database",
			},
		},
		{
			name:           "nil PVC",
			pvc:            nil,
			expectedPhase:  "",
			expectedLabels: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			volume := &structuredVolume{}
			volume.parsePVC(tc.pvc)

			assert.Equal(t, tc.expectedPhase, volume.pvcPhase, "Expected PVC phase to be %s", tc.expectedPhase)
			assert.Equal(t, tc.expectedLabels, volume.pvcLabels, "Expected PVC labels to match")
		})
	}
}