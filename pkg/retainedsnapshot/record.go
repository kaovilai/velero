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
package retainedsnapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
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
	"github.com/velero-io/velero/pkg/util/kube"
)

const (
	// RecordLabel identifies a record ConfigMap.
	RecordLabel = "velero.io/retained-snapshot"

	// ChainLabel is the hash of the chain key, on records, block DataUploads and
	// in-place incremental DataDownloads.
	ChainLabel = "velero.io/base-snapshot-chain"

	// StateLabel is the state of a record.
	StateLabel = "velero.io/retained-snapshot-state"

	// TempVSCRecordLabel names the record on the temp VolumeSnapshotContent that
	// deletes its snapshot.
	TempVSCRecordLabel = "velero.io/retained-snapshot-record"

	// InPlaceFallbackReasonAnnotation and InPlaceFallbackMessageAnnotation tell
	// the DataDownload controller why the in-place restore is full.
	InPlaceFallbackReasonAnnotation  = "velero.io/in-place-fallback-reason"
	InPlaceFallbackMessageAnnotation = "velero.io/in-place-fallback-message"

	// DecisionAnnotation records on the DataUpload the retention decision and its
	// source, for describe.
	DecisionAnnotation = "velero.io/base-snapshot-retention"

	// StateRetained means the snapshot is in the storage.
	StateRetained = "retained"
	// StateDeleting means a temp VolumeSnapshotContent is deleting the snapshot.
	StateDeleting = "deleting"
	// StateDeleted is a tombstone: the snapshot is gone, the record stays until
	// its backup is deleted.
	StateDeleted = "deleted"

	// ReasonRotated means a later DataUpload of the chain completed.
	ReasonRotated = "rotated"
	// ReasonDataUploadFailed means the DataUpload didn't complete after the commit point.
	ReasonDataUploadFailed = "dataUploadFailed"
	// ReasonVolumeGone means the source PVC is gone or was recreated.
	ReasonVolumeGone = "volumeGone"
	// ReasonBackupGone means the Backup is gone.
	ReasonBackupGone = "backupGone"
	// ReasonDeletedWithBackup means a backup deletion started the deletion; a
	// tombstone with it only exists when the backup deletion failed before
	// removing the record.
	ReasonDeletedWithBackup = "deletedWithBackup"

	keySnapshotHandle          = "snapshotHandle"
	keyDriver                  = "driver"
	keyDeletionSecretName      = "deletionSecretName"
	keyDeletionSecretNamespace = "deletionSecretNamespace"
	keyManagedBy               = "managedBy"
	keyClusterID               = "clusterID"
	keyBSL                     = "backupStorageLocation"
	keySourceNamespace         = "sourceNamespace"
	keySourcePVC               = "sourcePVC"
	keySourcePVCUID            = "sourcePVCUID"
	keyDataUpload              = "dataUpload"
	keyBackup                  = "backup"
	keyRepoSnapshotID          = "repoSnapshotID"
	keyRepoSnapshotStartTime   = "repoSnapshotStartTime"
	keyTempVSC                 = "tempVolumeSnapshotContent"
	keyDeletedAt               = "deletedAt"
	keyReason                  = "reason"
	keyRotatedByDataUpload     = "rotatedByDataUpload"
	keyRotatedByBackup         = "rotatedByBackup"
	keyDeletingBy              = "deletingBy"
	keyDeletingSince           = "deletingSince"
)

// ErrConflict is returned when another writer changed the record first.
var ErrConflict = errors.New("retained snapshot record changed by another writer")

// ErrGone is returned when the record was removed while being updated.
var ErrGone = errors.New("retained snapshot record removed by another writer")

