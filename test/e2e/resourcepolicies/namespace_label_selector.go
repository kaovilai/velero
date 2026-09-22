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

package resourcepolicies

import (
	"fmt"

	"github.com/cockroachdb/errors"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	. "github.com/vmware-tanzu/velero/test/e2e/test"
	. "github.com/vmware-tanzu/velero/test/util/k8s"
)

// NamespaceLabelSelector covers design step 8 of
// https://github.com/velero-io/velero/pull/9772: a backup with no explicit
// --include-namespaces (the same shape a Schedule with no includedNamespaces produces),
// relying entirely on a ResourcePolicy ConfigMap's includedNamespacesByLabel to select which
// namespaces to back up. Only namespaces matching the label selector should end up in the
// backup; everything else - including namespaces that already existed in the cluster before
// this test ran - must not.
type NamespaceLabelSelector struct {
	TestCase
	cmName      string
	labelKey    string
	labelValue  string
	labeledNS   []string
	unlabeledNS []string
}

var NamespaceLabelSelectorTest func() = TestFunc(&NamespaceLabelSelector{})

func (n *NamespaceLabelSelector) Init() error {
	Expect(n.TestCase.Init()).To(Succeed())

	n.CaseBaseName = "ns-label-selector-" + n.UUIDgen
	n.BackupName = "backup-" + n.CaseBaseName
	n.RestoreName = "restore-" + n.CaseBaseName
	n.cmName = "cm-" + n.CaseBaseName
	n.labelKey = "velero-e2e-ns-label-selector"
	n.labelValue = n.UUIDgen

	n.labeledNS = []string{
		n.CaseBaseName + "-included-0",
		n.CaseBaseName + "-included-1",
	}
	n.unlabeledNS = []string{
		n.CaseBaseName + "-excluded-0",
		n.CaseBaseName + "-excluded-1",
	}

	n.TestMsg = &TestMSG{
		Desc: "Backup with a ResourcePolicy includedNamespacesByLabel selector and no explicit --include-namespaces",
		Text: "should back up only the namespaces matching the label selector, replacing the " +
			"default \"all namespaces\" behavior rather than unioning with it",
		FailedMSG: "Failed to select namespaces by label via ResourcePolicy",
	}

	// Deliberately no --include-namespaces: this is the scenario design step 8 calls out - a
	// Schedule (or, as here, a one-off Backup) that leaves BackupSpec.IncludedNamespaces empty
	// and relies solely on includedNamespacesByLabel to narrow the namespace set.
	n.BackupArgs = []string{
		"create", "--namespace", n.VeleroCfg.VeleroNamespace, "backup", n.BackupName,
		"--resource-policies-configmap", n.cmName,
		"--wait",
	}

	n.RestoreArgs = []string{
		"create", "--namespace", n.VeleroCfg.VeleroNamespace, "restore", n.RestoreName,
		"--from-backup", n.BackupName, "--wait",
	}

	return nil
}

func (n *NamespaceLabelSelector) CreateResources() error {
	yamlConfig := fmt.Sprintf(`version: v1
includeExcludePolicy:
  includedNamespacesByLabel:
    - "%s=%s"
`, n.labelKey, n.labelValue)

	By(fmt.Sprintf("Create ResourcePolicy configmap %s in namespace %s\n", n.cmName, n.VeleroCfg.VeleroNamespace), func() {
		Expect(CreateConfigMapFromYAMLData(n.Client.ClientGo, yamlConfig, n.cmName, n.VeleroCfg.VeleroNamespace)).To(Succeed(),
			fmt.Sprintf("Failed to create configmap %s in namespace %s\n", n.cmName, n.VeleroCfg.VeleroNamespace))
	})
	By(fmt.Sprintf("Waiting for configmap %s in namespace %s ready\n", n.cmName, n.VeleroCfg.VeleroNamespace), func() {
		Expect(WaitForConfigMapComplete(n.Client.ClientGo, n.VeleroCfg.VeleroNamespace, n.cmName)).To(Succeed(),
			fmt.Sprintf("Failed to wait configmap %s in namespace %s ready\n", n.cmName, n.VeleroCfg.VeleroNamespace))
	})

	for _, ns := range n.labeledNS {
		By(fmt.Sprintf("Create labeled namespace %s\n", ns), func() {
			Expect(CreateNamespaceWithLabel(n.Ctx, n.Client, ns, map[string]string{n.labelKey: n.labelValue})).To(Succeed(),
				fmt.Sprintf("Failed to create namespace %s", ns))
		})
		if err := n.createVerificationConfigMap(ns); err != nil {
			return err
		}
	}

	for _, ns := range n.unlabeledNS {
		By(fmt.Sprintf("Create unlabeled namespace %s\n", ns), func() {
			Expect(CreateNamespaceWithLabel(n.Ctx, n.Client, ns, map[string]string{})).To(Succeed(),
				fmt.Sprintf("Failed to create namespace %s", ns))
		})
		if err := n.createVerificationConfigMap(ns); err != nil {
			return err
		}
	}

	return nil
}

func (n *NamespaceLabelSelector) createVerificationConfigMap(ns string) error {
	cmName := "marker-" + ns
	By(fmt.Sprintf("Creating marker configmap %s in namespace %s\n", cmName, ns), func() {
		_, err := CreateConfigMap(n.Client.ClientGo, ns, cmName, map[string]string{"marker": "true"}, nil)
		Expect(err).To(Succeed(), fmt.Sprintf("Failed to create marker configmap in namespace %s", ns))
	})
	return WaitForConfigMapComplete(n.Client.ClientGo, ns, cmName)
}

func (n *NamespaceLabelSelector) Verify() error {
	for _, ns := range n.labeledNS {
		By(fmt.Sprintf("Verify labeled namespace %s was backed up and restored", ns), func() {
			_, err := GetNamespace(n.Ctx, n.Client, ns)
			Expect(err).To(Succeed(), fmt.Sprintf("Labeled namespace %s should exist after restore", ns))

			_, err = GetConfigMap(n.Client.ClientGo, ns, "marker-"+ns)
			Expect(err).To(Succeed(), fmt.Sprintf("Marker configmap in labeled namespace %s should exist after restore", ns))
		})
	}

	for _, ns := range n.unlabeledNS {
		By(fmt.Sprintf("Verify unlabeled namespace %s was NOT backed up", ns), func() {
			_, err := GetNamespace(n.Ctx, n.Client, ns)
			Expect(err).To(HaveOccurred(), fmt.Sprintf("Unlabeled namespace %s should not exist after restore - it was never in the backup", ns))
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Error should be NotFound")
		})
	}

	return nil
}

func (n *NamespaceLabelSelector) Clean() error {
	if CurrentSpecReport().Failed() && n.VeleroCfg.FailFast {
		fmt.Println("Test case failed and fail fast is enabled. Skip resource clean up.")
		return nil
	}

	if err := DeleteConfigMap(n.Client.ClientGo, n.VeleroCfg.VeleroNamespace, n.cmName); err != nil {
		return errors.Wrap(err, "failed to delete resource policy configmap")
	}

	// Unlabeled namespaces are never touched by backup/restore, so the base Clean's
	// CaseBaseName-prefix sweep is what removes them (and the labeled ones) here.
	return n.GetTestCase().Clean()
}
