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
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	velerotest "github.com/velero-io/velero/pkg/test"
	"github.com/velero-io/velero/pkg/util/csi"
)

// releaseClient plays the API server and the CSI snapshotter for a release:
// creates of a static content fail createErrs times first, and a content being
// deleted with a seeded handle is removed on the next read unless stuck.
func releaseClient(t *testing.T, createErrs int, stuck bool, creates *int) crclient.Client {
	t.Helper()
	base := velerotest.NewFakeControllerRuntimeClientBuilder(t).WithStatusSubresource(&snapshotv1api.VolumeSnapshotContent{}).Build()
	return interceptor.NewClient(base.(crclient.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			*creates++
			if *creates <= createErrs {
				return errors.New("transient")
			}
			return c.Create(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			vsc := &snapshotv1api.VolumeSnapshotContent{}
			if err := c.Get(ctx, key, vsc); err == nil && !stuck && vsc.DeletionTimestamp != nil && vsc.Status != nil && vsc.Status.SnapshotHandle != nil {
				vsc.Finalizers = nil
				_ = c.Update(ctx, vsc)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

// TestReleaseRetainedSnapshotRetries proves the bounded retries of a release:
// a transient failure is retried within the bound, a release that keeps
// failing returns after the last attempt, and a content left being deleted is
// checked once, not retried.
func TestReleaseRetainedSnapshotRetries(t *testing.T) {
	origInterval, origTimeout := releaseRetryInterval, csi.RetainedSnapshotDeletionTimeout
	releaseRetryInterval = time.Millisecond
	t.Cleanup(func() { releaseRetryInterval, csi.RetainedSnapshotDeletionTimeout = origInterval, origTimeout })
	base := &velerov2alpha1api.RetainedCSIStorageSnapshot{CSISnapshotHandle: "base-snap", Driver: "hostpath.csi.k8s.io"}

	for _, tc := range []struct {
		name        string
		createErrs  int
		stuck       bool
		wantErr     bool
		wantCreates int
	}{
		{name: "released on the first attempt", wantCreates: 1},
		{name: "a transient failure is retried", createErrs: 2, wantCreates: 3},
		{name: "every attempt fails", createErrs: 5, wantErr: true, wantCreates: releaseAttempts},
		{name: "a content left being deleted is not retried", stuck: true, wantErr: true, wantCreates: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creates := 0
			c := releaseClient(t, tc.createErrs, tc.stuck, &creates)
			err := ReleaseRetainedSnapshot(t.Context(), c, base, "backup-uid", 500*time.Millisecond, velerotest.NewLogger())
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantCreates, creates)
		})
	}
}