// Record is the in-cluster record of a retained snapshot.
type Record struct {
	velerov2alpha1api.RetainedSnapshot
	BackupStorageLocation string
	SourceNamespace       string
	SourcePVC             string
	SourcePVCUID          string
	DataUpload            string
	Backup                string
	RepoSnapshotID        string
	RepoSnapshotStartTime time.Time
	State                 string
	TempVSC               string
	DeletedAt             time.Time
	Reason                string
	RotatedByDataUpload   string
	RotatedByBackup       string
	// DeletingBy and DeletingSince tell who moved the record to deleting and
	// when, so that another writer doesn't race the owner of the deletion.
	DeletingBy    string
	DeletingSince time.Time
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

// RecordName returns the name of the record ConfigMap of a DataUpload.
func RecordName(duName string) string {
	return fmt.Sprintf("%s-retained-snapshot", duName)
}

// TempVSCName returns the name of the temp VolumeSnapshotContent that deletes
// the snapshot, the same as csi.DeleteSnapshotByTempVSC derives.
func TempVSCName(driver, handle string) string {
	sum := sha256.Sum256([]byte(driver + "/" + handle))
	return "vsc-" + hex.EncodeToString(sum[:16])
}

// NewRecordForDataUpload builds a record for the snapshot a DataUpload retains.
func NewRecordForDataUpload(du *velerov2alpha1api.DataUpload, retained *velerov2alpha1api.RetainedSnapshot, repoSnapshotID string, repoSnapshotStartTime time.Time) *Record {
	return &Record{
		RetainedSnapshot:      *retained,
		BackupStorageLocation: du.Spec.BackupStorageLocation,
		SourceNamespace:       du.Spec.SourceNamespace,
		SourcePVC:             du.Spec.SourcePVC,
		SourcePVCUID:          du.Labels[velerov1api.PVCUIDLabel],
		DataUpload:            du.Name,
		Backup:                du.Labels[velerov1api.BackupNameLabel],
		RepoSnapshotID:        repoSnapshotID,
		RepoSnapshotStartTime: repoSnapshotStartTime,
		State:                 StateRetained,
	}
}

// ToConfigMap converts the record to its ConfigMap in namespace.
func (r *Record) ToConfigMap(namespace string) *corev1api.ConfigMap {
	cm := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      RecordName(r.DataUpload),
		},
	}
	r.WriteTo(cm)
	return cm
}

// WriteTo writes the record into cm, keeping its metadata.
func (r *Record) WriteTo(cm *corev1api.ConfigMap) {
	data := map[string]string{
		keySnapshotHandle:  r.SnapshotHandle,
		keyDriver:          r.Driver,
		keyClusterID:       r.ClusterID,
		keyBSL:             r.BackupStorageLocation,
		keySourceNamespace: r.SourceNamespace,
		keySourcePVC:       r.SourcePVC,
		keySourcePVCUID:    r.SourcePVCUID,
		keyDataUpload:      r.DataUpload,
		keyBackup:          r.Backup,
		keyRepoSnapshotID:  r.RepoSnapshotID,
	}
	set := func(k, v string) {
		if v != "" {
			data[k] = v
		}
	}
	set(keyDeletionSecretName, r.DeletionSecretName)
	set(keyDeletionSecretNamespace, r.DeletionSecretNamespace)
	set(keyManagedBy, r.ManagedBy)
	set(keyTempVSC, r.TempVSC)
	set(keyReason, r.Reason)
	set(keyRotatedByDataUpload, r.RotatedByDataUpload)
	set(keyRotatedByBackup, r.RotatedByBackup)
	set(keyDeletingBy, r.DeletingBy)
	if !r.DeletingSince.IsZero() {
		data[keyDeletingSince] = r.DeletingSince.UTC().Format(time.RFC3339)
	}
	if !r.RepoSnapshotStartTime.IsZero() {
		data[keyRepoSnapshotStartTime] = r.RepoSnapshotStartTime.UTC().Format(time.RFC3339Nano)
	}
	if !r.DeletedAt.IsZero() {
		data[keyDeletedAt] = r.DeletedAt.UTC().Format(time.RFC3339)
	}
	state := r.State
	if state == "" {
		state = StateRetained
	}
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels[velerov1api.BackupNameLabel] = label.GetValidName(r.Backup)
	cm.Labels[RecordLabel] = "true"
	cm.Labels[ChainLabel] = r.Chain()
	cm.Labels[StateLabel] = state
	cm.Data = data
}

