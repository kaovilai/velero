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

package csi

import (
	"context"
	"testing"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	fakesnapshotclientset "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewCBTFetcher(t *testing.T) {
	fakeClient := fakesnapshotclientset.NewSimpleClientset()
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	assert.NotNil(t, fetcher)
	assert.NotNil(t, fetcher.snapshotClient)
}

func TestSupportsCBT_WithAnnotation(t *testing.T) {
	ctx := context.Background()

	// Create a VolumeSnapshotClass with CBT support annotation
	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
			Annotations: map[string]string{
				CBTSupportAnnotation: "true",
			},
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	// Create a VolumeSnapshot referencing the class
	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
	}

	supported, err := fetcher.SupportsCBT(ctx, snapshot)
	require.NoError(t, err)
	assert.True(t, supported, "Should detect CBT support from annotation")
}

func TestSupportsCBT_WithoutAnnotation(t *testing.T) {
	ctx := context.Background()

	// Create a VolumeSnapshotClass without CBT support annotation
	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
			// No annotations
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
	}

	supported, err := fetcher.SupportsCBT(ctx, snapshot)
	require.NoError(t, err)
	assert.False(t, supported, "Should not detect CBT support without annotation")
}

func TestSupportsCBT_AnnotationFalse(t *testing.T) {
	ctx := context.Background()

	// Create a VolumeSnapshotClass with CBT support annotation set to false
	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
			Annotations: map[string]string{
				CBTSupportAnnotation: "false",
			},
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
	}

	supported, err := fetcher.SupportsCBT(ctx, snapshot)
	require.NoError(t, err)
	assert.False(t, supported, "Should not detect CBT support when annotation is false")
}

func TestSupportsCBT_NoClassName(t *testing.T) {
	ctx := context.Background()

	fakeClient := fakesnapshotclientset.NewSimpleClientset()
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	// Snapshot without VolumeSnapshotClassName
	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: nil,
		},
	}

	supported, err := fetcher.SupportsCBT(ctx, snapshot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no VolumeSnapshotClassName")
	assert.False(t, supported)
}

func TestSupportsCBT_ClassNotFound(t *testing.T) {
	ctx := context.Background()

	fakeClient := fakesnapshotclientset.NewSimpleClientset()
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	snapshotClassName := "non-existent-class"
	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
	}

	supported, err := fetcher.SupportsCBT(ctx, snapshot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get VolumeSnapshotClass")
	assert.False(t, supported)
}

func TestFetchCBTMetadata_NotSupported(t *testing.T) {
	ctx := context.Background()

	// Create a VolumeSnapshotClass without CBT support
	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
	}

	metadata, err := fetcher.FetchCBTMetadata(ctx, snapshot, "previous-snapshot-handle")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support CBT")
	assert.Nil(t, metadata)
}

func TestFetchCBTMetadata_NoSnapshotStatus(t *testing.T) {
	ctx := context.Background()

	// Create a VolumeSnapshotClass with CBT support
	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
			Annotations: map[string]string{
				CBTSupportAnnotation: "true",
			},
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	// Snapshot without status
	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
		Status: nil,
	}

	metadata, err := fetcher.FetchCBTMetadata(ctx, snapshot, "previous-snapshot-handle")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not bound to VolumeSnapshotContent")
	assert.Nil(t, metadata)
}

func TestFetchCBTMetadata_NoBoundContent(t *testing.T) {
	ctx := context.Background()

	// Create a VolumeSnapshotClass with CBT support
	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
			Annotations: map[string]string{
				CBTSupportAnnotation: "true",
			},
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	// Snapshot with status but no bound content
	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
		Status: &snapshotv1api.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: nil,
		},
	}

	metadata, err := fetcher.FetchCBTMetadata(ctx, snapshot, "previous-snapshot-handle")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not bound to VolumeSnapshotContent")
	assert.Nil(t, metadata)
}

func TestFetchCBTMetadata_ContentNotFound(t *testing.T) {
	ctx := context.Background()

	// Create a VolumeSnapshotClass with CBT support
	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
			Annotations: map[string]string{
				CBTSupportAnnotation: "true",
			},
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	contentName := "non-existent-content"
	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
		Status: &snapshotv1api.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: &contentName,
		},
	}

	metadata, err := fetcher.FetchCBTMetadata(ctx, snapshot, "previous-snapshot-handle")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get VolumeSnapshotContent")
	assert.Nil(t, metadata)
}

func TestFetchCBTMetadata_NoSnapshotHandle(t *testing.T) {
	ctx := context.Background()

	// Create a VolumeSnapshotClass with CBT support
	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
			Annotations: map[string]string{
				CBTSupportAnnotation: "true",
			},
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	contentName := "test-content"
	content := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: contentName,
		},
		Spec: snapshotv1api.VolumeSnapshotContentSpec{
			VolumeSnapshotRef: snapshotv1api.ObjectReference{
				Name:      "test-snapshot",
				Namespace: "default",
			},
			VolumeSnapshotClassName: &snapshotClassName,
			Driver:                  "test-driver",
			Source: snapshotv1api.VolumeSnapshotContentSource{
				SnapshotHandle: nil,
			},
			DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
		},
		Status: nil, // No status
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass, content)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
		Status: &snapshotv1api.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: &contentName,
		},
	}

	metadata, err := fetcher.FetchCBTMetadata(ctx, snapshot, "previous-snapshot-handle")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no snapshot handle")
	assert.Nil(t, metadata)
}

