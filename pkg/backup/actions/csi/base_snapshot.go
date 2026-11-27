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

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// snapshotMetadataServiceGVK is the SnapshotMetadataService of the Kubernetes
// CSI changed block tracking API, one cluster-scoped object per CSI driver,
// named after the driver. It is read as unstructured: the typed client of
// external-snapshot-metadata v1.0.0 that Velero depends on predates the
// requireBaseSnapshotForDelta field (added in v1.2.0).
var snapshotMetadataServiceGVK = schema.GroupVersionKind{Group: "cbt.storage.k8s.io", Version: "v1beta1", Kind: "SnapshotMetadataService"}

// driverRequiresBaseSnapshotForDelta returns spec.requireBaseSnapshotForDelta
// of the SnapshotMetadataService of the CSI driver: whether the driver needs
// the base snapshot to compute a delta. It returns nil -- no guidance -- when
// the field is unset, when the installed CRD predates the field (the API
// server prunes it), when the driver has no SnapshotMetadataService or the
// cluster no CRD, and when the object cannot be read; a read error is logged.
func driverRequiresBaseSnapshotForDelta(ctx context.Context, c crclient.Client, driver string, log logrus.FieldLogger) *bool {
	if c == nil || driver == "" {
		return nil
	}
	sms := &unstructured.Unstructured{}
	sms.SetGroupVersionKind(snapshotMetadataServiceGVK)
	if err := c.Get(ctx, crclient.ObjectKey{Name: driver}, sms); err != nil {
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			log.WithError(err).Warnf("Failed to read the SnapshotMetadataService of CSI driver %s, it gives no guidance on base snapshot retention", driver)
		}
		return nil
	}
	required, found, err := unstructured.NestedBool(sms.Object, "spec", "requireBaseSnapshotForDelta")
	if err != nil || !found {
		return nil
	}
	return &required
}

// decideBaseSnapshotRetention applies the precedence of the retention
// decision: an explicit requireBaseSnapshotForDelta of the volume policy wins,
// true or false; otherwise the SnapshotMetadataService of the CSI driver
// decides, retaining only when it sets requireBaseSnapshotForDelta to true;
// without guidance the snapshot is not retained, today's behavior. It returns
// the decision and the source that made it, for the log.
func decideBaseSnapshotRetention(policy, driver *bool) (bool, string) {
	switch {
	case policy != nil:
		return *policy, "the volume policy"
	case driver != nil:
		return *driver, "the CSI driver's SnapshotMetadataService"
	default:
		return false, "the default"
	}
}