// FromConfigMap parses a record ConfigMap.
func FromConfigMap(cm *corev1api.ConfigMap) (*Record, error) {
	d := cm.Data
	r := &Record{
		RetainedSnapshot: velerov2alpha1api.RetainedSnapshot{
			SnapshotHandle:          d[keySnapshotHandle],
			Driver:                  d[keyDriver],
			DeletionSecretName:      d[keyDeletionSecretName],
			DeletionSecretNamespace: d[keyDeletionSecretNamespace],
			ManagedBy:               d[keyManagedBy],
			ClusterID:               d[keyClusterID],
		},
		BackupStorageLocation: d[keyBSL],
		SourceNamespace:       d[keySourceNamespace],
		SourcePVC:             d[keySourcePVC],
		SourcePVCUID:          d[keySourcePVCUID],
		DataUpload:            d[keyDataUpload],
		Backup:                d[keyBackup],
		RepoSnapshotID:        d[keyRepoSnapshotID],
		State:                 cm.Labels[StateLabel],
		TempVSC:               d[keyTempVSC],
		Reason:                d[keyReason],
		RotatedByDataUpload:   d[keyRotatedByDataUpload],
		RotatedByBackup:       d[keyRotatedByBackup],
		DeletingBy:            d[keyDeletingBy],
	}
	if v := d[keyDeletingSince]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			r.DeletingSince = t
		}
	}
	if r.State == "" {
		r.State = StateRetained
	}
	if r.Backup == "" {
		r.Backup = cm.Labels[velerov1api.BackupNameLabel]
	}
	if v := d[keyRepoSnapshotStartTime]; v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid %s %q in configmap %s", keyRepoSnapshotStartTime, v, cm.Name)
		}
		r.RepoSnapshotStartTime = t
	}
	if v := d[keyDeletedAt]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			r.DeletedAt = t
		}
	}
	if r.SnapshotHandle == "" || r.Driver == "" {
		return nil, errors.Errorf("invalid retained snapshot record %s, handle %q, driver %q", cm.Name, r.SnapshotHandle, r.Driver)
	}
	return r, nil
}

// Describe returns a short description of where the base snapshot of the
// record is, e.g. for describe and the in-place restore decision.
func (r *Record) Describe() string {
	switch r.State {
	case StateRetained:
		return fmt.Sprintf("retained (%s)", r.SnapshotHandle)
	case StateDeleting:
		return fmt.Sprintf("being deleted (%s, temp VolumeSnapshotContent %s)", r.SnapshotHandle, r.TempVSC)
	}
	at := ""
	if !r.DeletedAt.IsZero() {
		at = " at " + r.DeletedAt.UTC().Format(time.RFC3339)
	}
	switch r.Reason {
	case ReasonRotated:
		return fmt.Sprintf("rotated out%s by backup %s (DataUpload %s)", at, r.RotatedByBackup, r.RotatedByDataUpload)
	case ReasonDataUploadFailed:
		return fmt.Sprintf("deleted%s because its DataUpload didn't complete", at)
	case ReasonVolumeGone:
		return fmt.Sprintf("deleted%s because the source volume is gone", at)
	case ReasonBackupGone:
		return fmt.Sprintf("deleted%s because its backup is gone", at)
	case ReasonDeletedWithBackup:
		return fmt.Sprintf("deleted with backup %s%s", r.Backup, at)
	}
	return fmt.Sprintf("deleted%s", at)
}

// Get reads the record ConfigMap from reader, which must be the API server,
// not an informer cache, before acting on it.
func Get(ctx context.Context, reader crclient.Reader, namespace, name string) (*corev1api.ConfigMap, *Record, error) {
	cm := &corev1api.ConfigMap{}
	if err := reader.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		return nil, nil, err
	}
	r, err := FromConfigMap(cm)
	if err != nil {
		return cm, nil, err
	}
	return cm, r, nil
}

// update writes r into cm with an update conditioned on cm's resourceVersion.
func update(ctx context.Context, c crclient.Client, cm *corev1api.ConfigMap, r *Record) error {
	updated := cm.DeepCopy()
	r.WriteTo(updated)
	if err := c.Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			return ErrConflict
		}
		if apierrors.IsNotFound(err) {
			// Removed by the owner of the deletion, e.g. backup deletion.
			return ErrGone
		}
		return errors.Wrapf(err, "error updating retained snapshot record %s", cm.Name)
	}
	*cm = *updated
	return nil
}

// DeleteOptions configures DeleteSnapshot.
type DeleteOptions struct {
	// Wait is how long to wait for the temp VolumeSnapshotContent to disappear in
	// this attempt. A pending deletion returns csi.ErrTempVSCDeletionPending and
	// the record stays deleting; the next attempt waits on the same object.
	Wait time.Duration
	// Remove removes the record once the snapshot is deleted (backup deletion),
	// instead of turning it into a tombstone.
	Remove bool
	// Reason, RotatedByDataUpload and RotatedByBackup are written to the tombstone.
	Reason              string
	RotatedByDataUpload string
	RotatedByBackup     string
	// Now returns the current time.
	Now func() time.Time
	// Owner names the writer, recorded when it moves the record to deleting.
	Owner string
}

