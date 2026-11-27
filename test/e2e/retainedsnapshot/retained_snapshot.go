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

// Package retainedsnapshot covers base-snapshot retention of the block data
// mover: a volume policy with retainSnapshot: true keeps the storage snapshot
// of a velero-block DataUpload alive past the backup, records it on the
// DataUpload, and the backup deletion deletes it again.
//
// The cases run only on kind with csi-driver-host-path, because they assert at
// the storage level: the hostpath driver keeps every snapshot as
// /csi-data-dir/<handle>.snap in the hostpath container of csi-hostpathplugin-0,
// so a snapshot that disappeared from the API but not from the storage, the
// regression velero-io/velero#10694 is about, is visible there and nowhere else.
package retainedsnapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/label"
	velerocsi "github.com/velero-io/velero/pkg/util/csi"
	veleroexec "github.com/velero-io/velero/pkg/util/exec"
	velerotest "github.com/velero-io/velero/test"
	. "github.com/velero-io/velero/test/e2e/test"
	"github.com/velero-io/velero/test/util/common"
	csi "github.com/velero-io/velero/test/util/csi"
	. "github.com/velero-io/velero/test/util/k8s"
	. "github.com/velero-io/velero/test/util/velero"
)

const (
	workloadName = "workload"
	// devicePath is where the pod sees its raw block PVC. The cases use block
	// volumes because the hostpath driver snapshots a block volume as a byte
	// copy and restores it with dd, which is what the velero-block mover
	// writes back; a filesystem volume's snapshot is a tarball there.
	devicePath = "/dev/data"

	hostpathDriver       = "hostpath.csi.k8s.io"
	hostpathNamespace    = "default"
	hostpathPod          = "csi-hostpathplugin-0"
	hostpathContainer    = "hostpath"
	hostpathSnapshotDir  = "/csi-data-dir"
	hostpathSnapshotExt  = ".snap"
	retentionPolicyEntry = "volumePolicies[0]"

	pollInterval  = 2 * time.Second
	watchInterval = 100 * time.Millisecond
	// deletionTimeout bounds a backup deletion with retained snapshots: it
	// creates a static content per snapshot and waits for the CSI sidecar to
	// delete it.
	deletionTimeout = 8 * time.Minute
	cleanupTimeout  = 3 * time.Minute
)

// policyYAML opts every volume of the suite's StorageClass into retention. The
// policy names the data mover too: a matching snapshot action without a
// dataMover parameter selects the default built-in mover, velero-fs, over the
// backup's --data-mover, and retention applies to velero-block only.
var policyYAML = fmt.Sprintf(`version: v1
volumePolicies:
- conditions:
    storageClass:
    - %s
  action:
    type: snapshot
    parameters:
      dataMover: velero-block
      retainSnapshot: true
`, velerotest.StorageClassName)

var RetainedSnapshotLifecycleTest = func() {
	Context("velero-block backup retains its snapshot and the backup deletion deletes it from the storage",
		Label("CSI", "DataMover", "BlockDataMover", "RetainedSnapshot", "Lifecycle"),
		TestFunc(&lifecycleCase{}),
	)
}

var RetainedSnapshotChainTest = func() {
	Context("each completed incremental releases its base, a missed release waits for its backup's deletion, and every backup restores",
		Label("CSI", "DataMover", "BlockDataMover", "RetainedSnapshot", "Chain"),
		TestFunc(&chainCase{}),
	)
}

var RetainedSnapshotHolderTest = func() {
	Context("deleting the backup holding the base makes the next backup a full read with the reason recorded",
		Label("CSI", "DataMover", "BlockDataMover", "RetainedSnapshot", "Holder"),
		TestFunc(&holderCase{}),
	)
}

var RetainedSnapshotDegradedTest = func() {
	Context("a Retain switch that fails or a record the API server prunes completes the backup without retention",
		Label("CSI", "DataMover", "BlockDataMover", "RetainedSnapshot", "Degraded"),
		TestFunc(&degradedCase{}),
	)
}

var RetainedSnapshotReleaseTest = func() {
	Context("a canceled DataUpload leaves no record and no snapshot, a DataUpload without the policy never retains, and the parent is untouched",
		Label("CSI", "DataMover", "BlockDataMover", "RetainedSnapshot", "Release"),
		TestFunc(&releaseCase{}),
	)
}

// retainedSnapshotCase is the shared base: one namespace, one raw block PVC,
// one deployment writing random data to it, a retention policy ConfigMap in the
// Velero namespace, and the storage-level assertions against the hostpath
// driver.
type retainedSnapshotCase struct {
	TestCase
	policyCM string
}

func (r *retainedSnapshotCase) init(prefix string) error {
	if err := r.TestCase.Init(); err != nil {
		return err
	}
	r.CaseBaseName = fmt.Sprintf("%s-%s", prefix, r.UUIDgen)
	r.NamespacesTotal = 1
	r.NSIncluded = &[]string{r.CaseBaseName}
	r.VeleroCfg.UseNodeAgent = true
	r.policyCM = r.CaseBaseName + "-policy"
	return nil
}

func (r *retainedSnapshotCase) Start() error {
	if r.VeleroCfg.WorkerOS == common.WorkerOSWindows {
		Skip("These cases use a Linux block workload")
	}
	if !velerotest.FeatureEnabled(r.VeleroCfg.Features, velerotest.FeatureCSI) || !r.VeleroCfg.UseVolumeSnapshots {
		Skip("These cases require CSI and volume snapshots enabled")
	}
	if r.VeleroCfg.CloudProvider != velerotest.Kind {
		Skip("These cases assert on the snapshot files of csi-driver-host-path, which only the kind CSI job runs")
	}
	return r.TestCase.Start()
}

// createWorkload creates the namespace, a raw block PVC on the suite's
// StorageClass and a deployment that attaches it at devicePath.
func (r *retainedSnapshotCase) createWorkload(size resource.Quantity) error {
	if err := CreateNamespaceWithLabel(r.Ctx, r.Client, r.CaseBaseName, map[string]string{}); err != nil {
		return err
	}
	pvc := NewPVC(r.CaseBaseName, workloadName).
		WithStorageClass(velerotest.StorageClassName).
		WithResourceStorage(size)
	pvc.Spec.VolumeMode = func(m corev1api.PersistentVolumeMode) *corev1api.PersistentVolumeMode { return &m }(corev1api.PersistentVolumeBlock)
	if err := CreatePvc(r.Client, pvc); err != nil {
		return err
	}
	deployment := NewDeployment(workloadName, r.CaseBaseName, 1,
		map[string]string{"app": workloadName}, r.VeleroCfg.ImageRegistryProxy, r.VeleroCfg.WorkerOS).
		WithVolume(CreateVolumes(workloadName, []string{"volume"})).Result()
	container := &deployment.Spec.Template.Spec.Containers[0]
	// A raw block device, not a mount. The device node is created root-owned,
	// so the writer runs as root; it still drops every capability.
	container.VolumeMounts = nil
	container.VolumeDevices = []corev1api.VolumeDevice{{Name: "volume", DevicePath: devicePath}}
	container.SecurityContext.RunAsNonRoot = func(b bool) *bool { return &b }(false)
	container.SecurityContext.RunAsUser = func(i int64) *int64 { return &i }(0)
	container.SecurityContext.RunAsGroup = func(i int64) *int64 { return &i }(0)
	deployment.Spec.Template.Spec.SecurityContext = nil
	if _, err := CreateDeployment(r.Client.ClientGo, r.CaseBaseName, deployment); err != nil {
		return err
	}
	return WaitForReadyDeployment(r.Client.ClientGo, r.CaseBaseName, workloadName)
}

