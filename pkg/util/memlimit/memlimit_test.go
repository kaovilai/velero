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

package memlimit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestContainerMemoryLimit(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		expected  int64
		expectErr bool
	}{
		{
			name:     "no cgroup files",
			expected: 0,
		},
		{
			name:     "cgroup v2 limit",
			files:    map[string]string{"memory.max": "1073741824\n"},
			expected: 1073741824,
		},
		{
			name:     "cgroup v2 no limit",
			files:    map[string]string{"memory.max": "max\n"},
			expected: 0,
		},
		{
			name:     "cgroup v1 limit",
			files:    map[string]string{"memory/memory.limit_in_bytes": "536870912\n"},
			expected: 536870912,
		},
		{
			name:     "cgroup v1 no limit",
			files:    map[string]string{"memory/memory.limit_in_bytes": "9223372036854771712\n"},
			expected: 0,
		},
		{
			name: "cgroup v2 preferred over v1",
			files: map[string]string{
				"memory.max":                   "1073741824\n",
				"memory/memory.limit_in_bytes": "536870912\n",
			},
			expected: 1073741824,
		},
		{
			name:      "invalid value",
			files:     map[string]string{"memory.max": "abc\n"},
			expectErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, content := range tc.files {
				writeFile(t, filepath.Join(root, name), content)
			}

			limit, err := containerMemoryLimit(root)
			if tc.expectErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.expected, limit)
		})
	}
}

func TestSetFromContainerLimit(t *testing.T) {
	tests := []struct {
		name       string
		gomemlimit string
		memoryMax  string
		expectSet  bool
		expected   int64
	}{
		{
			name:      "set from container limit",
			memoryMax: "1073741824",
			expectSet: true,
			expected:  858993440,
		},
		{
			name:       "GOMEMLIMIT set by user",
			gomemlimit: "700MiB",
			memoryMax:  "1073741824",
		},
		{
			name:      "no container limit",
			memoryMax: "max",
		},
		{
			name:      "unreadable limit",
			memoryMax: "abc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOMEMLIMIT", tc.gomemlimit)

			root := t.TempDir()
			writeFile(t, filepath.Join(root, "memory.max"), tc.memoryMax)

			prevRoot, prevSet := cgroupRoot, setMemoryLimit
			defer func() { cgroupRoot, setMemoryLimit = prevRoot, prevSet }()

			cgroupRoot = root
			called := false
			var got int64
			setMemoryLimit = func(limit int64) int64 {
				called = true
				got = limit
				return 0
			}

			SetFromContainerLimit(logrus.New())

			assert.Equal(t, tc.expectSet, called)
			assert.Equal(t, tc.expected, got)
		})
	}
}
