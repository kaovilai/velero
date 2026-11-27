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
	"context"
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	velerotest "github.com/velero-io/velero/pkg/test"
)

const smsTestDriver = "rbd.csi.ceph.com"

// snapshotMetadataService builds the SnapshotMetadataService of the test
// driver; a nil require leaves the field unset, as a CRD that predates it
// stores the object.
func snapshotMetadataService(require *bool) *unstructured.Unstructured {
	sms := &unstructured.Unstructured{}
	sms.SetGroupVersionKind(snapshotMetadataServiceGVK)
	sms.SetName(smsTestDriver)
	spec := map[string]any{"address": "csi-snapshot-metadata.rook-ceph:6443", "audience": "velero", "caCert": "Y2E="}
	if require != nil {
		spec["requireBaseSnapshotForDelta"] = *require
	}
	sms.Object["spec"] = spec
	return sms
}

// TestDecideBaseSnapshotRetention proves the precedence: an explicit volume
// policy parameter wins, true or false; otherwise the driver's flag decides;
// without either the snapshot is not retained.
func TestDecideBaseSnapshotRetention(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     *bool
		driver     *bool
		wantRetain bool
		wantSource string
	}{
		{name: "policy true, driver unset", policy: ptr.To(true), wantRetain: true, wantSource: "the volume policy"},
		{name: "policy true, driver false", policy: ptr.To(true), driver: ptr.To(false), wantRetain: true, wantSource: "the volume policy"},
		{name: "policy false, driver true", policy: ptr.To(false), driver: ptr.To(true), wantRetain: false, wantSource: "the volume policy"},
		{name: "policy false, driver unset", policy: ptr.To(false), wantRetain: false, wantSource: "the volume policy"},
		{name: "policy unset, driver true", driver: ptr.To(true), wantRetain: true, wantSource: "the CSI driver's SnapshotMetadataService"},
		{name: "policy unset, driver false", driver: ptr.To(false), wantRetain: false, wantSource: "the CSI driver's SnapshotMetadataService"},
		{name: "policy unset, driver unset", wantRetain: false, wantSource: "the default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retain, source := decideBaseSnapshotRetention(tc.policy, tc.driver)
			assert.Equal(t, tc.wantRetain, retain)
			assert.Equal(t, tc.wantSource, source)
		})
	}
}

// TestDriverRequiresBaseSnapshotForDelta proves the read of the driver's
// guidance: the field when set, and no guidance -- nil -- when it is unset or
// pruned by a CRD that predates it, when the driver has no
// SnapshotMetadataService, when the cluster has no such CRD, or when the read
// fails, which is logged.
func TestDriverRequiresBaseSnapshotForDelta(t *testing.T) {
	for _, tc := range []struct {
		name     string
		objs     []runtime.Object
		getErr   error
		want     *bool
		wantWarn bool
	}{
		{name: "field true", objs: []runtime.Object{snapshotMetadataService(ptr.To(true))}, want: ptr.To(true)},
		{name: "field false", objs: []runtime.Object{snapshotMetadataService(ptr.To(false))}, want: ptr.To(false)},
		{name: "field unset, or pruned by a CRD that predates it", objs: []runtime.Object{snapshotMetadataService(nil)}},
		{name: "no SnapshotMetadataService for the driver"},
		{name: "no SnapshotMetadataService CRD in the cluster", getErr: &meta.NoKindMatchError{GroupKind: snapshotMetadataServiceGVK.GroupKind()}},
		{name: "the read fails", getErr: errors.New("forbidden"), wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c crclient.Client = velerotest.NewFakeControllerRuntimeClientBuilder(t).WithRuntimeObjects(tc.objs...).Build()
			if tc.getErr != nil {
				c = interceptor.NewClient(c.(crclient.WithWatch), interceptor.Funcs{
					Get: func(context.Context, crclient.WithWatch, crclient.ObjectKey, crclient.Object, ...crclient.GetOption) error {
						return tc.getErr
					},
				})
			}
			logger, hook := logrustest.NewNullLogger()

			assert.Equal(t, tc.want, driverRequiresBaseSnapshotForDelta(t.Context(), c, smsTestDriver, logger))
			warned := false
			for _, entry := range hook.AllEntries() {
				warned = warned || entry.Level == logrus.WarnLevel
			}
			assert.Equal(t, tc.wantWarn, warned)
		})
	}
}