func (r *retainedSnapshotCase) createPolicy() error {
	By(fmt.Sprintf("Create the retention volume policy ConfigMap %s", r.policyCM))
	if err := CreateConfigMapFromYAMLData(r.Client.ClientGo, policyYAML, r.policyCM, r.VeleroCfg.VeleroNamespace); err != nil {
		return err
	}
	return WaitForConfigMapComplete(r.Client.ClientGo, r.VeleroCfg.VeleroNamespace, r.policyCM)
}

// writeData overwrites the first mebibytes of the block device with random
// data and returns the checksum of the whole device.
func (r *retainedSnapshotCase) writeData(mebibytes int) (string, error) {
	By(fmt.Sprintf("Write %dMiB of random data to the block device", mebibytes))
	script := fmt.Sprintf("dd if=/dev/urandom of=%s bs=1M count=%d conv=notrunc && sync && sha256sum %s", devicePath, mebibytes, devicePath)
	out, err := r.exec(script)
	if err != nil {
		return "", err
	}
	return checksumOf(out), nil
}

func (r *retainedSnapshotCase) readChecksum() (string, error) {
	out, err := r.exec("sha256sum " + devicePath)
	if err != nil {
		return "", err
	}
	return checksumOf(out), nil
}

func checksumOf(sha256sumOutput string) string {
	lines := strings.Split(strings.TrimSpace(sha256sumOutput), "\n")
	return strings.Fields(lines[len(lines)-1])[0]
}

func (r *retainedSnapshotCase) exec(script string) (string, error) {
	stdout, _, err := KubectlExec(r.Ctx, "--namespace", r.CaseBaseName,
		"exec", "deployment/"+workloadName, "--", "sh", "-c", script)
	return stdout, err
}

func (r *retainedSnapshotCase) backupArgs(backupName string, withPolicy bool) []string {
	args := []string{
		"backup", "create", backupName, "--namespace", r.VeleroCfg.VeleroNamespace,
		"--include-namespaces", r.CaseBaseName, "--snapshot-volumes=true",
		"--default-volumes-to-fs-backup=false", "--snapshot-move-data=true",
		"--data-mover", "velero-block",
	}
	if withPolicy {
		args = append(args, "--resource-policies-configmap", r.policyCM)
	}
	return args
}

// backup runs a backup to completion while a watcher follows its DataUpload,
// and returns the completed DataUpload and what the watcher saw.
func (r *retainedSnapshotCase) backup(backupName string, withPolicy bool) (*velerov2alpha1api.DataUpload, *dataUploadObservation, error) {
	By(fmt.Sprintf("Back up namespace %s as %s with the velero-block data mover (retention policy: %t)", r.CaseBaseName, backupName, withPolicy))
	watcher := r.watchDataUpload(backupName)
	defer watcher.stop()
	if err := VeleroBackupExec(r.Ctx, r.VeleroCfg.VeleroCLI, r.VeleroCfg.VeleroNamespace, backupName, append(r.backupArgs(backupName, withPolicy), "--wait")); err != nil {
		RunDebug(context.Background(), r.VeleroCfg.VeleroCLI, r.VeleroCfg.VeleroNamespace, backupName, "")
		return nil, nil, err
	}
	du, err := r.dataUploadOf(backupName)
	if err != nil {
		return nil, nil, err
	}
	obs := watcher.stop()
	return du, obs, nil
}

func (r *retainedSnapshotCase) dataUploadOf(backupName string) (*velerov2alpha1api.DataUpload, error) {
	list := new(velerov2alpha1api.DataUploadList)
	if err := r.Client.Kubebuilder.List(r.Ctx, list,
		kbclient.InNamespace(r.VeleroCfg.VeleroNamespace),
		kbclient.MatchingLabels{velerov1api.BackupNameLabel: label.GetValidName(backupName)}); err != nil {
		return nil, errors.Wrapf(err, "list the DataUploads of backup %s", backupName)
	}
	if len(list.Items) != 1 {
		return nil, errors.Errorf("backup %s has %d DataUploads, want 1", backupName, len(list.Items))
	}
	return &list.Items[0], nil
}

func (r *retainedSnapshotCase) backupUID(backupName string) (string, error) {
	backup := new(velerov1api.Backup)
	if err := r.Client.Kubebuilder.Get(r.Ctx, types.NamespacedName{Namespace: r.VeleroCfg.VeleroNamespace, Name: backupName}, backup); err != nil {
		return "", err
	}
	return string(backup.UID), nil
}

// dataUploadObservation is what the watcher saw while a DataUpload ran.
type dataUploadObservation struct {
	// handle is the snapshot handle of the backup VolumeSnapshotContent, read
	// while the content existed; it is the only way to learn the handle of a
	// snapshot that is not recorded, e.g. without the policy or after a cancel.
	handle string
	// retainSeen is whether the backup content was ever observed with
	// deletionPolicy Retain.
	retainSeen bool
	// retainBeforeRecord is set when the content was Retain while the
	// DataUpload, re-read right after, still carried no record: the ordering
	// the design forbids.
	retainBeforeRecord bool
	// recordSeen is whether the DataUpload was observed with a record.
	recordSeen bool
}

type dataUploadWatcher struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	obs    dataUploadObservation
}

func (w *dataUploadWatcher) stop() *dataUploadObservation {
	w.once.Do(func() {
		w.cancel()
		<-w.done
	})
	return &w.obs
}

// watchDataUpload polls the DataUpload of the backup and the backup content
// named after it until stopped, recording the snapshot handle and the order
// in which the record and the Retain policy appeared.
func (r *retainedSnapshotCase) watchDataUpload(backupName string) *dataUploadWatcher {
	ctx, cancel := context.WithCancel(r.Ctx)
	w := &dataUploadWatcher{cancel: cancel, done: make(chan struct{})}
	_, snapshotClient, err := csi.GetClients()
	Expect(err).NotTo(HaveOccurred())
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(watchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			du, err := r.dataUploadOf(backupName)
			if err != nil {
				continue
			}
			if du.Status.RetainedCSIStorageSnapshot != nil {
				w.obs.recordSeen = true
			}
			vsc, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().Get(ctx, du.Name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if vsc.Spec.Source.SnapshotHandle != nil && *vsc.Spec.Source.SnapshotHandle != "" {
				w.obs.handle = *vsc.Spec.Source.SnapshotHandle
			} else if vsc.Status != nil && vsc.Status.SnapshotHandle != nil {
				w.obs.handle = *vsc.Status.SnapshotHandle
			}
			if vsc.Spec.DeletionPolicy == "Retain" {
				w.obs.retainSeen = true
				if !w.obs.recordSeen {
					// The record may have landed between the two reads; only a
					// re-read that still has no record is a violation.
					if again, err := r.dataUploadOf(backupName); err == nil && again.Status.RetainedCSIStorageSnapshot == nil {
						w.obs.retainBeforeRecord = true
					}
				}
			}
		}
	}()
	return w
}

