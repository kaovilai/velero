package datamover

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	storagev1api "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// deleteRetainedSnapshot issues the deletion of the retained snapshot a
// DataUpload of the backup tarball records, and does not wait for the CSI
// driver. The tarball is the only source: no DataUpload in the cluster is
// read. A snapshot a later backup already released is gone, and deleting it
// again succeeds by the CSI spec. A record naming a driver the cluster does
// not run is skipped with a warning; its handle stays in the tarball for
// manual cleanup. An error means the deletion could not be issued, which
// fails the backup deletion request before the tarball goes, so a retry still
// has the handle.
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
		log.Warnf("The cluster does not run CSI driver %s; the retained snapshot %s is left for manual cleanup", record.Driver, record.CSISnapshotHandle)
		return nil
	}

	return csi.DeleteRetainedSnapshot(ctx, d.client, record, bak.UID, log)
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

func NewDataUploadDeleteAction(logger logrus.FieldLogger, client client.Client) *DataUploadDeleteAction {
	return &DataUploadDeleteAction{
		logger: logger,
		client: client,
	}
}
