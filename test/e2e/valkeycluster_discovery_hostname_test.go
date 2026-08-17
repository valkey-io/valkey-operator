//go:build e2e
// +build e2e

/*
Copyright 2025 Valkey Contributors.

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("ValkeyCluster Hostname discovery", Ordered, Label("ValkeyCluster", "Discovery"), func() {
	const clusterName = "cluster-hostname-e2e"
	const stsName = "valkey-cluster-hostname-e2e-0-0"
	const podName = "valkey-cluster-hostname-e2e-0-0-0"
	const headless = "valkey-cluster-hostname-e2e"
	const announceHostnameFQDN = "$(POD_NAME).valkey-cluster-hostname-e2e.default.svc.cluster.local."
	const slotsHostname = "valkey-cluster-hostname-e2e-0-0-0.valkey-cluster-hostname-e2e.default.svc.cluster.local"

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo("default")
			utils.CollectDebugInfo(namespace)
		}
	})

	AfterAll(func() {
		_, _ = utils.Run(exec.Command("kubectl", "scale", "deploy", "valkey-operator-controller-manager",
			"-n", namespace, "--replicas=1"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false"))
	})

	It("keeps the Ready pod through serviceName recreate and announces hostname only on a new pod", func() {
		By("creating an IP-announce cluster")
		manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 0
  exporter:
    enabled: false
`, clusterName)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for Ready")
		Eventually(func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		}, 10*time.Minute, 5*time.Second).Should(Succeed())

		podUID := podUIDOf(Default, podName)
		expectIPAnnounce(Default, stsName, podName)
		Expect(stsServiceName(Default, stsName)).To(Equal(headless))
		Expect(podSubdomain(Default, podName)).To(Equal(headless))

		By("injecting a legacy per-node serviceName without replacing the pod")
		withOperatorPaused(func() {
			forceLegacyServiceName(stsName, stsName)
			Expect(podUIDOf(Default, podName)).To(Equal(podUID))
			expectIPAnnounce(Default, stsName, podName)
		})

		By("waiting for headless serviceName restore on the same Ready pod")
		var newSTSUID string
		Eventually(func(g Gomega) {
			g.Expect(stsServiceName(g, stsName)).To(Equal(headless))
			newSTSUID = stsUID(g, stsName)
			g.Expect(podUIDOf(g, podName)).To(Equal(podUID))
			g.Expect(podOwnerSTSUID(g, podName)).To(Equal(newSTSUID))
			expectIPAnnounce(g, stsName, podName)
			g.Expect(podSubdomain(g, podName)).To(Equal(headless))
		}, 3*time.Minute, 200*time.Millisecond).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(podUIDOf(g, podName)).To(Equal(podUID))
			expectIPAnnounce(g, stsName, podName)
		}, 5*time.Second, 500*time.Millisecond).Should(Succeed())

		By("switching the cluster to Hostname announce")
		patch := `{"spec":{"networking":{"discovery":{"preferredEndpointType":"Hostname"}}}}`
		_, err = utils.Run(exec.Command("kubectl", "patch", "valkeycluster", clusterName, "--type=merge", "-p", patch))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for hostname announce only on a replacement pod")
		Eventually(func(g Gomega) {
			uid := podUIDOf(g, podName)
			podCmd := serverCommand(g, podName)
			if uid == podUID && commandHasHostname(podCmd) {
				Fail("original pod announced hostname")
			}
			g.Expect(uid).NotTo(Equal(podUID))
			g.Expect(strings.Join(podCmd, " ")).To(ContainSubstring("--cluster-announce-hostname"))
			g.Expect(strings.Join(podCmd, " ")).To(ContainSubstring(announceHostnameFQDN))
			g.Expect(strings.Join(stsServerCommand(g, stsName), " ")).To(ContainSubstring(announceHostnameFQDN))
		}, 10*time.Minute, 2*time.Second).Should(Succeed())

		By("waiting for Ready, preferred endpoint type, and CLUSTER SLOTS hostname")
		Eventually(func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			pref, err := utils.Run(exec.Command("kubectl", "exec", podName, "-c", "server", "--",
				"valkey-cli", "CONFIG", "GET", "cluster-preferred-endpoint-type"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pref).To(ContainSubstring("hostname"))
			slots, err := utils.Run(exec.Command("kubectl", "exec", podName, "-c", "server", "--",
				"valkey-cli", "CLUSTER", "SLOTS"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(slots).To(ContainSubstring(slotsHostname))
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})
})

func expectIPAnnounce(g Gomega, stsName, podName string) {
	GinkgoHelper()
	g.Expect(podReady(g, podName)).To(Equal("True"))
	g.Expect(podDeletionTimestamp(g, podName)).To(BeEmpty())
	for _, cmd := range [][]string{stsServerCommand(g, stsName), serverCommand(g, podName)} {
		g.Expect(cmd).To(ContainElement("--cluster-announce-ip"))
		g.Expect(cmd).NotTo(ContainElement("--cluster-announce-hostname"))
	}
}

func commandHasHostname(cmd []string) bool {
	return strings.Contains(strings.Join(cmd, " "), "--cluster-announce-hostname")
}

func podUIDOf(g Gomega, name string) string {
	GinkgoHelper()
	uid := kubectlJSONPath(g, "pod", name, "{.metadata.uid}")
	g.Expect(uid).NotTo(BeEmpty())
	return uid
}

func stsUID(g Gomega, name string) string {
	GinkgoHelper()
	uid := kubectlJSONPath(g, "sts", name, "{.metadata.uid}")
	g.Expect(uid).NotTo(BeEmpty())
	return uid
}

func stsServiceName(g Gomega, name string) string {
	GinkgoHelper()
	return kubectlJSONPath(g, "sts", name, "{.spec.serviceName}")
}

func podOwnerSTSUID(g Gomega, name string) string {
	GinkgoHelper()
	return kubectlJSONPath(g, "pod", name,
		"{.metadata.ownerReferences[?(@.kind=='StatefulSet')].uid}")
}

func podSubdomain(g Gomega, name string) string {
	GinkgoHelper()
	return kubectlJSONPath(g, "pod", name, "{.spec.subdomain}")
}

func podReady(g Gomega, name string) string {
	GinkgoHelper()
	return kubectlJSONPath(g, "pod", name, `{.status.conditions[?(@.type=="Ready")].status}`)
}

func podDeletionTimestamp(g Gomega, name string) string {
	GinkgoHelper()
	return kubectlJSONPath(g, "pod", name, "{.metadata.deletionTimestamp}")
}

func serverCommand(g Gomega, pod string) []string {
	GinkgoHelper()
	return parseCommand(kubectlJSONPath(g, "pod", pod,
		`{.spec.containers[?(@.name=="server")].command}`))
}

func stsServerCommand(g Gomega, name string) []string {
	GinkgoHelper()
	return parseCommand(kubectlJSONPath(g, "sts", name,
		`{.spec.template.spec.containers[?(@.name=="server")].command}`))
}

func kubectlJSONPath(g Gomega, kind, name, path string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", kind, name, "-o", "jsonpath="+path))
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

func parseCommand(raw string) []string {
	var cmd []string
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		return strings.Fields(raw)
	}
	return cmd
}

func forceLegacyServiceName(stsName, legacyServiceName string) {
	GinkgoHelper()
	raw, err := utils.Run(exec.Command("kubectl", "get", "sts", stsName, "-o", "json"))
	Expect(err).NotTo(HaveOccurred())

	var obj unstructured.Unstructured
	Expect(json.Unmarshal([]byte(raw), &obj)).To(Succeed())
	Expect(unstructured.SetNestedField(obj.Object, legacyServiceName, "spec", "serviceName")).To(Succeed())
	unstructured.RemoveNestedField(obj.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(obj.Object, "metadata", "uid")
	unstructured.RemoveNestedField(obj.Object, "metadata", "generation")
	unstructured.RemoveNestedField(obj.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	delete(obj.Object, "status")

	rewritten, err := json.Marshal(obj.Object)
	Expect(err).NotTo(HaveOccurred())

	_, err = utils.Run(exec.Command("kubectl", "delete", "sts", stsName, "--cascade=orphan", "--wait=true"))
	Expect(err).NotTo(HaveOccurred())
	Eventually(func(g Gomega) {
		_, err := utils.Run(exec.Command("kubectl", "get", "sts", stsName))
		g.Expect(err).To(HaveOccurred())
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

	create := exec.Command("kubectl", "create", "-f", "-")
	create.Stdin = strings.NewReader(string(rewritten))
	_, err = utils.Run(create)
	Expect(err).NotTo(HaveOccurred())
	Expect(stsServiceName(Default, stsName)).To(Equal(legacyServiceName))
}

func withOperatorPaused(body func()) {
	GinkgoHelper()
	defer scaleOperator(1)
	scaleOperator(0)
	body()
}

func scaleOperator(replicas int) {
	GinkgoHelper()
	_, err := utils.Run(exec.Command("kubectl", "scale", "deploy", "valkey-operator-controller-manager",
		"-n", namespace, fmt.Sprintf("--replicas=%d", replicas)))
	Expect(err).NotTo(HaveOccurred())
	if replicas == 0 {
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", namespace,
				"-l", "control-plane=controller-manager", "-o", "jsonpath={.items[*].metadata.name}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeEmpty())
		}, 60*time.Second, time.Second).Should(Succeed())
		return
	}
	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command("kubectl", "get", "deploy", "valkey-operator-controller-manager",
			"-n", namespace, "-o", "jsonpath={.status.availableReplicas}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal(fmt.Sprintf("%d", replicas)))
	}, 2*time.Minute, time.Second).Should(Succeed())
}
