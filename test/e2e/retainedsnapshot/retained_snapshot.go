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
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kbclient "sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/label"
	velerocsi "github.com/velero-io/velero/pkg/util/csi"
	veleroexec "github.com/velero-io/velero/pkg/util/exec"
	"github.com/velero-io/velero/pkg/util/retainedsnapshot"
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
	// creates a temporary content per snapshot and waits for the CSI sidecar to
	// delete it, which the retained-snapshot controller itself bounds at five
	// minutes per content.
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
	Context("deleting one backup of a chain keeps the other backups' retained snapshots and restores",
		Label("CSI", "DataMover", "BlockDataMover", "RetainedSnapshot", "Chain"),
		TestFunc(&chainCase{}),
	)
}

var RetainedSnapshotReleaseTest = func() {
	Context("a canceled DataUpload releases its snapshot and a DataUpload without the policy never retains",
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
	Expect(du.Spec.DataMoverConfig).To(HaveKeyWithValue(velerov2alpha1api.RetainSnapshotPolicyDataMoverConfigKey, retentionPolicyEntry))
	Expect(du.Status.RetainedCSIStorageSnapshot).NotTo(BeNil(), "DataUpload %s has no retained snapshot record", du.Name)
	record := du.Status.RetainedCSIStorageSnapshot
	Expect(record.Driver).To(Equal(hostpathDriver))
	Expect(record.CSISnapshotHandle).NotTo(BeEmpty())
	if obs.handle != "" {
		Expect(record.CSISnapshotHandle).To(Equal(obs.handle), "the record names a different snapshot than the backup content")
	}
	Expect(obs.retainBeforeRecord).To(BeFalse(), "the backup content was Retain before the DataUpload carried the record")
	fmt.Fprintf(GinkgoWriter, "DataUpload %s: handle %s, Retain observed=%t, record observed while running=%t, fallbackReason=%q\n",
		du.Name, record.CSISnapshotHandle, obs.retainSeen, obs.recordSeen, du.Status.FallbackReason)

	By("The backup VolumeSnapshot and VolumeSnapshotContent are deleted while the storage snapshot survives")
	Expect(r.expectBackupSnapshotObjectsGone(du.Name)).To(Succeed())
	r.expectStorageSnapshot(record.CSISnapshotHandle, true)
	return record.CSISnapshotHandle
}

// deletionObservation is what the watcher saw while a backup was deleted.
type deletionObservation struct {
	familySeen  bool
	tempVSCSeen bool
}

// deleteBackup deletes the backup through the Velero CLI, waits for the Backup
// to be gone, and asserts that the retained snapshots named by handles were
// deleted from the storage through the record family and a temp content, and
// that nothing of that machinery is left behind.
func (r *retainedSnapshotCase) deleteBackup(backupName string, handles ...string) {
	By(fmt.Sprintf("Delete backup %s through the CLI and wait for it to be gone", backupName))
	uid, err := r.backupUID(backupName)
	Expect(err).NotTo(HaveOccurred())
	_, snapshotClient, err := csi.GetClients()
	Expect(err).NotTo(HaveOccurred())

	tempVSCs := make([]string, 0, len(handles))
	for _, handle := range handles {
		tempVSCs = append(tempVSCs, velerocsi.TempVSCName(hostpathDriver, handle))
	}
	familyPrefix := retainedsnapshot.FamilyNamePrefix(uid)

	ctx, cancel := context.WithCancel(r.Ctx)
	done := make(chan struct{})
	var obs deletionObservation
	go func() {
		defer close(done)
		ticker := time.NewTicker(watchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if !obs.familySeen {
				if shards, err := r.familyShards(ctx, familyPrefix); err == nil && len(shards) > 0 {
					obs.familySeen = true
				}
			}
			if !obs.tempVSCSeen {
				for _, name := range tempVSCs {
					if _, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().Get(ctx, name, metav1.GetOptions{}); err == nil {
						obs.tempVSCSeen = true
					}
				}
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
	cancel()
	<-done

	if len(handles) > 0 {
		By("The record was copied into the retained-snapshots ConfigMap family and deleted through a temp VolumeSnapshotContent")
		Expect(obs.familySeen).To(BeTrue(), "no %s* ConfigMap was observed during the deletion", familyPrefix)
		Expect(obs.tempVSCSeen).To(BeTrue(), "no temp VolumeSnapshotContent %v was observed during the deletion", tempVSCs)
	}

	By("Nothing of the deletion is left behind and the storage snapshots are gone")
	for i, handle := range handles {
		r.expectStorageSnapshot(handle, false)
		_, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().Get(r.Ctx, tempVSCs[i], metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "temp VolumeSnapshotContent %s is still there (err=%v)", tempVSCs[i], err)
	}
	shards, err := r.familyShards(r.Ctx, familyPrefix)
	Expect(err).NotTo(HaveOccurred())
	Expect(shards).To(BeEmpty(), "record family %s* was not removed", familyPrefix)
	vscs, err := snapshotClient.SnapshotV1().VolumeSnapshotContents().List(r.Ctx, metav1.ListOptions{
		LabelSelector: velerov1api.BackupNameLabel + "=" + label.GetValidName(backupName)})
	Expect(err).NotTo(HaveOccurred())
	Expect(vscs.Items).To(BeEmpty(), "VolumeSnapshotContents labeled for backup %s remain", backupName)
	dus := new(velerov2alpha1api.DataUploadList)
	Expect(r.Client.Kubebuilder.List(r.Ctx, dus, kbclient.InNamespace(r.VeleroCfg.VeleroNamespace),
		kbclient.MatchingLabels{velerov1api.BackupNameLabel: label.GetValidName(backupName)})).To(Succeed())
	Expect(dus.Items).To(BeEmpty(), "DataUploads of backup %s remain", backupName)
	for _, handle := range handles {
		r.expectNoOrphanEntry(handle)
	}
}

// familyShards lists every shard of the backup's record family: shard 0, the
// manifest, which carries the family label, and the entry shards, which carry
// only the backup labels. Both carry the backup UID label and the name prefix.
func (r *retainedSnapshotCase) familyShards(ctx context.Context, prefix string) ([]string, error) {
	uid := strings.TrimSuffix(strings.TrimPrefix(prefix, "retained-snapshots-"), "-")
	list, err := r.Client.ClientGo.CoreV1().ConfigMaps(r.VeleroCfg.VeleroNamespace).List(ctx, metav1.ListOptions{LabelSelector: velerov1api.BackupUIDLabel + "=" + uid})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, cm := range list.Items {
		if strings.HasPrefix(cm.Name, prefix) {
			names = append(names, cm.Name)
		}
	}
	return names, nil
}

// expectNoOrphanEntry asserts that no orphan-snapshot ConfigMap names the
// handle: an entry there would mean the deletion gave up on the snapshot.
func (r *retainedSnapshotCase) expectNoOrphanEntry(handle string) {
	list, err := r.Client.ClientGo.CoreV1().ConfigMaps(r.VeleroCfg.VeleroNamespace).List(r.Ctx, metav1.ListOptions{LabelSelector: retainedsnapshot.OrphanLabel})
	Expect(err).NotTo(HaveOccurred())
	for _, cm := range list.Items {
		for key, value := range cm.Data {
			Expect(value).NotTo(ContainSubstring(handle), "orphan-snapshot ConfigMap %s entry %s names snapshot %s", cm.Name, key, handle)
		}
	}
}

func (r *retainedSnapshotCase) dumpDeletionState(backupName string) {
	fmt.Fprintf(GinkgoWriter, "--- deletion state of backup %s ---\n", backupName)
	for _, args := range [][]string{
		{"get", "deletebackuprequests", "-n", r.VeleroCfg.VeleroNamespace, "-o", "yaml"},
		{"get", "configmaps", "-n", r.VeleroCfg.VeleroNamespace, "-l", velerov1api.BackupUIDLabel, "-o", "yaml"},
		{"get", "volumesnapshotcontents", "-o", "yaml"},
		{"get", "events", "-n", hostpathNamespace, "--field-selector", "reason=SnapshotDeleteError"},
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

// lifecycleCase: one backup under the policy retains its snapshot; deleting
// the backup deletes the snapshot from the storage through the record.
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
	c.deleteBackup(c.BackupName, c.handle)
	return nil
}

// chainCase: three backups of the same volume, each retaining its snapshot.
// Deleting the middle one, then the first one, deletes only their snapshots,
// and the remaining backups still restore their data.
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
		Desc:      "chain of three velero-block backups with base snapshot retention",
		Text:      "Each backup retains its own snapshot; deleting one backup deletes only its snapshot and leaves the other backups restorable",
		FailedMSG: "Deleting a backup of the chain broke another backup's retained snapshot or restore",
	}
	return nil
}

func (c *chainCase) CreateResources() error {
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
	}
	Expect(c.handles[0]).NotTo(Equal(c.handles[1]))
	Expect(c.handles[1]).NotTo(Equal(c.handles[2]))
	By("All three retained snapshots are in the storage")
	for _, handle := range c.handles {
		c.expectStorageSnapshot(handle, true)
	}
	return nil
}

// Destroy is a no-op: restoreAndVerify deletes the namespace before each restore.
func (c *chainCase) Destroy() error { return nil }

func (c *chainCase) Restore() error {
	By("Delete the middle backup: only its snapshot goes")
	c.deleteBackup(c.backups[1], c.handles[1])
	c.expectStorageSnapshot(c.handles[0], true)
	c.expectStorageSnapshot(c.handles[2], true)

	if err := c.restoreAndVerify(c.backups[0], "restore-1-"+c.CaseBaseName, c.checksums[0]); err != nil {
		return err
	}
	if err := c.restoreAndVerify(c.backups[2], "restore-3a-"+c.CaseBaseName, c.checksums[2]); err != nil {
		return err
	}

	By("Delete the first backup: the last backup's snapshot stays and it still restores")
	c.deleteBackup(c.backups[0], c.handles[0])
	c.expectStorageSnapshot(c.handles[2], true)
	return c.restoreAndVerify(c.backups[2], "restore-3b-"+c.CaseBaseName, c.checksums[2])
}

func (c *chainCase) Verify() error {
	c.deleteBackup(c.backups[2], c.handles[2])
	return nil
}

// releaseCase: a DataUpload canceled mid-flight releases its snapshot instead
// of retaining it, and a DataUpload backed up without the policy never writes
// a record and never leaves a snapshot behind.
type releaseCase struct {
	retainedSnapshotCase
	canceledBackup string
	plainBackup    string
}

func (c *releaseCase) Init() error {
	if err := c.init("retain-release"); err != nil {
		return err
	}
	c.canceledBackup = "backup-cancel-" + c.CaseBaseName
	c.plainBackup = "backup-plain-" + c.CaseBaseName
	c.TestMsg = &TestMSG{
		Desc:      "canceled velero-block DataUpload under the retention policy, and a DataUpload without it",
		Text:      "A DataUpload that does not complete releases its snapshot and keeps no record; without the policy nothing is retained",
		FailedMSG: "An unfinished or non-retaining DataUpload left a snapshot or a record behind",
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
	if err := c.cancelDataUpload(); err != nil {
		return err
	}
	return c.backupWithoutPolicy()
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

	By("The DataUpload ends Canceled with no record")
	var du *velerov2alpha1api.DataUpload
	if err := wait.PollUntilContextTimeout(c.Ctx, pollInterval, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		current := new(velerov2alpha1api.DataUpload)
		if err := c.Client.Kubebuilder.Get(ctx, types.NamespacedName{Namespace: c.VeleroCfg.VeleroNamespace, Name: duName}, current); err != nil {
			return false, err
		}
		du = current
		return current.Status.Phase == velerov2alpha1api.DataUploadPhaseCanceled, nil
	}); err != nil {
		return errors.Wrapf(err, "wait for DataUpload %s to be Canceled", duName)
	}
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
	Expect(du.Spec.DataMoverConfig).NotTo(HaveKey(velerov2alpha1api.RetainSnapshotPolicyDataMoverConfigKey))
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
	By("Both backups delete cleanly: no record family, no orphan entry, no leftover content")
	c.deleteBackup(c.canceledBackup)
	c.deleteBackup(c.plainBackup)
	return nil
}
