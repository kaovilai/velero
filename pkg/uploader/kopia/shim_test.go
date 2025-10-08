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

package kopia

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/vmware-tanzu/velero/pkg/repository/udmrepo"
	"github.com/vmware-tanzu/velero/pkg/repository/udmrepo/mocks"
)

func TestShimRepo(t *testing.T) {
	ctx := t.Context()
	backupRepo := &mocks.BackupRepo{}
	backupRepo.On("Time").Return(time.Time{})
	shim := NewShimRepo(backupRepo)
	// All below calls put together for the implementation are empty or just very simple, and just want to cover testing
	// If wanting to write unit tests for some functions could remove it and with writing new function alone
	shim.VerifyObject(ctx, object.ID{})
	shim.Time()
	shim.ClientOptions()
	shim.Refresh(ctx)
	shim.ContentInfo(ctx, content.ID{})
	shim.PrefetchContents(ctx, []content.ID{}, "hint")
	shim.PrefetchObjects(ctx, []object.ID{}, "hint")
	shim.UpdateDescription("desc")
	shim.NewWriter(ctx, repo.WriteSessionOptions{})
	shim.OnSuccessfulFlush(func(ctx context.Context, w repo.RepositoryWriter) error { return nil })

	backupRepo.On("Close", mock.Anything).Return(nil)
	NewShimRepo(backupRepo).Close(ctx)

	var id udmrepo.ID
	backupRepo.On("PutManifest", mock.Anything, mock.Anything).Return(id, nil)
	NewShimRepo(backupRepo).PutManifest(ctx, map[string]string{}, nil)

	var mf manifest.ID
	backupRepo.On("DeleteManifest", mock.Anything, mock.Anything).Return(nil)
	NewShimRepo(backupRepo).DeleteManifest(ctx, mf)

	backupRepo.On("Flush", mock.Anything).Return(nil)
	NewShimRepo(backupRepo).Flush(ctx)

	backupRepo.On("NewObjectWriter", mock.Anything, mock.Anything).Return(nil)
	NewShimRepo(backupRepo).NewObjectWriter(ctx, object.WriterOptions{})
}

func TestOpenObject(t *testing.T) {
	tests := []struct {
		name              string
		backupRepo        *mocks.BackupRepo
		isOpenObjectError bool
		isReaderNil       bool
	}{
		{
			name: "Success",
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("OpenObject", mock.Anything, mock.Anything).Return(&shimObjectReader{}, nil)
				return backupRepo
			}(),
		},
		{
			name: "Open object error",
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("OpenObject", mock.Anything, mock.Anything).Return(&shimObjectReader{}, errors.New("Error open object"))
				return backupRepo
			}(),
			isOpenObjectError: true,
		},
		{
			name: "Get nil reader",
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("OpenObject", mock.Anything, mock.Anything).Return(nil, nil)
				return backupRepo
			}(),
			isReaderNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			reader, err := NewShimRepo(tc.backupRepo).OpenObject(ctx, object.ID{})
			if tc.isOpenObjectError {
				require.ErrorContains(t, err, "failed to open object")
			} else if tc.isReaderNil {
				assert.Nil(t, reader)
			} else {
				assert.NotNil(t, reader)
				assert.NoError(t, err)
			}
		})
	}
}

