package datamover

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	storagev1api "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	velerov1 "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1 "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/label"
	"github.com/velero-io/velero/pkg/plugin/velero"
	repotypes "github.com/velero-io/velero/pkg/repository/types"
	"github.com/velero-io/velero/pkg/util/csi"
	"github.com/velero-io/velero/pkg/util/datamover"
)

type DataUploadDeleteAction struct {
	logger logrus.FieldLogger
	client client.Client
	// resourceTimeout bounds the wait for a retained snapshot's deletion: the
	// --resource-timeout of the server that runs the action.
	resourceTimeout time.Duration

	// timedOut records the backups whose deletion already waited out the
	// bound once: the action runs inside the backup-deletion controller's
	// reconcile, which handles one request at a time, so after the first
	// record that times out, the remaining records of the same request are
	// deleted without waiting.
	mu       sync.Mutex
	timedOut map[types.UID]bool
}

func (d *DataUploadDeleteAction) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{"datauploads.velero.io"},
	}, nil
}

func (d *DataUploadDeleteAction) Execute(input *velero.DeleteItemActionExecuteInput) error {
	d.logger.Infof("Executing DataUploadDeleteAction")
	du := &velerov2alpha1.DataUpload{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(input.Item.UnstructuredContent(), &du); err != nil {
		return errors.WithStack(errors.Wrapf(err, "failed to convert input.Item from unstructured"))
	}
	// Only create ConfigMaps when the DataUpload's owning backup (its
	// velero.io/backup-name label) matches the backup currently being deleted.
	// Two other cases reach this code path and must be skipped, because the
	// resulting CM would be unmatchable and only adds etcd churn:
	//
	//  1. The label is missing. We have no verifiable owner, so a CM created
	//     with the executing backup's label is a guess that the deletion
	//     cannot rely on.
	//  2. The label names a different backup. Velero does not support
	//     self-protection, so this almost always means the velero namespace
	//     was captured in a backup tarball and the DataUpload CR belongs to
	//     an unrelated backup. Creating a CM labeled with the executing
	//     backup mislabels the snapshot and causes the real owning backup's
	//     deletion to miss it, leaking the snapshot in the storage.
	//
	// Both cases warn so misconfigured installs surface in logs.
	owner := du.Labels[velerov1.BackupNameLabel]
	switch {
	case owner == "":
		d.logger.Warnf(
			"DataUpload %q has no %q label, so its owning backup cannot be verified; "+
				"skipping ConfigMap creation because a CM without a verifiable owner "+
				"cannot be matched back to its snapshot at backup deletion time.",
			du.Name, velerov1.BackupNameLabel,
		)
		return nil
	case owner != label.GetValidName(input.Backup.Name):
		d.logger.Warnf(
			"DataUpload %q belongs to backup %q but is being deleted under backup %q; "+
				"this almost always means the velero namespace was included in a backup tarball. "+
				"Velero does not support self-protection — exclude the velero namespace from your schedules. "+
				"Skipping ConfigMap creation to avoid mislabeling.",
			du.Name, owner, input.Backup.Name,
		)
		return nil
	}
	// A retained snapshot the rolling base did not release yet belongs to this
	// backup and is deleted here, synchronously, before the repository data:
	// a failure fails the delete actions, which fails the deletion request
	// before the repository data, the DataUploads and the tarball are touched,
	// so the backup stays deletable on retry.
	if err := d.deleteRetainedSnapshot(input.Backup, du); err != nil {
		return err
	}

	// The snapshot-info ConfigMap of the repository data. It is only created
	// for a DataUpload the config can be generated for; the retained snapshot
	// deletion above does not depend on it.
	if cm := genConfigmap(input.Backup, *du); cm != nil {
		err := d.client.Create(context.Background(), cm)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return errors.WithStack(errors.Wrapf(err, "failed to create the configmap for DataUpload %s/%s", du.Namespace, du.Name))
		}
	}
	return nil
}