// DeleteSnapshot deletes the snapshot of a record. cm must be read from the API
// server. The record moves to deleting with an update conditioned on its
// resourceVersion, so that only the writer that wins deletes; a loser gets
// ErrConflict. Only after the temp VolumeSnapshotContent is gone is the record
// marked deleted or removed. Deleting a snapshot already gone succeeds.
func DeleteSnapshot(ctx context.Context, c crclient.Client, apiClient crclient.Client, cm *corev1api.ConfigMap, opts DeleteOptions, log logrus.FieldLogger) error {
	r, err := FromConfigMap(cm)
	if err != nil {
		return err
	}
	if r.State == StateDeleted {
		if opts.Remove {
			return Remove(ctx, c, cm)
		}
		return nil
	}

	tempVSC := TempVSCName(r.Driver, r.SnapshotHandle)
	if r.State == StateRetained {
		r.State = StateDeleting
		r.TempVSC = tempVSC
		// Keep why it is deleted, so that whoever finishes an interrupted
		// deletion writes the same tombstone.
		r.Reason = opts.Reason
		r.RotatedByDataUpload = opts.RotatedByDataUpload
		r.RotatedByBackup = opts.RotatedByBackup
		r.DeletingBy = opts.Owner
		r.DeletingSince = opts.Now()
		if err := update(ctx, c, cm, r); err != nil {
			return err
		}
		log.Infof("Retained snapshot record %s moved to deleting, temp VolumeSnapshotContent %s", cm.Name, tempVSC)
	}

	if err := deleteByTempVSC(ctx, apiClient, r, cm.Name, opts.Wait, log); err != nil {
		return err
	}

	if opts.Remove {
		return Remove(ctx, c, cm)
	}
	r.State = StateDeleted
	r.DeletedAt = opts.Now()
	r.Reason = opts.Reason
	r.RotatedByDataUpload = opts.RotatedByDataUpload
	r.RotatedByBackup = opts.RotatedByBackup
	if err := update(ctx, c, cm, r); err != nil {
		return err
	}
	log.Infof("Retained snapshot record %s is a tombstone now: %s", cm.Name, r.Describe())
	return nil
}

// Remove removes the record ConfigMap with a delete conditioned on its
// resourceVersion. A record already gone is not an error.
func Remove(ctx context.Context, c crclient.Client, cm *corev1api.ConfigMap) error {
	if err := c.Delete(ctx, cm, crclient.Preconditions{ResourceVersion: &cm.ResourceVersion}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if apierrors.IsConflict(err) {
			return ErrConflict
		}
		return errors.Wrapf(err, "error removing retained snapshot record %s", cm.Name)
	}
	return nil
}

// deleteByTempVSC has the CSI driver delete the snapshot of the record through
// a temp VolumeSnapshotContent labeled with the record name.
func deleteByTempVSC(ctx context.Context, c crclient.Client, r *Record, recordName string, wait time.Duration, log logrus.FieldLogger) error {
	handle := r.SnapshotHandle
	vsc := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				velerov1api.BackupNameLabel: label.GetValidName(r.Backup),
				TempVSCRecordLabel:          label.GetValidName(recordName),
			},
		},
		Spec: snapshotv1api.VolumeSnapshotContentSpec{
			Driver: r.Driver,
			Source: snapshotv1api.VolumeSnapshotContentSource{SnapshotHandle: &handle},
		},
	}
	if r.ManagedBy != "" {
		vsc.Labels[kube.VolumeSnapshotContentManagedByLabel] = r.ManagedBy
	}
	if r.DeletionSecretName != "" {
		vsc.Annotations = map[string]string{
			csi.VSCDeletionSecretNameAnnotation:      r.DeletionSecretName,
			csi.VSCDeletionSecretNamespaceAnnotation: r.DeletionSecretNamespace,
		}
	}
	return csi.DeleteSnapshotByTempVSCWait(ctx, c, vsc, wait, log)
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

// IsForeign tells whether a snapshot recorded with clusterID belongs to another
// cluster. An empty recorded identity is treated as local.
func IsForeign(recorded, local string) bool {
	return recorded != "" && local != "" && recorded != local
}