// storageSnapshotExists reports whether the hostpath driver still holds the
// snapshot: the API objects can be gone while the file is not, and vice versa.
func (r *retainedSnapshotCase) storageSnapshotExists(handle string) (bool, error) {
	Expect(handle).NotTo(BeEmpty(), "a snapshot handle is needed to check the storage")
	file := hostpathSnapshotDir + "/" + handle + hostpathSnapshotExt
	stdout, stderr, err := KubectlExec(r.Ctx, "--namespace", hostpathNamespace, "exec", hostpathPod, "-c", hostpathContainer, "--",
		"sh", "-c", fmt.Sprintf("if [ -f %s ]; then echo present; else echo absent; fi", file))
	if err != nil {
		return false, errors.Wrapf(err, "check %s in %s/%s: %s", file, hostpathPod, hostpathContainer, stderr)
	}
	switch strings.TrimSpace(stdout) {
	case "present":
		return true, nil
	case "absent":
		return false, nil
	}
	return false, errors.Errorf("unexpected output checking %s: %q", file, stdout)
}

func (r *retainedSnapshotCase) expectStorageSnapshot(handle string, present bool) {
	exists, err := r.storageSnapshotExists(handle)
	Expect(err).NotTo(HaveOccurred())
	Expect(exists).To(Equal(present), "storage snapshot %s%s present=%t in %s/%s", handle, hostpathSnapshotExt, present, hostpathPod, hostpathContainer)
}

// expectBackupSnapshotObjectsGone waits until the backup VolumeSnapshot and
// VolumeSnapshotContent named after the DataUpload are deleted.
func (r *retainedSnapshotCase) expectBackupSnapshotObjectsGone(duName string) error {
	_, snapshotClient, err := csi.GetClients()
	if err != nil {
		return err
	}
	return wait.PollUntilContextTimeout(r.Ctx, pollInterval, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
		if _, err := snapshotClient.SnapshotV1().VolumeSnapshots(r.VeleroCfg.VeleroNamespace).Get(ctx, duName, metav1.GetOptions{}); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
		if _, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().Get(ctx, duName, metav1.GetOptions{}); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
		return true, nil
	})
}

// expectRetained asserts the API side of a completed, retaining DataUpload.
func (r *retainedSnapshotCase) expectRetained(du *velerov2alpha1api.DataUpload, obs *dataUploadObservation) string {
	Expect(du.Status.Phase).To(Equal(velerov2alpha1api.DataUploadPhaseCompleted))
	Expect(du.Spec.DataMoverConfig).To(HaveKeyWithValue(velerov2alpha1api.RetainSnapshotDataMoverConfigKey, "true"))
	Expect(du.Status.RetainedCSIStorageSnapshot).NotTo(BeNil(), "DataUpload %s has no retained snapshot record", du.Name)
	record := du.Status.RetainedCSIStorageSnapshot
	Expect(record.Driver).To(Equal(hostpathDriver))
	Expect(record.CSISnapshotHandle).NotTo(BeEmpty())
	if obs.handle != "" {
		Expect(record.CSISnapshotHandle).To(Equal(obs.handle), "the record names a different snapshot than the backup content")
	}
	Expect(obs.retainBeforeRecord).To(BeFalse(), "the backup content was Retain before the DataUpload carried the record")
	fmt.Fprintf(GinkgoWriter, "DataUpload %s: handle %s, Retain observed=%t, record observed=%t, fallbackFull=%t, incrementalBytes=%v\n",
		du.Name, record.CSISnapshotHandle, obs.retainSeen, obs.recordSeen, du.Status.FallbackFull, ptr.Deref(du.Status.IncrementalBytes, -1))

	By("The backup VolumeSnapshot and VolumeSnapshotContent are deleted while the storage snapshot survives")
	Expect(r.expectBackupSnapshotObjectsGone(du.Name)).To(Succeed())
	r.expectStorageSnapshot(record.CSISnapshotHandle, true)
	return record.CSISnapshotHandle
}

// expectReleased waits until the completion of releasingBackup released the
// parent's snapshot: the release only issues the deletion, so the storage is
// polled until the snapshot is gone.
func (r *retainedSnapshotCase) expectReleased(handle, releasingBackup string) {
	By(fmt.Sprintf("The parent's snapshot %s is released by backup %s and gone from the storage", handle, releasingBackup))
	r.expectStorageSnapshotGone(handle)
	fmt.Fprintf(GinkgoWriter, "Released: parent snapshot %s, by backup %s\n", handle, releasingBackup)
}

// expectStorageSnapshotGone polls the storage until the snapshot is gone: the
// deletion is issued and the CSI snapshotter deletes it in the background.
func (r *retainedSnapshotCase) expectStorageSnapshotGone(handle string) {
	Expect(wait.PollUntilContextTimeout(r.Ctx, pollInterval, deletionTimeout, true, func(context.Context) (bool, error) {
		exists, err := r.storageSnapshotExists(handle)
		return !exists, err
	})).To(Succeed(), "storage snapshot %s%s is still present", handle, hostpathSnapshotExt)
}

// expectIncremental asserts the DataUpload computed its changed blocks against
// a base instead of reading the volume in full.
func (r *retainedSnapshotCase) expectIncremental(du *velerov2alpha1api.DataUpload) {
	Expect(du.Status.FallbackFull).To(BeFalse(), "DataUpload %s read in full", du.Name)
}

// requireCBT skips a case whose assertions need changed-block tracking when
// the CSI driver runs no snapshot metadata service: without it every backup
// reads in full and no base is ever released.
func (r *retainedSnapshotCase) requireCBT() {
	if _, _, err := KubectlExec(r.Ctx, "get", "snapshotmetadataservices.cbt.storage.k8s.io", hostpathDriver); err != nil {
		Skip(fmt.Sprintf("the CSI driver %s runs no snapshot metadata service, install it with ENABLE_SNAPSHOT_METADATA=true: %v", hostpathDriver, err))
	}
}

// deletionObservation is what the watcher saw while a backup was deleted.
type deletionObservation struct {
	tempVSCSeen bool
}