// deleteRetainedSnapshot deletes the retained snapshot a DataUpload of the
// backup tarball records. The tarball is the only source: no DataUpload in the
// cluster is read. A snapshot a later backup already released is gone, and
// deleting it again succeeds -- the helper seeds the content status, the CSI
// snapshotter issues DeleteSnapshot, and the CSI spec requires success for a
// snapshot that does not exist. A record naming a driver the cluster does not run is skipped with
// a Warning event on the Backup, and its handle stays in the tarball for
// manual cleanup. The helper's error is returned unchanged.
func (d *DataUploadDeleteAction) deleteRetainedSnapshot(bak *velerov1.Backup, du *velerov2alpha1.DataUpload) error {
	ctx := context.Background()
	record := du.Status.RetainedCSIStorageSnapshot
	if record == nil || record.CSISnapshotHandle == "" {
		return nil
	}
	log := d.logger.WithField("dataupload", du.Namespace+"/"+du.Name)

	driver := &storagev1api.CSIDriver{}
	if err := d.client.Get(ctx, client.ObjectKey{Name: record.Driver}, driver); err != nil {
		if !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "error getting CSIDriver %s", record.Driver)
		}
		message := fmt.Sprintf("the cluster does not run CSI driver %s; the retained snapshot %s of DataUpload %s is left for manual cleanup", record.Driver, record.CSISnapshotHandle, du.Name)
		log.Warn(message)
		d.warnOnBackup(ctx, bak, "RetainedSnapshotDriverUnknown", message, log)
		return nil
	}

	d.mu.Lock()
	noWait := d.timedOut[bak.UID]
	d.mu.Unlock()
	log.Infof("Deleting the retained snapshot %s of driver %s", record.CSISnapshotHandle, record.Driver)
	// The plugin process's client (client.Factory.KubebuilderClient) is a
	// direct client, so the helper's existence reads go to the API server.
	err := csi.DeleteRetainedSnapshot(ctx, d.client, d.client, record, bak.UID, csi.RetainedSnapshotDeletionOptions{Timeout: d.resourceTimeout, NoWait: noWait}, log)
	var pending *csi.RetainedSnapshotDeletionPendingError
	if errors.As(err, &pending) && !noWait {
		d.mu.Lock()
		if d.timedOut == nil {
			d.timedOut = map[types.UID]bool{}
		}
		d.timedOut[bak.UID] = true
		d.mu.Unlock()
	}
	return err
}

// warnOnBackup records a Warning event on the Backup; a failure to record it
// is logged only.
func (d *DataUploadDeleteAction) warnOnBackup(ctx context.Context, bak *velerov1.Backup, reason, message string, log logrus.FieldLogger) {
	now := metav1.Now()
	event := &corev1api.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: bak.Namespace, GenerateName: bak.Name + "."},
		InvolvedObject: corev1api.ObjectReference{
			APIVersion: velerov1.SchemeGroupVersion.String(),
			Kind:       "Backup",
			Namespace:  bak.Namespace,
			Name:       bak.Name,
			UID:        bak.UID,
		},
		Reason:         reason,
		Message:        message,
		Type:           corev1api.EventTypeWarning,
		Source:         corev1api.EventSource{Component: "velero"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	if err := d.client.Create(ctx, event); err != nil {
		log.WithError(err).Warn("Failed to record the event on the Backup")
	}
}

// generate the configmap which is to be created and used as a way to communicate the snapshot info to the backup deletion controller
func genConfigmap(bak *velerov1.Backup, du velerov2alpha1.DataUpload) *corev1api.ConfigMap {
	if !datamover.IsBuiltInDataMover(du.Spec.DataMover) || du.Status.SnapshotID == "" {
		return nil
	}
	snapshot := repotypes.SnapshotIdentifier{
		VolumeNamespace:       du.Spec.SourceNamespace,
		BackupStorageLocation: bak.Spec.StorageLocation,
		SnapshotID:            du.Status.SnapshotID,
		RepositoryType:        velerov1.BackupRepositoryTypeKopia,
		UploaderType:          GetUploaderType(du.Spec.DataMover),
		Source:                GetRealSource(du.Spec.SourceNamespace, du.Spec.SourcePVC),
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return nil
	}
	data := make(map[string]string)
	if err := json.Unmarshal(b, &data); err != nil {
		return nil
	}
	return &corev1api.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1api.SchemeGroupVersion.String(),
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: bak.Namespace,
			Name:      fmt.Sprintf("%s-info", du.Name),
			Labels: map[string]string{
				// The backup-deletion controller lists these ConfigMaps by
				// the sanitized backup name, and a label value is capped at
				// 63 characters: the raw name of a long backup name fails the
				// create and left the backup undeletable.
				velerov1.BackupNameLabel:             label.GetValidName(bak.Name),
				velerov1.DataUploadSnapshotInfoLabel: "true",
			},
		},
		Data: data,
	}
}

// NewDataUploadDeleteAction returns the DataUpload delete item action; a
// retained snapshot's deletion is bounded by resourceTimeout, the server's
// --resource-timeout, or the helper's default when zero.
func NewDataUploadDeleteAction(logger logrus.FieldLogger, client client.Client, resourceTimeout time.Duration) *DataUploadDeleteAction {
	return &DataUploadDeleteAction{
		logger:          logger,
		client:          client,
		resourceTimeout: resourceTimeout,
	}
}
