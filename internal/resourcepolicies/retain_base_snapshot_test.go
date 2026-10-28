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
)

func TestRetainBaseSnapshotValidation(t *testing.T) {
	tests := []struct {
		name    string
		action  Action
		wantErr string
	}{
		{name: "YAML true with velero-block", action: Action{Type: Snapshot, Parameters: map[string]any{"retainBaseSnapshot": true, "dataMover": "velero-block"}}},
		{name: "string true with velero-block", action: Action{Type: Snapshot, Parameters: map[string]any{"retainBaseSnapshot": "true", "dataMover": "velero-block"}}},
		{name: "false with any data mover", action: Action{Type: Snapshot, Parameters: map[string]any{"retainBaseSnapshot": "false"}}},
		{name: "TRUE is rejected", action: Action{Type: Snapshot, Parameters: map[string]any{"retainBaseSnapshot": "TRUE", "dataMover": "velero-block"}}, wantErr: `got string "TRUE"`},
		{name: "1 is rejected", action: Action{Type: Snapshot, Parameters: map[string]any{"retainBaseSnapshot": 1, "dataMover": "velero-block"}}, wantErr: "got int 1"},
		{name: "true without dataMover", action: Action{Type: Snapshot, Parameters: map[string]any{"retainBaseSnapshot": true}}, wantErr: `it must be "velero-block"`},
		{name: "true with velero-fs", action: Action{Type: Snapshot, Parameters: map[string]any{"retainBaseSnapshot": true, "dataMover": "velero-fs"}}, wantErr: `is "velero-fs"`},
		{name: "on fs-backup", action: Action{Type: FSBackup, Parameters: map[string]any{"retainBaseSnapshot": false}}, wantErr: "only supported for the"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.action.validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestUnknownParameterWarnings(t *testing.T) {
	p := &Policies{}
	require.NoError(t, p.BuildPolicy(&ResourcePolicies{
		Version: "v1",
		VolumePolicies: []VolumePolicy{
			{Action: Action{Type: Snapshot, Parameters: map[string]any{"dataMover": "velero-block", "retainBaseSnapshots": true}}},
			{Action: Action{Type: Skip, Parameters: map[string]any{"whatever": 1}}},
		},
	}))
	warnings := p.UnknownParameterWarnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], `volume policy 0: unknown parameter "retainBaseSnapshots"`)
}

func TestMatchIndex(t *testing.T) {
	p := &Policies{}
	require.NoError(t, p.BuildPolicy(&ResourcePolicies{
		Version: "v1",
		VolumePolicies: []VolumePolicy{
			{Conditions: map[string]any{"storageClass": []string{"other"}}, Action: Action{Type: Skip}},
			{Conditions: map[string]any{"storageClass": []string{"gold"}}, Action: Action{Type: Snapshot}},
		},
	}))
	action := p.match(&structuredVolume{storageClass: "gold"})
	require.NotNil(t, action)
	assert.Equal(t, 1, action.Index())
}