func TestFindManifests(t *testing.T) {
	meta := []*udmrepo.ManifestEntryMetadata{}
	tests := []struct {
		name               string
		backupRepo         *mocks.BackupRepo
		isGetManifestError bool
	}{
		{
			name: "Success",
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("FindManifests", mock.Anything, mock.Anything).Return(meta, nil)
				return backupRepo
			}(),
		},
		{
			name:               "Failed to find manifest",
			isGetManifestError: true,
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("FindManifests", mock.Anything, mock.Anything).Return(meta,
					errors.New("failed to find manifest"))
				return backupRepo
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			_, err := NewShimRepo(tc.backupRepo).FindManifests(ctx, map[string]string{})
			if tc.isGetManifestError {
				require.ErrorContains(t, err, "failed")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestShimObjReader(t *testing.T) {
	reader := new(shimObjectReader)
	objReader := &mocks.ObjectReader{}
	reader.repoReader = objReader
	// All below calls put together for the implementation are empty or just very simple, and just want to cover testing
	// If wanting to write unit tests for some functions could remove it and with writing new function alone
	objReader.On("Seek", mock.Anything, mock.Anything).Return(int64(0), nil)
	reader.Seek(int64(0), 0)

	objReader.On("Read", mock.Anything).Return(0, nil)
	reader.Read(nil)

	objReader.On("Close").Return(nil)
	reader.Close()

	objReader.On("Length").Return(int64(0))
	reader.Length()
}

func TestShimObjWriter(t *testing.T) {
	writer := new(shimObjectWriter)
	objWriter := &mocks.ObjectWriter{}
	writer.repoWriter = objWriter
	// All below calls put together for the implementation are empty or just very simple, and just want to cover testing
	// If wanting to write unit tests for some functions could remove it and with writing new function alone
	var id udmrepo.ID
	objWriter.On("Checkpoint").Return(id, nil)
	writer.Checkpoint()

	objWriter.On("Result").Return(id, nil)
	writer.Result()

	objWriter.On("Write", mock.Anything).Return(0, nil)
	writer.Write(nil)

	objWriter.On("Close").Return(nil)
	writer.Close()
}

func TestReplaceManifests(t *testing.T) {
	meta1 := udmrepo.ManifestEntryMetadata{
		ID: "mani-1",
	}

	meta2 := udmrepo.ManifestEntryMetadata{
		ID: "mani-2",
	}

	tests := []struct {
		name               string
		backupRepo         *mocks.BackupRepo
		isGetManifestError bool
		expectedError      string
		expectedID         manifest.ID
	}{
		{
			name:               "Failed to find manifest",
			isGetManifestError: true,
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("FindManifests", mock.Anything, mock.Anything).Return([]*udmrepo.ManifestEntryMetadata{},
					errors.New("fake-find-error"))
				return backupRepo
			}(),
			expectedError: "unable to load manifests: failed to get manifests with labels map[]: fake-find-error",
		},
		{
			name:               "Failed to delete manifest",
			isGetManifestError: true,
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("FindManifests", mock.Anything, mock.Anything).Return([]*udmrepo.ManifestEntryMetadata{
					&meta1,
					&meta2,
				}, nil)
				backupRepo.On("Time").Return(time.Now())
				backupRepo.On("DeleteManifest", mock.Anything, mock.Anything).Return(errors.New("fake-delete-error"))
				return backupRepo
			}(),
			expectedError: "unable to delete previous manifest mani-1: fake-delete-error",
		},
		{
			name: "Failed to put manifest",
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("FindManifests", mock.Anything, mock.Anything).Return([]*udmrepo.ManifestEntryMetadata{
					&meta1,
					&meta2,
				}, nil)
				backupRepo.On("Time").Return(time.Now())
				backupRepo.On("DeleteManifest", mock.Anything, mock.Anything).Return(nil)
				backupRepo.On("PutManifest", mock.Anything, mock.Anything).Return(udmrepo.ID(""), errors.New("fake-put-error"))
				return backupRepo
			}(),
			expectedError: "fake-put-error",
		},
		{
			name: "Success",
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("FindManifests", mock.Anything, mock.Anything).Return([]*udmrepo.ManifestEntryMetadata{
					&meta1,
					&meta2,
				}, nil)
				backupRepo.On("Time").Return(time.Now())
				backupRepo.On("DeleteManifest", mock.Anything, mock.Anything).Return(nil)
				backupRepo.On("PutManifest", mock.Anything, mock.Anything).Return(udmrepo.ID("fake-id"), nil)
				return backupRepo
			}(),
			expectedID: manifest.ID("fake-id"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			id, err := NewShimRepo(tc.backupRepo).ReplaceManifests(ctx, map[string]string{}, nil)

			if tc.expectedError != "" {
				require.EqualError(t, err, tc.expectedError)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tc.expectedID, id)
		})
	}
}