// deleteBackup deletes the backup through the Velero CLI, waits for the Backup
// to be gone, and asserts that the retained snapshots named by handles were
// deleted from the storage, synchronously, through the static vsc-<hash>
// content, and that nothing is left behind.
func (r *retainedSnapshotCase) deleteBackup(backupName string, handles ...string) {
	By(fmt.Sprintf("Delete backup %s through the CLI and wait for it to be gone", backupName))
	_, snapshotClient, err := csi.GetClients()
	Expect(err).NotTo(HaveOccurred())

	tempVSCs := make([]string, 0, len(handles))
	for _, handle := range handles {
		tempVSCs = append(tempVSCs, velerocsi.TempVSCName(hostpathDriver, handle))
	}

	// The static content of a synchronous deletion can live for milliseconds,
	// so it is observed through a watch started before the deletion, not by
	// polling.
	listed, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().List(r.Ctx, metav1.ListOptions{Limit: 1})
	Expect(err).NotTo(HaveOccurred())
	watcher, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().Watch(r.Ctx, metav1.ListOptions{ResourceVersion: listed.ResourceVersion})
	Expect(err).NotTo(HaveOccurred())
	wanted := map[string]bool{}
	for _, name := range tempVSCs {
		wanted[name] = true
	}
	done := make(chan struct{})
	var obs deletionObservation
	go func() {
		defer close(done)
		for event := range watcher.ResultChan() {
			if vsc, ok := event.Object.(*snapshotv1api.VolumeSnapshotContent); ok && wanted[vsc.Name] {
				obs.tempVSCSeen = true
			}
		}
	}()

	Expect(VeleroBackupDelete(r.Ctx, r.VeleroCfg.VeleroCLI, r.VeleroCfg.VeleroNamespace, backupName)).To(Succeed())
	Expect(wait.PollUntilContextTimeout(r.Ctx, pollInterval, deletionTimeout, true, func(ctx context.Context) (bool, error) {
		err := r.Client.Kubebuilder.Get(ctx, types.NamespacedName{Namespace: r.VeleroCfg.VeleroNamespace, Name: backupName}, new(velerov1api.Backup))
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})).To(Succeed(), func() string {
		r.dumpDeletionState(backupName)
		return fmt.Sprintf("backup %s was not deleted within %s", backupName, deletionTimeout)
	})
	watcher.Stop()
	<-done

	if len(handles) > 0 {
		By("The retained snapshot was deleted through the static VolumeSnapshotContent")
		Expect(obs.tempVSCSeen).To(BeTrue(), "no static VolumeSnapshotContent %v was observed during the deletion", tempVSCs)
	}

	By("The storage snapshots go and nothing of the deletion is left behind")
	for i, handle := range handles {
		r.expectStorageSnapshotGone(handle)
		Expect(wait.PollUntilContextTimeout(r.Ctx, pollInterval, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
			_, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().Get(ctx, tempVSCs[i], metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		})).To(Succeed(), "static VolumeSnapshotContent %s is still there", tempVSCs[i])
	}
	vscs, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().List(r.Ctx, metav1.ListOptions{
		LabelSelector: velerov1api.BackupNameLabel + "=" + label.GetValidName(backupName)})
	Expect(err).NotTo(HaveOccurred())
	Expect(vscs.Items).To(BeEmpty(), "VolumeSnapshotContents labeled for backup %s remain", backupName)
	dus := new(velerov2alpha1api.DataUploadList)
	Expect(r.Client.Kubebuilder.List(r.Ctx, dus, kbclient.InNamespace(r.VeleroCfg.VeleroNamespace),
		kbclient.MatchingLabels{velerov1api.BackupNameLabel: label.GetValidName(backupName)})).To(Succeed())
	Expect(dus.Items).To(BeEmpty(), "DataUploads of backup %s remain", backupName)
}

func (r *retainedSnapshotCase) dumpDeletionState(backupName string) {
	fmt.Fprintf(GinkgoWriter, "--- deletion state of backup %s ---\n", backupName)
	for _, args := range [][]string{
		{"get", "deletebackuprequests", "-n", r.VeleroCfg.VeleroNamespace, "-o", "yaml"},
		{"get", "volumesnapshotcontents", "-o", "yaml"},
		{"get", "events", "-A", "--field-selector", "reason=SnapshotDeleteError"},
		{"get", "events", "-n", r.VeleroCfg.VeleroNamespace, "--field-selector", "involvedObject.kind=Backup"},
	} {
		_, _, _ = KubectlExec(r.Ctx, args...)
	}
}

