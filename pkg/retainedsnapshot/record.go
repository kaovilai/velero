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

// Package retainedsnapshot tracks the base snapshots the block data mover keeps
// in the storage for storages that need the base snapshot to compute changed
// blocks (Case 2 in design/block-data-mover/base-snapshot-retention.md).
//
// Prototype notes: the decision is passed to the node-agent through
// DataUpload.spec.dataMoverConfig and the cluster identity through a DataUpload
// annotation, so that no CRD change is needed.
package retainedsnapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/label"
	"github.com/velero-io/velero/pkg/util/csi"
)

const (
	// DataMoverConfigKey is the DataUpload spec.dataMoverConfig key that tells the
	// node-agent to retain the snapshot after the DataUpload completes.
	DataMoverConfigKey = "retainBaseSnapshot"

	// ClusterIDAnnotation records on the DataUpload the cluster whose storage
	// holds the retained snapshot.
	ClusterIDAnnotation = "velero.io/retained-snapshot-cluster-id"

	// ChainLabel is the hash of the chain key of a record.
	ChainLabel = "velero.io/retained-snapshot-chain"

	// StateLabel is the state of a record: StateRetained or StateDeleted.
	StateLabel = "velero.io/retained-snapshot-state"

	// StateRetained means the snapshot is still in the storage.
	StateRetained = "retained"
	// StateDeleted means the snapshot was rotated out. The record stays until
	// its backup is deleted, so that backup deletion doesn't recreate it from
	// the DataUpload status in the backup.
	StateDeleted = "deleted"

	keySnapshotHandle          = "snapshotHandle"
	keyDriver                  = "driver"
	keyDeletionSecretName      = "deletionSecretName"
	keyDeletionSecretNamespace = "deletionSecretNamespace"
	keyManagedBy               = "managedBy"
	keyClusterID               = "clusterID"
	keyBSL                     = "backupStorageLocation"
	keySourceNamespace         = "sourceNamespace"
	keySourcePVC               = "sourcePVC"
	keyDataUpload              = "dataUpload"
	keyBackup                  = "backup"
	keyRepoSnapshotID          = "repoSnapshotID"
	keyStartTime               = "startTime"
	keyDeletedAt               = "deletedAt"
)

// Record is the in-cluster record of a retained snapshot.
type Record struct {
	velerov2alpha1api.RetainedSnapshot
	ClusterID             string
	BackupStorageLocation string
	SourceNamespace       string
	SourcePVC             string
	DataUpload            string
	Backup                string
	RepoSnapshotID        string
	StartTime             time.Time
	State                 string
}

// Chain returns the chain key hash of the record.
func (r *Record) Chain() string {
	return ChainHash(r.BackupStorageLocation, r.SourceNamespace, r.SourcePVC)
}

// ChainHash returns the label value for the chain key (BSL, source namespace,
// source PVC), the scope in which the block data mover looks for the parent.
func ChainHash(bsl, namespace, pvc string) string {
	sum := sha256.Sum256([]byte(bsl + "/" + namespace + "/" + pvc))
	return hex.EncodeToString(sum[:16])
}

// ChainOfDataUpload returns the chain key hash of a DataUpload.
func ChainOfDataUpload(du *velerov2alpha1api.DataUpload) string {
	return ChainHash(du.Spec.BackupStorageLocation, du.Spec.SourceNamespace, du.Spec.SourcePVC)
}

// RequestedFor tells whether the DataUpload asks to retain its snapshot.
func RequestedFor(du *velerov2alpha1api.DataUpload) bool {
	return du.Spec.DataMoverConfig[DataMoverConfigKey] == "true"
}

// RecordName returns the name of the record ConfigMap of a DataUpload.
func RecordName(duName string) string {
	return fmt.Sprintf("%s-retained-snapshot", duName)
}

// NewRecordForDataUpload builds a record for the snapshot a DataUpload retains.
func NewRecordForDataUpload(du *velerov2alpha1api.DataUpload, retained *velerov2alpha1api.RetainedSnapshot, clusterID, repoSnapshotID string) *Record {
	r := &Record{
		RetainedSnapshot:      *retained,
		ClusterID:             clusterID,
		BackupStorageLocation: du.Spec.BackupStorageLocation,
		SourceNamespace:       du.Spec.SourceNamespace,
		SourcePVC:             du.Spec.SourcePVC,
		DataUpload:            du.Name,
		Backup:                du.Labels[velerov1api.BackupNameLabel],
		RepoSnapshotID:        repoSnapshotID,
		State:                 StateRetained,
	}
	if du.Status.StartTimestamp != nil {
		r.StartTime = du.Status.StartTimestamp.Time
	}
	return r
}