func TestConcatenateObjects(t *testing.T) {
	tests := []struct {
		name          string
		backupRepo    *mocks.BackupRepo
		objectIDs     []object.ID
		expectedError string
	}{
		{
			name:          "empty object list",
			expectedError: "object list is empty",
		},
		{
			name: "concatenate error",
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("ConcatenateObjects", mock.Anything, mock.Anything).Return(udmrepo.ID(""), errors.New("fake-concatenate-error"))
				return backupRepo
			}(),
			objectIDs: []object.ID{
				{},
			},
			expectedError: "fake-concatenate-error",
		},
		{
			name: "parse error",
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("ConcatenateObjects", mock.Anything, mock.Anything).Return(udmrepo.ID("fake-id"), nil)
				return backupRepo
			}(),
			objectIDs: []object.ID{
				{},
			},
			expectedError: "malformed content ID: \"fake-id\": invalid content prefix",
		},
		{
			name: "success",
			backupRepo: func() *mocks.BackupRepo {
				backupRepo := &mocks.BackupRepo{}
				backupRepo.On("ConcatenateObjects", mock.Anything, mock.Anything).Return(udmrepo.ID("I123456"), nil)
				return backupRepo
			}(),
			objectIDs: []object.ID{
				{},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			_, err := NewShimRepo(tc.backupRepo).ConcatenateObjects(ctx, tc.objectIDs, repo.ConcatenateOptions{})

			if tc.expectedError != "" {
				assert.EqualError(t, err, tc.expectedError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestNewShimRepoWithCBT tests CBT-aware repository creation
func TestNewShimRepoWithCBT(t *testing.T) {
	tests := []struct {
		name       string
		cbtEnabled bool
	}{
		{
			name:       "CBT enabled",
			cbtEnabled: true,
		},
		{
			name:       "CBT disabled",
			cbtEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backupRepo := &mocks.BackupRepo{}
			shimRepo := NewShimRepoWithCBT(backupRepo, tt.cbtEnabled)

			assert.NotNil(t, shimRepo)

			// Verify the shim repository has the correct CBT flag
			sr, ok := shimRepo.(*shimRepository)
			require.True(t, ok, "Should be able to cast to shimRepository")
			assert.Equal(t, tt.cbtEnabled, sr.cbtEnabled)
		})
	}
}

// TestNewShimRepo_DefaultsCBTDisabled tests that default constructor has CBT disabled
func TestNewShimRepo_DefaultsCBTDisabled(t *testing.T) {
	backupRepo := &mocks.BackupRepo{}
	shimRepo := NewShimRepo(backupRepo)

	sr, ok := shimRepo.(*shimRepository)
	require.True(t, ok)
	assert.False(t, sr.cbtEnabled, "Default NewShimRepo should have CBT disabled")
}

// TestShimObjectWriter_CBTEnabled tests zero-block optimization with CBT enabled
func TestShimObjectWriter_CBTEnabled(t *testing.T) {
	tests := []struct {
		name           string
		data           []byte
		cbtEnabled     bool
		expectWrite    bool
		expectedBytes  int
		description    string
	}{
		{
			name:           "CBT enabled - zero block above threshold",
			data:           make([]byte, 8192), // 8KB of zeros
			cbtEnabled:     true,
			expectWrite:    false, // Should skip write
			expectedBytes:  8192,  // But should return successful write count
			description:    "Should skip writing large zero blocks when CBT is enabled",
		},
		{
			name:           "CBT enabled - non-zero block",
			data:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			cbtEnabled:     true,
			expectWrite:    true, // Should write
			expectedBytes:  10,
			description:    "Should write non-zero data even when CBT is enabled",
		},
		{
			name:           "CBT disabled - zero block",
			data:           make([]byte, 8192), // 8KB of zeros
			cbtEnabled:     false,
			expectWrite:    true, // Should write
			expectedBytes:  8192,
			description:    "Should write zero blocks when CBT is disabled",
		},
		{
			name:           "CBT enabled - small zero block below threshold",
			data:           make([]byte, 1024), // 1KB of zeros (below 4KB threshold)
			cbtEnabled:     true,
			expectWrite:    true, // Should write (below threshold)
			expectedBytes:  1024,
			description:    "Should write small zero blocks even when CBT is enabled",
		},
		{
			name:           "CBT enabled - exactly at threshold",
			data:           make([]byte, 4096), // Exactly 4KB
			cbtEnabled:     true,
			expectWrite:    false, // Should skip (at threshold)
			expectedBytes:  4096,
			description:    "Should skip zero blocks exactly at threshold when CBT is enabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objWriter := &mocks.ObjectWriter{}

			if tt.expectWrite {
				objWriter.On("Write", tt.data).Return(tt.expectedBytes, nil).Once()
			}

			shimWriter := &shimObjectWriter{
				repoWriter: objWriter,
				cbtEnabled: tt.cbtEnabled,
			}

			n, err := shimWriter.Write(tt.data)

			require.NoError(t, err)
			assert.Equal(t, tt.expectedBytes, n, tt.description)

			if tt.expectWrite {
				objWriter.AssertExpectations(t)
			} else {
				// Verify Write was NOT called on the underlying writer
				objWriter.AssertNotCalled(t, "Write", mock.Anything)
			}
		})
	}
}

// TestIsAllZeros tests the zero detection helper function
func TestIsAllZeros(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected bool
	}{
		{
			name:     "all zeros",
			data:     make([]byte, 1024),
			expected: true,
		},
		{
			name:     "single non-zero at start",
			data:     append([]byte{1}, make([]byte, 1023)...),
			expected: false,
		},
		{
			name:     "single non-zero at end",
			data:     append(make([]byte, 1023), 1),
			expected: false,
		},
		{
			name:     "single non-zero in middle",
			data:     func() []byte {
				d := make([]byte, 1024)
				d[512] = 1
				return d
			}(),
			expected: false,
		},
		{
			name:     "all non-zero",
			data:     []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			expected: false,
		},
		{
			name:     "empty slice",
			data:     []byte{},
			expected: true,
		},
		{
			name:     "single zero byte",
			data:     []byte{0},
			expected: true,
		},
		{
			name:     "single non-zero byte",
			data:     []byte{1},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAllZeros(tt.data)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestShimObjectWriter_NewObjectWriterPropagatesCBT tests that NewObjectWriter propagates CBT flag
func TestShimObjectWriter_NewObjectWriterPropagatesCBT(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		cbtEnabled bool
	}{
		{
			name:       "propagates CBT enabled",
			cbtEnabled: true,
		},
		{
			name:       "propagates CBT disabled",
			cbtEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objWriter := &mocks.ObjectWriter{}
			backupRepo := &mocks.BackupRepo{}
			backupRepo.On("NewObjectWriter", mock.Anything, mock.Anything).Return(objWriter)

			shimRepo := NewShimRepoWithCBT(backupRepo, tt.cbtEnabled)

			writer := shimRepo.NewObjectWriter(ctx, object.WriterOptions{
				Description: "test",
			})

			require.NotNil(t, writer)

			// Verify the writer has the correct CBT flag
			sw, ok := writer.(*shimObjectWriter)
			require.True(t, ok, "Should be able to cast to shimObjectWriter")
			assert.Equal(t, tt.cbtEnabled, sw.cbtEnabled, "CBT flag should be propagated to writer")
		})
	}
}

// TestShimObjectWriter_ZeroBlockThreshold tests boundary conditions for threshold
func TestShimObjectWriter_ZeroBlockThreshold(t *testing.T) {
	objWriter := &mocks.ObjectWriter{}

	tests := []struct {
		name        string
		size        int
		expectWrite bool
	}{
		{
			name:        "below threshold - 1 byte",
			size:        1,
			expectWrite: true,
		},
		{
			name:        "below threshold - 4095 bytes",
			size:        4095,
			expectWrite: true,
		},
		{
			name:        "at threshold - 4096 bytes",
			size:        4096,
			expectWrite: false,
		},
		{
			name:        "above threshold - 4097 bytes",
			size:        4097,
			expectWrite: false,
		},
		{
			name:        "above threshold - 1MB",
			size:        1024 * 1024,
			expectWrite: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, tt.size)

			if tt.expectWrite {
				objWriter.On("Write", data).Return(tt.size, nil).Once()
			}

			shimWriter := &shimObjectWriter{
				repoWriter: objWriter,
				cbtEnabled: true,
			}

			n, err := shimWriter.Write(data)

			require.NoError(t, err)
			assert.Equal(t, tt.size, n)

			if tt.expectWrite {
				objWriter.AssertExpectations(t)
			} else {
				objWriter.AssertNotCalled(t, "Write", mock.Anything)
			}

			// Reset mock for next iteration
			objWriter.ExpectedCalls = nil
			objWriter.Calls = nil
		})
	}
}