// describedRetainedHandle reads the retained handle `velero backup describe`
// reports for the volume, the operator-facing view of the record.
func (r *retainedSnapshotCase) describedRetainedHandle(backupName string) (string, error) {
	cmd := exec.CommandContext(r.Ctx, r.VeleroCfg.VeleroCLI,
		"backup", "describe", backupName, "--namespace", r.VeleroCfg.VeleroNamespace,
		"--details", "--output", "json")
	stdout, stderr, err := veleroexec.RunCommand(cmd)
	if err != nil {
		return "", errors.Wrapf(err, "describe backup: %s", stderr)
	}
	var description struct {
		Status struct {
			BackupVolumes struct {
				CSISnapshots json.RawMessage `json:"csiSnapshots"`
			} `json:"backupVolumes"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(stdout), &description); err != nil {
		return "", errors.Wrap(err, "decode backup description")
	}
	if len(description.Status.BackupVolumes.CSISnapshots) == 0 {
		return "", errors.Errorf("the description has no status.backupVolumes.csiSnapshots: %s", stdout)
	}
	var snapshots map[string]struct {
		DataMovement struct {
			DataMover              string `json:"dataMover"`
			BaseSnapshotRetention  string `json:"baseSnapshotRetention"`
			RetentionPolicy        string `json:"retentionPolicy"`
			RetainedSnapshotHandle string `json:"retainedSnapshotHandle"`
		} `json:"dataMovement"`
	}
	if err := json.Unmarshal(description.Status.BackupVolumes.CSISnapshots, &snapshots); err != nil {
		return "", errors.Wrapf(err, "decode the CSI snapshots of the description: %s", description.Status.BackupVolumes.CSISnapshots)
	}
	entry, found := snapshots[r.CaseBaseName+"/"+workloadName]
	if !found {
		return "", errors.Errorf("the description has no CSI snapshot for %s/%s: %s", r.CaseBaseName, workloadName, description.Status.BackupVolumes.CSISnapshots)
	}
	Expect(entry.DataMovement.DataMover).To(Equal("velero-block"))
	Expect(entry.DataMovement.RetentionPolicy).To(Equal(retentionPolicyEntry))
	fmt.Fprintf(GinkgoWriter, "describe %s: baseSnapshotRetention=%q retentionPolicy=%q retainedSnapshotHandle=%q\n",
		backupName, entry.DataMovement.BaseSnapshotRetention, entry.DataMovement.RetentionPolicy, entry.DataMovement.RetainedSnapshotHandle)
	return entry.DataMovement.RetainedSnapshotHandle, nil
}

// restoreAndVerify deletes the workload namespace, restores it from the backup
// and checks the device content against the checksum taken at backup time.
func (r *retainedSnapshotCase) restoreAndVerify(backupName, restoreName, checksum string) error {
	By(fmt.Sprintf("Delete namespace %s and restore it from backup %s as %s", r.CaseBaseName, backupName, restoreName))
	if err := CleanupNamespacesWithPoll(r.Ctx, r.Client, r.CaseBaseName); err != nil {
		return err
	}
	args := []string{"restore", "create", restoreName, "--namespace", r.VeleroCfg.VeleroNamespace,
		"--from-backup", backupName, "--wait"}
	if err := VeleroRestoreExec(r.Ctx, r.VeleroCfg.VeleroCLI, r.VeleroCfg.VeleroNamespace, restoreName, args, velerov1api.RestorePhaseCompleted); err != nil {
		RunDebug(context.Background(), r.VeleroCfg.VeleroCLI, r.VeleroCfg.VeleroNamespace, "", restoreName)
		return err
	}
	if err := WaitForReadyDeployment(r.Client.ClientGo, r.CaseBaseName, workloadName); err != nil {
		return err
	}
	restored, err := r.readChecksum()
	if err != nil {
		return err
	}
	Expect(restored).To(Equal(checksum), "the device restored from %s differs from the data at backup time", backupName)
	return nil
}

func (r *retainedSnapshotCase) Clean() error {
	if !(CurrentSpecReport().Failed() && r.VeleroCfg.FailFast) {
		if err := DeleteConfigMap(r.Client.ClientGo, r.VeleroCfg.VeleroNamespace, r.policyCM); err != nil && !apierrors.IsNotFound(err) {
			fmt.Println("Fail to delete the policy ConfigMap: ", err)
		}
	}
	return r.TestCase.Clean()
}

// lifecycleCase: one backup under the policy retains its snapshot, and its
// deletion returns without waiting on the driver: with the snapshot's
// deletion already in flight, the backup goes and the static content in
// deletion names the snapshot for manual cleanup.
type lifecycleCase struct {
	retainedSnapshotCase
	handle string
}

func (c *lifecycleCase) Init() error {
	if err := c.init("retain-lifecycle"); err != nil {
		return err
	}
	c.BackupName = "backup-" + c.CaseBaseName
	c.TestMsg = &TestMSG{
		Desc:      "velero-block backup with base snapshot retention, then backup deletion",
		Text:      "The DataUpload records the retained snapshot, the API objects go while the storage snapshot stays, and the backup deletion removes it from the storage",
		FailedMSG: "Base snapshot retention or its deletion did not behave as designed",
	}
	return nil
}

func (c *lifecycleCase) CreateResources() error {
	if err := c.createPolicy(); err != nil {
		return err
	}
	if err := c.createWorkload(resource.MustParse("64Mi")); err != nil {
		return err
	}
	_, err := c.writeData(16)
	return err
}

func (c *lifecycleCase) Backup() error {
	du, obs, err := c.backup(c.BackupName, true)
	if err != nil {
		return err
	}
	c.handle = c.expectRetained(du, obs)

	By("velero backup describe reports the retained snapshot")
	described, err := c.describedRetainedHandle(c.BackupName)
	if err != nil {
		return err
	}
	Expect(described).To(Equal(c.handle))
	return nil
}

func (c *lifecycleCase) Verify() error {
	By("A backup deletion does not wait for a snapshot whose deletion is in flight")
	c.blockRelease(c.handle)
	start := time.Now()
	c.deleteBackup(c.BackupName)
	Expect(time.Since(start)).To(BeNumerically("<", 3*time.Minute), "the deletion waited on the driver")
	_, snapshotClient, err := csi.GetClients()
	Expect(err).NotTo(HaveOccurred())
	left, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().Get(c.Ctx, velerocsi.TempVSCName(hostpathDriver, c.handle), metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "the static content in deletion is the trace for manual cleanup")
	Expect(left.DeletionTimestamp).NotTo(BeNil())
	Expect(left.Spec.Source.SnapshotHandle).NotTo(BeNil())
	Expect(*left.Spec.Source.SnapshotHandle).To(Equal(c.handle), "the content left in deletion names the snapshot")
	c.expectStorageSnapshot(c.handle, true)

	By("Manual cleanup: the content goes, the snapshot is deleted through a new static content")
	c.unblockRelease(c.handle)
	Expect(velerocsi.DeleteRetainedSnapshot(c.Ctx, c.Client.Kubebuilder,
		&velerov2alpha1api.RetainedCSIStorageSnapshot{CSISnapshotHandle: c.handle, Driver: hostpathDriver}, "", logrus.StandardLogger())).To(Succeed())
	c.expectStorageSnapshotGone(c.handle)
	return nil
}

// chainCase: three backups of the same volume, every one after the first an
// incremental. Backup 2's release of backup 1's snapshot is made to miss -- a
// static content for that handle is left in deletion behind a test finalizer,
// so the release finds a deletion in flight and leaves it -- so backup 1's
// snapshot stays until backup 1 is deleted. Backup 3 releases backup 2's snapshot, its base, as soon as it
// completes. All three backups restore their data, deleting the released
// backup 2 changes nothing on the storage, and deleting backups 1 and 3
// removes their snapshots.
type chainCase struct {
	retainedSnapshotCase
	backups   [3]string
	checksums [3]string
	handles   [3]string
}

func (c *chainCase) Init() error {
	if err := c.init("retain-chain"); err != nil {
		return err
	}
	for i := range c.backups {
		c.backups[i] = fmt.Sprintf("backup-%d-%s", i+1, c.CaseBaseName)
	}
	c.TestMsg = &TestMSG{
		Desc:      "chain of three velero-block backups, each releasing its base",
		Text:      "Each completed incremental releases its base; a missed release leaves the snapshot to its backup's deletion; every backup restores",
		FailedMSG: "A base was kept or released wrongly, or a backup of the chain did not restore",
	}
	return nil
}

func (c *chainCase) CreateResources() error {
	c.requireCBT()
	if err := c.createPolicy(); err != nil {
		return err
	}
	return c.createWorkload(resource.MustParse("64Mi"))
}

func (c *chainCase) Backup() error {
	for i := range c.backups {
		checksum, err := c.writeData(8 * (i + 1))
		if err != nil {
			return err
		}
		c.checksums[i] = checksum
		du, obs, err := c.backup(c.backups[i], true)
		if err != nil {
			return err
		}
		c.handles[i] = c.expectRetained(du, obs)
		if i > 0 {
			c.expectIncremental(du)
		}
		switch i {
		case 0:
			c.blockRelease(c.handles[0])
		case 1:
			By("Backup 2's release of backup 1's snapshot missed: the snapshot stays")
			c.expectReleaseMissed(c.handles[0])
			c.unblockRelease(c.handles[0])
		case 2:
			By("Backup 3 released its base, backup 2's snapshot, as soon as it completed")
			c.expectReleased(c.handles[1], c.backups[2])
		}
	}
	Expect(c.handles[0]).NotTo(Equal(c.handles[1]))
	Expect(c.handles[1]).NotTo(Equal(c.handles[2]))
	By("The missed snapshot and the last snapshot are in the storage, the released one is not")
	c.expectStorageSnapshot(c.handles[0], true)
	c.expectStorageSnapshot(c.handles[1], false)
	c.expectStorageSnapshot(c.handles[2], true)
	return nil
}

// blockReleaseFinalizer keeps the static content of the blocked release in
// deletion.
const blockReleaseFinalizer = "e2e.velero.io/block-release"

// blockRelease leaves a static content for the handle in deletion, as an
// earlier request the driver could not finish would: deletionPolicy Retain, so
// the sidecar never deletes the snapshot through it, and a test finalizer that
// keeps it present. A release or a backup deletion finds the deletion in
// flight and leaves it.
func (c *retainedSnapshotCase) blockRelease(handle string) {
	By(fmt.Sprintf("Leave a static content for snapshot %s in deletion so the next release misses", handle))
	_, snapshotClient, err := csi.GetClients()
	Expect(err).NotTo(HaveOccurred())
	name := velerocsi.TempVSCName(hostpathDriver, handle)
	h := handle
	blocking := &snapshotv1api.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{blockReleaseFinalizer}},
		Spec: snapshotv1api.VolumeSnapshotContentSpec{
			DeletionPolicy: snapshotv1api.VolumeSnapshotContentRetain,
			Driver:         hostpathDriver,
			Source:         snapshotv1api.VolumeSnapshotContentSource{SnapshotHandle: &h},
			VolumeSnapshotRef: corev1api.ObjectReference{
				APIVersion: snapshotv1api.SchemeGroupVersion.String(),
				Kind:       "VolumeSnapshot",
				Namespace:  "ns-" + name,
				Name:       "name-" + name,
			},
		},
	}
	_, err = snapshotClient.SnapshotV1().VolumeSnapshotContents().Create(c.Ctx, blocking, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(snapshotClient.SnapshotV1().VolumeSnapshotContents().Delete(c.Ctx, name, metav1.DeleteOptions{})).To(Succeed())
}

// unblockRelease drops the test finalizer, so the blocking content goes with
// no DeleteSnapshot call: its policy is Retain.
func (c *retainedSnapshotCase) unblockRelease(handle string) {
	_, snapshotClient, err := csi.GetClients()
	Expect(err).NotTo(HaveOccurred())
	name := velerocsi.TempVSCName(hostpathDriver, handle)
	Expect(wait.PollUntilContextTimeout(c.Ctx, pollInterval, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
		vsc, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, nil
		}
		var kept []string
		for _, f := range vsc.Finalizers {
			if f != blockReleaseFinalizer {
				kept = append(kept, f)
			}
		}
		vsc.Finalizers = kept
		// A conflict or a transient error is retried on the next poll.
		_, _ = snapshotClient.SnapshotV1().VolumeSnapshotContents().Update(ctx, vsc, metav1.UpdateOptions{})
		return false, nil
	})).To(Succeed(), "the blocking content %s did not go", name)
}

// expectReleaseMissed asserts the release of the snapshot missed: the snapshot
// stays; the end of the chain proves it stays past later releases.
func (c *chainCase) expectReleaseMissed(handle string) {
	c.expectStorageSnapshot(handle, true)
}

// Destroy is a no-op: restoreAndVerify deletes the namespace before each restore.
func (c *chainCase) Destroy() error { return nil }

func (c *chainCase) Restore() error {
	By("Every backup of the chain restores its data: restores read the repository, not the storage snapshots")
	for i := range c.backups {
		if err := c.restoreAndVerify(c.backups[i], fmt.Sprintf("restore-%d-%s", i+1, c.CaseBaseName), c.checksums[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *chainCase) Verify() error {
	By("Deleting the released backup 2 succeeds and changes nothing on the storage")
	c.deleteBackup(c.backups[1])
	c.expectStorageSnapshot(c.handles[0], true)
	c.expectStorageSnapshot(c.handles[2], true)

	By("Deleting backup 1 removes the snapshot its missed release left")
	c.deleteBackup(c.backups[0], c.handles[0])
	c.expectStorageSnapshot(c.handles[2], true)

	By("Deleting backup 3 removes the last snapshot")
	c.deleteBackup(c.backups[2], c.handles[2])
	return nil
}

// holderCase: two backups of a volume; the second, an incremental, releases
// the first's snapshot and holds the base. Deleting the holder deletes its
// snapshot; the next backup's parent is then the first backup, whose base is
// gone, so it reads in full and records the driver's reason.
type holderCase struct {
	retainedSnapshotCase
	backups [3]string
	handles [3]string
}

func (c *holderCase) Init() error {
	if err := c.init("retain-holder"); err != nil {
		return err
	}
	for i := range c.backups {
		c.backups[i] = fmt.Sprintf("backup-%d-%s", i+1, c.CaseBaseName)
	}
	c.TestMsg = &TestMSG{
		Desc:      "deleting the backup holding the base",
		Text:      "The next backup reads in full and records why",
		FailedMSG: "The next backup after deleting the base holder did not read in full with a reason",
	}
	return nil
}

func (c *holderCase) CreateResources() error {
	c.requireCBT()
	if err := c.createPolicy(); err != nil {
		return err
	}
	return c.createWorkload(resource.MustParse("64Mi"))
}

func (c *holderCase) Backup() error {
	for i := 0; i < 2; i++ {
		if _, err := c.writeData(8 * (i + 1)); err != nil {
			return err
		}
		du, obs, err := c.backup(c.backups[i], true)
		if err != nil {
			return err
		}
		c.handles[i] = c.expectRetained(du, obs)
		if i == 1 {
			c.expectIncremental(du)
		}
	}
	c.expectReleased(c.handles[0], c.backups[1])
	return nil
}

// Destroy is a no-op: the next backup runs against the same volume.
func (c *holderCase) Destroy() error { return nil }

func (c *holderCase) Restore() error {
	By("Delete the backup holding the base")
	c.deleteBackup(c.backups[1], c.handles[1])

	if _, err := c.writeData(24); err != nil {
		return err
	}
	du, obs, err := c.backup(c.backups[2], true)
	if err != nil {
		return err
	}
	c.handles[2] = c.expectRetained(du, obs)

	By("The next backup read in full and recorded why")
	Expect(du.Status.FallbackFull).To(BeTrue())
	Expect(ptr.Deref(du.Status.IncrementalBytes, 0)).To(Equal(du.Status.SourceSize), "a full read uploads the whole volume")
	By("The node-agent log names the cause of the full read")
	logs, _, err := KubectlExec(c.Ctx, "logs", "-n", c.VeleroCfg.VeleroNamespace, "daemonset/node-agent", "--tail=-1")
	Expect(err).NotTo(HaveOccurred())
	var cause string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=warning") && strings.Contains(line, "fallback to full") && strings.Contains(line, c.handles[0]) {
			cause = line
		}
	}
	Expect(cause).NotTo(BeEmpty(), "no Warn line in the node-agent log names the full read's cause against base %s", c.handles[0])
	fmt.Fprintf(GinkgoWriter, "Holder deleted: backup %s fallbackFull=true; node-agent: %s\n", c.backups[2], cause)
	return nil
}

func (c *holderCase) Verify() error {
	By("Deleting the released backup 1 succeeds")
	c.deleteBackup(c.backups[0])
	c.deleteBackup(c.backups[2], c.handles[2])
	return nil
}

// releaseCase: a completed retained parent is untouched by the backups of its
// volume that do not complete or do not retain. A DataUpload canceled
// mid-flight never writes a record and its snapshot goes with the cleanup; a
// DataUpload backed up without the policy never retains. The next retaining
// backup's parent is the backup without the policy, whose snapshot is gone, so
// it reads in full and releases nothing: the parent's snapshot stays until the
// parent backup is deleted.
type releaseCase struct {
	retainedSnapshotCase
	parentBackup   string
	canceledBackup string
	plainBackup    string
	nextBackup     string
	parentHandle   string
	nextHandle     string
}

func (c *releaseCase) Init() error {
	if err := c.init("retain-release"); err != nil {
		return err
	}
	c.parentBackup = "backup-parent-" + c.CaseBaseName
	c.canceledBackup = "backup-cancel-" + c.CaseBaseName
	c.plainBackup = "backup-plain-" + c.CaseBaseName
	c.nextBackup = "backup-next-" + c.CaseBaseName
	c.TestMsg = &TestMSG{
		Desc:      "canceled velero-block DataUpload under the retention policy, and a DataUpload without it, after a retained parent",
		Text:      "A DataUpload that does not complete keeps no record and no snapshot; without the policy nothing is retained; the parent stays until its backup is deleted",
		FailedMSG: "An unfinished or non-retaining DataUpload left a snapshot or a record behind, or touched the parent",
	}
	return nil
}

func (c *releaseCase) CreateResources() error {
	if err := c.createPolicy(); err != nil {
		return err
	}
	// Large enough that the upload is in progress for a few seconds, so the
	// cancel lands before the DataUpload completes.
	if err := c.createWorkload(resource.MustParse("256Mi")); err != nil {
		return err
	}
	_, err := c.writeData(256)
	return err
}

func (c *releaseCase) Backup() error {
	du, obs, err := c.backup(c.parentBackup, true)
	if err != nil {
		return err
	}
	c.parentHandle = c.expectRetained(du, obs)

	if err := c.cancelDataUpload(); err != nil {
		return err
	}
	By("The canceled backup left the retained parent untouched")
	c.expectStorageSnapshot(c.parentHandle, true)

	if err := c.backupWithoutPolicy(); err != nil {
		return err
	}
	By("The backup without the policy left the retained parent untouched")
	c.expectStorageSnapshot(c.parentHandle, true)

	if _, err := c.writeData(8); err != nil {
		return err
	}
	du, obs, err = c.backup(c.nextBackup, true)
	if err != nil {
		return err
	}
	c.nextHandle = c.expectRetained(du, obs)
	By("The next retaining backup released nothing: the parent stays until its backup is deleted")
	c.expectStorageSnapshot(c.parentHandle, true)
	return nil
}

func (c *releaseCase) cancelDataUpload() error {
	// The cancel lands once the data path is running: a cancel of a Prepared
	// DataUpload whose pod has not started yet is not acknowledged by the
	// data path and only takes effect after the controller's five-minute
	// cancel delay, which says nothing about retention.
	By(fmt.Sprintf("Start backup %s under the retention policy and cancel its DataUpload once it is in progress", c.canceledBackup))
	watcher := c.watchDataUpload(c.canceledBackup)
	defer watcher.stop()
	if err := VeleroCmdExec(c.Ctx, c.VeleroCfg.VeleroCLI, c.backupArgs(c.canceledBackup, true)); err != nil {
		return err
	}
	var duName string
	if err := wait.PollUntilContextTimeout(c.Ctx, watchInterval, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		du, err := c.dataUploadOf(c.canceledBackup)
		if err != nil {
			return false, nil
		}
		switch du.Status.Phase {
		case velerov2alpha1api.DataUploadPhaseInProgress:
		case velerov2alpha1api.DataUploadPhaseCompleted, velerov2alpha1api.DataUploadPhaseFailed, velerov2alpha1api.DataUploadPhaseCanceled:
			return false, errors.Errorf("DataUpload %s reached %s before it could be canceled", du.Name, du.Status.Phase)
		default:
			return false, nil
		}
		du.Spec.Cancel = true
		if err := c.Client.Kubebuilder.Update(ctx, du); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		duName = du.Name
		fmt.Fprintf(GinkgoWriter, "Canceled DataUpload %s in phase %s\n", du.Name, du.Status.Phase)
		return true, nil
	}); err != nil {
		return errors.Wrap(err, "cancel the DataUpload")
	}

	// The canceled data path usually ends the DataUpload Canceled, but when
	// the data mover pod terminates before the cancel event is consumed, the
	// pod-termination callback ends it Failed instead. Either way it did not
	// complete, and the retention contract is the same: no record, snapshot
	// released.
	By("The DataUpload ends Canceled or Failed with no record")
	var du *velerov2alpha1api.DataUpload
	if err := wait.PollUntilContextTimeout(c.Ctx, pollInterval, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		current := new(velerov2alpha1api.DataUpload)
		if err := c.Client.Kubebuilder.Get(ctx, types.NamespacedName{Namespace: c.VeleroCfg.VeleroNamespace, Name: duName}, current); err != nil {
			return false, err
		}
		du = current
		switch current.Status.Phase {
		case velerov2alpha1api.DataUploadPhaseCanceled, velerov2alpha1api.DataUploadPhaseFailed:
			return true, nil
		case velerov2alpha1api.DataUploadPhaseCompleted:
			return false, errors.Errorf("the canceled DataUpload %s completed", duName)
		}
		return false, nil
	}); err != nil {
		return errors.Wrapf(err, "wait for DataUpload %s to end Canceled or Failed", duName)
	}
	fmt.Fprintf(GinkgoWriter, "Canceled DataUpload %s ended %s: %s\n", du.Name, du.Status.Phase, du.Status.Message)
	Expect(du.Spec.DataMoverConfig).To(HaveKeyWithValue(velerov2alpha1api.RetainSnapshotDataMoverConfigKey, "true"))
	Expect(du.Status.RetainedCSIStorageSnapshot).To(BeNil(), "the canceled DataUpload %s kept a retained snapshot record", du.Name)

	By("The backup finishes, the backup snapshot objects are gone, and the storage snapshot was released")
	if err := c.waitBackupFinal(c.canceledBackup); err != nil {
		return err
	}
	Expect(c.expectBackupSnapshotObjectsGone(du.Name)).To(Succeed())
	obs := watcher.stop()
	Expect(obs.handle).NotTo(BeEmpty(), "the watcher never saw the backup VolumeSnapshotContent of %s, so the storage cannot be checked", du.Name)
	c.expectStorageSnapshot(obs.handle, false)
	return nil
}

func (c *releaseCase) backupWithoutPolicy() error {
	du, obs, err := c.backup(c.plainBackup, false)
	if err != nil {
		return err
	}
	By("The DataUpload was not opted in: no retainSnapshot key, no record, snapshot deleted with the backup content")
	Expect(du.Status.Phase).To(Equal(velerov2alpha1api.DataUploadPhaseCompleted))
	Expect(du.Spec.DataMoverConfig).NotTo(HaveKey(velerov2alpha1api.RetainSnapshotDataMoverConfigKey))
	Expect(du.Status.RetainedCSIStorageSnapshot).To(BeNil(), "DataUpload %s without the policy carries a record", du.Name)
	Expect(obs.retainSeen).To(BeFalse(), "the backup content of a non-retaining DataUpload was switched to Retain")
	Expect(c.expectBackupSnapshotObjectsGone(du.Name)).To(Succeed())
	Expect(obs.handle).NotTo(BeEmpty(), "the watcher never saw the backup VolumeSnapshotContent of %s, so the storage cannot be checked", du.Name)
	// The sidecar deletes the snapshot after the content is gone; give it a moment.
	Expect(wait.PollUntilContextTimeout(c.Ctx, pollInterval, cleanupTimeout, true, func(context.Context) (bool, error) {
		exists, err := c.storageSnapshotExists(obs.handle)
		return !exists, err
	})).To(Succeed(), "storage snapshot %s of the non-retaining DataUpload %s was not deleted", obs.handle, du.Name)
	return nil
}

func (c *releaseCase) waitBackupFinal(backupName string) error {
	return wait.PollUntilContextTimeout(c.Ctx, pollInterval, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		backup := new(velerov1api.Backup)
		if err := c.Client.Kubebuilder.Get(ctx, types.NamespacedName{Namespace: c.VeleroCfg.VeleroNamespace, Name: backupName}, backup); err != nil {
			return false, err
		}
		switch backup.Status.Phase {
		case velerov1api.BackupPhaseCompleted, velerov1api.BackupPhasePartiallyFailed, velerov1api.BackupPhaseFailed:
			fmt.Fprintf(GinkgoWriter, "Backup %s ended %s\n", backupName, backup.Status.Phase)
			return true, nil
		}
		return false, nil
	})
}

func (c *releaseCase) Verify() error {
	By("Every backup deletes cleanly; the retaining backups' deletions remove their snapshots")
	c.deleteBackup(c.canceledBackup)
	c.deleteBackup(c.plainBackup)
	c.deleteBackup(c.parentBackup, c.parentHandle)
	c.expectStorageSnapshot(c.nextHandle, true)
	c.deleteBackup(c.nextBackup, c.nextHandle)
	return nil
}

// degradedCase: the steps after the Completed update are best effort. A Retain
// switch the API server rejects -- the end state a node-agent that dies between
// the Completed update and the switch leaves -- and a record a DataUpload CRD
// without the field prunes both complete the backup without retention: the
// DataUpload is Completed, the content keeps Delete, the snapshot goes with the
// cleanup, and the backup deletion succeeds, the recorded snapshot being
// already gone.
type degradedCase struct {
	retainedSnapshotCase
	deniedBackup string
	prunedBackup string
}

// retainDenyPolicy rejects the completion's switch of the backup content --
// named after the DataUpload of the denied backup -- from Delete to Retain. The
// exposer's own switch of the source content, snapcontent-<uid>, stays allowed.
const retainDenyPolicy = "e2e-velero-deny-retain"

func (c *degradedCase) Init() error {
	if err := c.init("retain-degraded"); err != nil {
		return err
	}
	c.deniedBackup = "backup-denied-" + c.CaseBaseName
	c.prunedBackup = "backup-pruned-" + c.CaseBaseName
	c.TestMsg = &TestMSG{
		Desc:      "retention steps after the Completed update fail",
		Text:      "The DataUpload completes without retention, the snapshot goes, and the backup deletion succeeds",
		FailedMSG: "A failed retention step failed the backup or left a snapshot behind",
	}
	return nil
}

func (c *degradedCase) CreateResources() error {
	if err := c.createPolicy(); err != nil {
		return err
	}
	if err := c.createWorkload(resource.MustParse("64Mi")); err != nil {
		return err
	}
	_, err := c.writeData(16)
	return err
}

func (c *degradedCase) Backup() error {
	By("A Retain switch the API server rejects completes the backup without retention")
	c.applyManifest(fmt.Sprintf(`apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: %[1]s
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
    - apiGroups: ["snapshot.storage.k8s.io"]
      apiVersions: ["v1"]
      operations: ["UPDATE"]
      resources: ["volumesnapshotcontents"]
  validations:
  - expression: "!(oldObject.spec.deletionPolicy == 'Delete' && object.spec.deletionPolicy == 'Retain' && object.metadata.name.startsWith('%[2]s-'))"
    message: "e2e: the Retain switch is denied"
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: %[1]s
spec:
  policyName: %[1]s
  validationActions: ["Deny"]
`, retainDenyPolicy, c.deniedBackup))
	DeferCleanup(func() {
		_, _, _ = KubectlExec(context.Background(), "delete", "--ignore-not-found", "validatingadmissionpolicybinding,validatingadmissionpolicy", retainDenyPolicy)
	})
	// The policy takes effect once the API server's admission plugin has
	// loaded it.
	time.Sleep(10 * time.Second)
	c.expectCompletedWithoutRetention(c.deniedBackup, true)
	_, _, err := KubectlExec(c.Ctx, "delete", "validatingadmissionpolicybinding,validatingadmissionpolicy", retainDenyPolicy)
	Expect(err).NotTo(HaveOccurred())

	By("A record the DataUpload CRD prunes completes the backup without retention")
	original, _, err := KubectlExec(c.Ctx, "get", "crd", "datauploads.velero.io", "-o", "json")
	Expect(err).NotTo(HaveOccurred())
	c.applyCRD(withoutRecordField(original))
	DeferCleanup(func() { c.applyCRD(original) })
	c.expectCompletedWithoutRetention(c.prunedBackup, false)
	return nil
}

// expectCompletedWithoutRetention backs up under the retention policy and
// asserts the DataUpload completed without retention; withRecord says whether
// the record is expected on it.
func (c *degradedCase) expectCompletedWithoutRetention(backupName string, withRecord bool) {
	du, obs, err := c.backup(backupName, true)
	Expect(err).NotTo(HaveOccurred())
	Expect(du.Status.Phase).To(Equal(velerov2alpha1api.DataUploadPhaseCompleted))
	if withRecord {
		Expect(du.Status.RetainedCSIStorageSnapshot).NotTo(BeNil(), "the record is written with Completed")
	} else {
		Expect(du.Status.RetainedCSIStorageSnapshot).To(BeNil(), "the pruned record is not on the DataUpload")
	}
	Expect(obs.retainSeen).To(BeFalse(), "the backup content of %s was switched to Retain", du.Name)
	Expect(obs.handle).NotTo(BeEmpty(), "the watcher never saw the backup VolumeSnapshotContent of %s", du.Name)
	Expect(c.expectBackupSnapshotObjectsGone(du.Name)).To(Succeed())
	Expect(wait.PollUntilContextTimeout(c.Ctx, pollInterval, cleanupTimeout, true, func(context.Context) (bool, error) {
		exists, err := c.storageSnapshotExists(obs.handle)
		return !exists, err
	})).To(Succeed(), "the snapshot %s of %s that was not retained is still in the storage", obs.handle, du.Name)
	fmt.Fprintf(GinkgoWriter, "Completed without retention: DataUpload %s, snapshot %s gone, record on the DataUpload=%t\n", du.Name, obs.handle, du.Status.RetainedCSIStorageSnapshot != nil)
}

// applyManifest applies the YAML through kubectl.
func (c *degradedCase) applyManifest(manifest string) {
	// Not GinkgoT().TempDir(): that registers a DeferCleanup, which a
	// DeferCleanup cannot.
	dir, err := os.MkdirTemp("", "retain-e2e-")
	Expect(err).NotTo(HaveOccurred())
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "manifest.yaml")
	Expect(os.WriteFile(file, []byte(manifest), 0o600)).To(Succeed())
	_, stderr, err := KubectlExec(c.Ctx, "apply", "-f", file)
	Expect(err).NotTo(HaveOccurred(), stderr)
}

// applyCRD replaces the DataUpload CRD with the given JSON, unconditionally.
// It runs as a cleanup too, after the case context is canceled, so it uses
// its own.
func (c *degradedCase) applyCRD(crdJSON string) {
	var crd map[string]any
	Expect(json.Unmarshal([]byte(crdJSON), &crd)).To(Succeed())
	delete(crd["metadata"].(map[string]any), "resourceVersion")
	out, err := json.Marshal(crd)
	Expect(err).NotTo(HaveOccurred())
	dir, err := os.MkdirTemp("", "retain-e2e-")
	Expect(err).NotTo(HaveOccurred())
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "crd.json")
	Expect(os.WriteFile(file, out, 0o600)).To(Succeed())
	_, stderr, err := KubectlExec(context.Background(), "replace", "-f", file)
	Expect(err).NotTo(HaveOccurred(), stderr)
	_, stderr, err = KubectlExec(context.Background(), "wait", "--for=condition=established", "--timeout=60s", "crd/datauploads.velero.io")
	Expect(err).NotTo(HaveOccurred(), stderr)
	// The API server serves the new schema once its CRD handler is rebuilt.
	time.Sleep(5 * time.Second)
}

// withoutRecordField returns the CRD JSON without the record property in the
// status schema of every version, as a CRD that predates the field.
func withoutRecordField(crdJSON string) string {
	var crd map[string]any
	Expect(json.Unmarshal([]byte(crdJSON), &crd)).To(Succeed())
	for _, v := range crd["spec"].(map[string]any)["versions"].([]any) {
		schema := v.(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
		status := schema["properties"].(map[string]any)["status"].(map[string]any)
		delete(status["properties"].(map[string]any), "retainedCSIStorageSnapshot")
	}
	out, err := json.Marshal(crd)
	Expect(err).NotTo(HaveOccurred())
	return string(out)
}

func (c *degradedCase) Verify() error {
	By("Deleting the backups succeeds: their snapshots are already gone")
	c.deleteBackup(c.deniedBackup)
	c.deleteBackup(c.prunedBackup)
	return nil
}
