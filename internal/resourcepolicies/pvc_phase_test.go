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
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPVCPhaseVolumePolicy(t *testing.T) {
	testCases := []struct {
		name     string
		yamlData string
		pvc      *corev1api.PersistentVolumeClaim
		skip     bool
	}{
		{
			name: "skip PVC in Pending phase",
			yamlData: `version: v1
volumePolicies:
- conditions:
    pvc:
      phase: Pending
  action:
    type: skip`,
			pvc: &corev1api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimPending,
				},
			},
			skip: true,
		},
		{
			name: "don't skip PVC in Bound phase when looking for Pending",
			yamlData: `version: v1
volumePolicies:
- conditions:
    pvc:
      phase: Pending
  action:
    type: skip`,
			pvc: &corev1api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimBound,
				},
			},
			skip: false,
		},
		{
			name: "skip PVC in Lost phase",
			yamlData: `version: v1
volumePolicies:
- conditions:
    pvc:
      phase: Lost
  action:
    type: skip`,
			pvc: &corev1api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimLost,
				},
			},
			skip: true,
		},
		{
			name: "skip PVC in Bound phase",
			yamlData: `version: v1
volumePolicies:
- conditions:
    pvc:
      phase: Bound
  action:
    type: skip`,
			pvc: &corev1api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimBound,
				},
			},
			skip: true,
		},
		{
			name: "complex policy with phase and labels",
			yamlData: `version: v1
volumePolicies:
- conditions:
    pvc:
      phase: Pending
    pvcLabels:
      environment: test
  action:
    type: skip`,
			pvc: &corev1api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
					Labels: map[string]string{
						"environment": "test",
						"app":         "web",
					},
				},
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimPending,
				},
			},
			skip: true,
		},
		{
			name: "complex policy with phase and labels - no match",
			yamlData: `version: v1
volumePolicies:
- conditions:
    pvc:
      phase: Pending
    pvcLabels:
      environment: prod
  action:
    type: skip`,
			pvc: &corev1api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
					Labels: map[string]string{
						"environment": "test",
						"app":         "web",
					},
				},
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimPending,
				},
			},
			skip: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resPolicies, err := unmarshalResourcePolicies(&tc.yamlData)
			require.NoError(t, err)
			
			policies := &Policies{}
			err = policies.BuildPolicy(resPolicies)
			require.NoError(t, err)

			err = policies.Validate()
			require.NoError(t, err)

			// Test with PVC-only (no PV - simulating unbound PVC case)
			vfd := NewVolumeFilterData(nil, nil, tc.pvc)
			action, err := policies.GetMatchAction(vfd)
			require.NoError(t, err)

			if tc.skip {
				require.NotNil(t, action, "Expected an action for test case %s", tc.name)
				assert.Equal(t, Skip, action.Type, "Expected skip action for test case %s", tc.name)
			} else {
				assert.Nil(t, action, "Expected no action for test case %s", tc.name)
			}
		})
	}
}

func TestPVCPhaseWithPVVolumePolicy(t *testing.T) {
	// Test cases where we have both PVC and PV (bound PVC case)
	testCases := []struct {
		name     string
		yamlData string
		pv       *corev1api.PersistentVolume
		pvc      *corev1api.PersistentVolumeClaim
		skip     bool
	}{
		{
			name: "bound PVC with PV - skip based on PVC phase",
			yamlData: `version: v1
volumePolicies:
- conditions:
    pvc:
      phase: Bound
  action:
    type: skip`,
			pv: &corev1api.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pv",
				},
				Spec: corev1api.PersistentVolumeSpec{
					Capacity: corev1api.ResourceList{
						corev1api.ResourceStorage: resource.MustParse("10Gi"),
					},
					StorageClassName: "standard",
				},
			},
			pvc: &corev1api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimBound,
				},
			},
			skip: true,
		},
		{
			name: "bound PVC with PV - no skip for different phase",
			yamlData: `version: v1
volumePolicies:
- conditions:
    pvc:
      phase: Pending
  action:
    type: skip`,
			pv: &corev1api.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pv",
				},
				Spec: corev1api.PersistentVolumeSpec{
					Capacity: corev1api.ResourceList{
						corev1api.ResourceStorage: resource.MustParse("10Gi"),
					},
					StorageClassName: "standard",
				},
			},
			pvc: &corev1api.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Status: corev1api.PersistentVolumeClaimStatus{
					Phase: corev1api.ClaimBound,
				},
			},
			skip: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resPolicies, err := unmarshalResourcePolicies(&tc.yamlData)
			require.NoError(t, err)
			
			policies := &Policies{}
			err = policies.BuildPolicy(resPolicies)
			require.NoError(t, err)

			err = policies.Validate()
			require.NoError(t, err)

			// Test with both PV and PVC (bound PVC case)
			vfd := NewVolumeFilterData(tc.pv, nil, tc.pvc)
			action, err := policies.GetMatchAction(vfd)
			require.NoError(t, err)

			if tc.skip {
				require.NotNil(t, action, "Expected an action for test case %s", tc.name)
				assert.Equal(t, Skip, action.Type, "Expected skip action for test case %s", tc.name)
			} else {
				assert.Nil(t, action, "Expected no action for test case %s", tc.name)
			}
		})
	}
}