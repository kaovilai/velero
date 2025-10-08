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

	"github.com/pkg/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapshotter "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/typed/volumesnapshot/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CBTSupportAnnotation is the annotation key that indicates CBT support in VolumeSnapshotClass
	// This is a placeholder until the official Kubernetes CBT API is stabilized
	CBTSupportAnnotation = "velero.io/cbt-support"
)

// ByteRange represents a changed block range with offset and length
type ByteRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// CBTMetadata contains changed block tracking metadata for a snapshot
type CBTMetadata struct {
	ChangedRanges []ByteRange `json:"changedRanges"`
	BaseSnapshot  string      `json:"baseSnapshot"`  // Previous snapshot handle
	TotalSize     int64       `json:"totalSize"`     // Total volume size
}

// CBTFetcher handles fetching Changed Block Tracking metadata
type CBTFetcher struct {
	snapshotClient snapshotter.SnapshotV1Interface
}

// NewCBTFetcher creates a new CBT fetcher
func NewCBTFetcher(snapshotClient snapshotter.SnapshotV1Interface) *CBTFetcher {
	return &CBTFetcher{
		snapshotClient: snapshotClient,
	}
}

// SupportsCBT checks if the CSI driver for the snapshot supports CBT
// Currently checks for annotation-based support until K8s CBT API is stable
func (f *CBTFetcher) SupportsCBT(ctx context.Context, snapshot *snapshotv1api.VolumeSnapshot) (bool, error) {
	if snapshot.Spec.VolumeSnapshotClassName == nil {
		return false, errors.New("snapshot has no VolumeSnapshotClassName")
	}

	// Get the VolumeSnapshotClass to check for CBT support
	snapClass, err := f.snapshotClient.VolumeSnapshotClasses().Get(
		ctx,
		*snapshot.Spec.VolumeSnapshotClassName,
		metav1.GetOptions{},
	)
	if err != nil {
		return false, errors.Wrap(err, "failed to get VolumeSnapshotClass")
	}

	// Check for CBT support annotation
	// TODO: Update to use SnapshotMetadataService CR detection when K8s CBT API is GA
	// See: https://github.com/kubernetes/enhancements/tree/master/keps/sig-storage/3314-csi-changed-block-tracking
	if snapClass.Annotations != nil {
		if val, ok := snapClass.Annotations[CBTSupportAnnotation]; ok && val == "true" {
			return true, nil
		}
	}

	return false, nil
}

// FetchCBTMetadata retrieves changed block tracking metadata for a snapshot
// Currently returns placeholder implementation - will be replaced with actual
// SnapshotMetadataService.GetMetadataDelta call when K8s CBT API is available
func (f *CBTFetcher) FetchCBTMetadata(ctx context.Context,
	currentSnapshot *snapshotv1api.VolumeSnapshot,
	previousSnapshotHandle string) (*CBTMetadata, error) {

	// Check if CSI driver supports CBT
	supported, err := f.SupportsCBT(ctx, currentSnapshot)
	if err != nil {
		return nil, errors.Wrap(err, "failed to check CBT support")
	}
	if !supported {
		return nil, errors.New("CSI driver does not support CBT")
	}

	if currentSnapshot.Status == nil || currentSnapshot.Status.BoundVolumeSnapshotContentName == nil {
		return nil, errors.New("snapshot not bound to VolumeSnapshotContent")
	}

	// Get the current snapshot's handle from VolumeSnapshotContent
	vsc, err := f.snapshotClient.VolumeSnapshotContents().Get(
		ctx,
		*currentSnapshot.Status.BoundVolumeSnapshotContentName,
		metav1.GetOptions{},
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get VolumeSnapshotContent")
	}

	if vsc.Status == nil || vsc.Status.SnapshotHandle == nil {
		return nil, errors.New("VolumeSnapshotContent has no snapshot handle")
	}

	currentHandle := *vsc.Status.SnapshotHandle

	// TODO: Implement actual SnapshotMetadataService.GetMetadataDelta call when API is available
	// For now, return error indicating CBT API is not yet available
	return nil, errors.Errorf("CBT metadata API not yet available - current snapshot: %s, previous: %s",
		currentHandle, previousSnapshotHandle)

	// Future implementation when K8s CBT API is GA:
	/*
		metaClient := ... // SnapshotMetadataService client
		req := &metadataapi.GetMetadataDeltaRequest{
			SecurityToken:        "", // Set if required by CSI driver
			BaseSnapshotID:       previousSnapshotHandle,
			TargetSnapshotID:     currentHandle,
			StartingOffset:       0,
			MaxResults:           0, // 0 means return all changed blocks
		}

		resp, err := metaClient.GetMetadataDelta(ctx, req)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get CBT metadata from SnapshotMetadataService")
		}

		// Convert to our internal format
		metadata := &CBTMetadata{
			BaseSnapshot:  previousSnapshotHandle,
			TotalSize:     resp.VolumeCapacityBytes,
			ChangedRanges: make([]ByteRange, len(resp.BlockMetadataDeltas)),
		}

		for i, delta := range resp.BlockMetadataDeltas {
			metadata.ChangedRanges[i] = ByteRange{
				Offset: delta.ByteOffset,
				Length: delta.SizeBytes,
			}
		}

		return metadata, nil
	*/
}