func TestFetchCBTMetadata_APINotAvailable(t *testing.T) {
	ctx := context.Background()

	// Create a complete setup with CBT support
	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
			Annotations: map[string]string{
				CBTSupportAnnotation: "true",
			},
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	snapshotHandle := "snapshot-123"
	contentName := "test-content"
	content := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: contentName,
		},
		Spec: snapshotv1api.VolumeSnapshotContentSpec{
			VolumeSnapshotRef: snapshotv1api.ObjectReference{
				Name:      "test-snapshot",
				Namespace: "default",
			},
			VolumeSnapshotClassName: &snapshotClassName,
			Driver:                  "test-driver",
			Source: snapshotv1api.VolumeSnapshotContentSource{
				SnapshotHandle: &snapshotHandle,
			},
			DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
		},
		Status: &snapshotv1api.VolumeSnapshotContentStatus{
			SnapshotHandle: &snapshotHandle,
		},
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass, content)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
		Status: &snapshotv1api.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: &contentName,
		},
	}

	previousHandle := "previous-snapshot-handle"
	metadata, err := fetcher.FetchCBTMetadata(ctx, snapshot, previousHandle)

	// Should return error indicating API is not available yet
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CBT metadata API not yet available")
	assert.Contains(t, err.Error(), snapshotHandle)
	assert.Contains(t, err.Error(), previousHandle)
	assert.Nil(t, metadata)
}

// TestByteRange tests the ByteRange structure
func TestByteRange(t *testing.T) {
	tests := []struct {
		name   string
		ranges []ByteRange
	}{
		{
			name: "single range",
			ranges: []ByteRange{
				{Offset: 0, Length: 1024},
			},
		},
		{
			name: "multiple ranges",
			ranges: []ByteRange{
				{Offset: 0, Length: 512},
				{Offset: 1024, Length: 512},
				{Offset: 2048, Length: 1024},
			},
		},
		{
			name:   "empty ranges",
			ranges: []ByteRange{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotNil(t, tt.ranges)

			// Verify ranges are sorted and non-overlapping
			for i := 0; i < len(tt.ranges)-1; i++ {
				end := tt.ranges[i].Offset + tt.ranges[i].Length
				start := tt.ranges[i+1].Offset
				assert.LessOrEqual(t, end, start, "Ranges should not overlap")
			}
		})
	}
}

// TestCBTMetadata tests the CBTMetadata structure
func TestCBTMetadata(t *testing.T) {
	metadata := &CBTMetadata{
		ChangedRanges: []ByteRange{
			{Offset: 0, Length: 512},
			{Offset: 1024, Length: 512},
		},
		BaseSnapshot: "base-snapshot-handle",
		TotalSize:    10 * 1024 * 1024, // 10MB
	}

	assert.NotNil(t, metadata)
	assert.Equal(t, 2, len(metadata.ChangedRanges))
	assert.Equal(t, "base-snapshot-handle", metadata.BaseSnapshot)
	assert.Equal(t, int64(10*1024*1024), metadata.TotalSize)

	// Calculate total changed bytes
	totalChanged := int64(0)
	for _, r := range metadata.ChangedRanges {
		totalChanged += r.Length
	}
	assert.Equal(t, int64(1024), totalChanged)

	// Calculate change percentage
	changePercent := float64(totalChanged) / float64(metadata.TotalSize) * 100
	assert.InDelta(t, 0.0097, changePercent, 0.001) // ~0.01%
}

// TestCBTSupportAnnotation verifies the annotation constant
func TestCBTSupportAnnotation(t *testing.T) {
	assert.Equal(t, "velero.io/cbt-support", CBTSupportAnnotation)
}

// TestCBTFetcher_MultipleAnnotations tests handling of multiple annotations
func TestCBTFetcher_MultipleAnnotations(t *testing.T) {
	ctx := context.Background()

	snapshotClassName := "test-snapshot-class"
	snapClass := &snapshotv1api.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotClassName,
			Annotations: map[string]string{
				CBTSupportAnnotation:   "true",
				"other-annotation":     "value",
				"velero.io/other-prop": "value2",
			},
		},
		Driver:         "test-driver",
		DeletionPolicy: snapshotv1api.VolumeSnapshotContentDelete,
	}

	fakeClient := fakesnapshotclientset.NewSimpleClientset(snapClass)
	fetcher := NewCBTFetcher(fakeClient.SnapshotV1())

	snapshot := &snapshotv1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-snapshot",
			Namespace: "default",
		},
		Spec: snapshotv1api.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
		},
	}

	supported, err := fetcher.SupportsCBT(ctx, snapshot)
	require.NoError(t, err)
	assert.True(t, supported, "Should detect CBT support even with other annotations present")
}