// TestShimObjectWriter_CBTDisabledWritesAllData tests that CBT disabled preserves all data
func TestShimObjectWriter_CBTDisabledWritesAllData(t *testing.T) {
	// This is critical: when CBT is disabled, even zero blocks must be written
	// because they could be legitimate data

	testData := []struct {
		name string
		data []byte
	}{
		{
			name: "large zero block",
			data: make([]byte, 1024*1024), // 1MB zeros
		},
		{
			name: "mixed data with zeros",
			data: func() []byte {
				d := make([]byte, 8192)
				d[0] = 1
				d[8191] = 1
				return d
			}(),
		},
		{
			name: "all zeros at threshold",
			data: make([]byte, 4096),
		},
	}

	for _, td := range testData {
		t.Run(td.name, func(t *testing.T) {
			objWriter := &mocks.ObjectWriter{}
			objWriter.On("Write", td.data).Return(len(td.data), nil).Once()

			shimWriter := &shimObjectWriter{
				repoWriter: objWriter,
				cbtEnabled: false, // CBT disabled
			}

			n, err := shimWriter.Write(td.data)

			require.NoError(t, err)
			assert.Equal(t, len(td.data), n)
			objWriter.AssertExpectations(t)
			objWriter.AssertCalled(t, "Write", td.data)
		})
	}
}