// ToConfigMap converts the record to its ConfigMap in namespace.
func (r *Record) ToConfigMap(namespace string) *corev1api.ConfigMap {
	data := map[string]string{
		keySnapshotHandle:  r.SnapshotHandle,
		keyDriver:          r.Driver,
		keyClusterID:       r.ClusterID,
		keyBSL:             r.BackupStorageLocation,
		keySourceNamespace: r.SourceNamespace,
		keySourcePVC:       r.SourcePVC,
		keyDataUpload:      r.DataUpload,
		keyBackup:          r.Backup,
		keyRepoSnapshotID:  r.RepoSnapshotID,
	}
	if r.DeletionSecretName != "" {
		data[keyDeletionSecretName] = r.DeletionSecretName
		data[keyDeletionSecretNamespace] = r.DeletionSecretNamespace
	}
	if r.ManagedBy != "" {
		data[keyManagedBy] = r.ManagedBy
	}
	if !r.StartTime.IsZero() {
		data[keyStartTime] = r.StartTime.UTC().Format(time.RFC3339Nano)
	}
	state := r.State
	if state == "" {
		state = StateRetained
	}
	return &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      RecordName(r.DataUpload),
			Labels: map[string]string{
				velerov1api.BackupNameLabel:                 label.GetValidName(r.Backup),
				velerov1api.DataUploadRetainedSnapshotLabel: "true",
				ChainLabel: r.Chain(),
				StateLabel: state,
			},
		},
		Data: data,
	}
}

// FromConfigMap parses a record ConfigMap. Records created by #10691 before the
// prototype have only the RetainedSnapshot fields; their chain fields are empty.
func FromConfigMap(cm *corev1api.ConfigMap) (*Record, error) {
	d := cm.Data
	r := &Record{
		RetainedSnapshot: velerov2alpha1api.RetainedSnapshot{
			SnapshotHandle:          d[keySnapshotHandle],
			Driver:                  d[keyDriver],
			DeletionSecretName:      d[keyDeletionSecretName],
			DeletionSecretNamespace: d[keyDeletionSecretNamespace],
			ManagedBy:               d[keyManagedBy],
		},
		ClusterID:             d[keyClusterID],
		BackupStorageLocation: d[keyBSL],
		SourceNamespace:       d[keySourceNamespace],
		SourcePVC:             d[keySourcePVC],
		DataUpload:            d[keyDataUpload],
		Backup:                d[keyBackup],
		RepoSnapshotID:        d[keyRepoSnapshotID],
		State:                 cm.Labels[StateLabel],
	}
	if r.State == "" {
		r.State = StateRetained
	}
	if v := d[keyStartTime]; v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid %s %q in configmap %s", keyStartTime, v, cm.Name)
		}
		r.StartTime = t
	}
	if r.SnapshotHandle == "" || r.Driver == "" {
		return nil, errors.Errorf("invalid retained snapshot record %s, handle %q, driver %q", cm.Name, r.SnapshotHandle, r.Driver)
	}
	return r, nil
}

// MarkDeleted turns the record ConfigMap into a tombstone.
func MarkDeleted(cm *corev1api.ConfigMap, now time.Time) {
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels[StateLabel] = StateDeleted
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[keyDeletedAt] = now.UTC().Format(time.RFC3339)
}

// GetClusterID returns the identity of the cluster: the UID of the kube-system
// namespace.
func GetClusterID(ctx context.Context, kubeClient kubernetes.Interface) (string, error) {
	ns, err := kubeClient.CoreV1().Namespaces().Get(ctx, metav1.NamespaceSystem, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", errors.Wrap(err, "error getting the kube-system namespace for the cluster identity")
	}
	return string(ns.UID), nil
}

// IsForeign tells whether a snapshot recorded with clusterID belongs to another
// cluster. An empty recorded identity is treated as local (records written
// before the identity was recorded).
func IsForeign(recorded, local string) bool {
	return recorded != "" && local != "" && recorded != local
}

// DeleteSnapshot has the CSI driver delete the snapshot of the record through a
// temp VolumeSnapshotContent. Deleting a snapshot that is already gone succeeds.
func DeleteSnapshot(ctx context.Context, crClient crclient.Client, r *Record, log logrus.FieldLogger) error {
	return csi.DeleteRetainedSnapshot(ctx, crClient, &r.RetainedSnapshot, label.GetValidName(r.Backup), log)
}

// GetClusterIDFromReader is GetClusterID through a controller-runtime reader.
func GetClusterIDFromReader(ctx context.Context, reader crclient.Reader) (string, error) {
	ns := &corev1api.Namespace{}
	if err := reader.Get(ctx, crclient.ObjectKey{Name: metav1.NamespaceSystem}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			// No identity available; IsForeign treats an empty identity as local.
			return "", nil
		}
		return "", errors.Wrap(err, "error getting the kube-system namespace for the cluster identity")
	}
	return string(ns.UID), nil
}
