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

package retainedsnapshot

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// SnapshotMetadataServiceGVK is the SnapshotMetadataService of
// kubernetes-csi/external-snapshot-metadata. It is read unstructured because
// spec.requireBaseSnapshotForDelta is not in a released client yet.
var SnapshotMetadataServiceGVK = schema.GroupVersionKind{Group: "cbt.storage.k8s.io", Version: "v1beta1", Kind: "SnapshotMetadataService"}

// RequireBaseSnapshotForDelta reads spec.requireBaseSnapshotForDelta of the
// SnapshotMetadataService named after the CSI driver. It returns nil and the
// reason when there is no value: no SnapshotMetadataService (also when its CRD
// is not installed), no field (a CRD without the field prunes it), or a read
// error. Otherwise it returns the value and the SnapshotMetadataService name.
func RequireBaseSnapshotForDelta(ctx context.Context, reader crclient.Reader, driver string) (*bool, string) {
	if driver == "" {
		return nil, "no CSI driver"
	}
	sms := &unstructured.Unstructured{}
	sms.SetGroupVersionKind(SnapshotMetadataServiceGVK)
	if err := reader.Get(ctx, crclient.ObjectKey{Name: driver}, sms); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, fmt.Sprintf("no SnapshotMetadataService for %s", driver)
		}
		return nil, fmt.Sprintf("reading SnapshotMetadataService failed: %v", err)
	}
	b, found, err := unstructured.NestedBool(sms.Object, "spec", "requireBaseSnapshotForDelta")
	if err != nil {
		return nil, fmt.Sprintf("reading SnapshotMetadataService failed: %v", err)
	}
	if !found {
		return nil, fmt.Sprintf("SnapshotMetadataService %s has no requireBaseSnapshotForDelta", sms.GetName())
	}
	return &b, sms.GetName()
}
