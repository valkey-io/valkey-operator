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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/controller"
	"valkey.io/valkey-operator/test/utils"
)

// Test valkey cluster on PSS restricted namespace
const pssRestrictedNamespace = "valkey-pss-restricted"

var _ = Describe("ValkeyCluster on PSS restricted namespace and readOnlyRootFilesystem", Ordered, Label("ValkeyCluster", "ReadOnlyRootFilesystem"), func() {
	const noPersistClusterName = "cluster-readonly-rootfs-sample"
	const noPersistManifest = "config/samples/v1alpha1_valkeycluster-readonly-rootfs.yaml"
	const persistClusterName = "cluster-readonly-rootfs-persistent-sample"
	const persistManifest = "config/samples/v1alpha1_valkeycluster-readonly-rootfs-persistent.yaml"

	BeforeAll(func() {
		By("creating the restricted-PSS namespace")
		cmd := exec.Command("kubectl", "delete", "ns", pssRestrictedNamespace, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "ns", pssRestrictedNamespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create restricted-PSS namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", pssRestrictedNamespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")
	})

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(pssRestrictedNamespace)
		}
	})

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "-f", noPersistManifest, "-n", pssRestrictedNamespace, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "-f", persistManifest, "-n", pssRestrictedNamespace, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "pvc",
			"-n", pssRestrictedNamespace,
			"-l", fmt.Sprintf("valkey.io/cluster=%s", persistClusterName),
			"--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "ns", pssRestrictedNamespace, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
	})

	It("starts with readOnlyRootFilesystem and no persistence", func() {
		By("creating the no-persistence readOnlyRootFilesystem cluster")
		cmd := exec.Command("kubectl", "delete", "-f", noPersistManifest, "-n", pssRestrictedNamespace, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "-f", noPersistManifest, "-n", pssRestrictedNamespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")

		By("verifying the cluster reaches Ready")
		verifyClusterReady := func(g Gomega) {
			cr, err := getClusterStatus(noPersistClusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(1)))
		}
		Eventually(verifyClusterReady).Should(Succeed())

		// /data must be wired up even without persistence, otherwise the server
		// can't write nodes.conf to a read-only rootfs.
		By("verifying the ConfigMap sets dir and cluster-config-file under /data")
		verifyConfigMap := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "configmap", controller.GetServerConfigMapName(noPersistClusterName),
				"-o", "jsonpath={.data.valkey\\.conf}",
				"-n", pssRestrictedNamespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("dir /data"))
			g.Expect(output).To(ContainSubstring("cluster-config-file /data/nodes.conf"))
		}
		Eventually(verifyConfigMap).Should(Succeed())

		By("verifying server and metrics-exporter containers have readOnlyRootFilesystem: true")
		verifyROFS := func(g Gomega) {
			for _, container := range []string{"server", "metrics-exporter"} {
				cmd := exec.Command("kubectl", "get", "pod",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", noPersistClusterName),
					"-o", fmt.Sprintf("jsonpath={.items[0].spec.containers[?(@.name=='%s')].securityContext.readOnlyRootFilesystem}", container),
					"-n", pssRestrictedNamespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal("true"),
					"container %q should have readOnlyRootFilesystem=true", container)
			}
		}
		Eventually(verifyROFS).Should(Succeed())

		By("verifying /data is backed by an emptyDir")
		verifyEmptyDir := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", noPersistClusterName),
				"-o", "jsonpath={.items[0].spec.volumes[?(@.name=='data')].emptyDir}",
				"-n", pssRestrictedNamespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())
		}
		Eventually(verifyEmptyDir).Should(Succeed())

		// Verify cluster must reach a healthy state.
		By("verifying nodes.conf is written under /data and the cluster is healthy")
		verifyHealthy := func(g Gomega) {
			podName, err := getPodName(noPersistClusterName)
			g.Expect(err).NotTo(HaveOccurred())
			podName = strings.TrimSpace(podName)

			cmd := exec.Command("kubectl", "exec", podName,
				"-n", pssRestrictedNamespace,
				"-c", "server", "--",
				"sh", "-c", "test -f /data/nodes.conf")
			_, err = utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "/data/nodes.conf should exist")

			cmd = exec.Command("kubectl", "exec", podName,
				"-n", pssRestrictedNamespace,
				"-c", "server", "--",
				"valkey-cli", "CLUSTER", "INFO")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("cluster_state:ok"))
		}
		Eventually(verifyHealthy).Should(Succeed())
	})

	It("starts with readOnlyRootFilesystem, persistence, and podSecurityContext", func() {
		By("creating the persistent readOnlyRootFilesystem cluster")
		cmd := exec.Command("kubectl", "delete", "-f", persistManifest, "--ignore-not-found=true", "-n", pssRestrictedNamespace)
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "-f", persistManifest, "-n", pssRestrictedNamespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create persistent ValkeyCluster CR")

		By("verifying the cluster reaches Ready")
		verifyClusterReady := func(g Gomega) {
			cr, err := getClusterStatus(persistClusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(1)))
		}
		Eventually(verifyClusterReady).Should(Succeed())

		By("verifying the pod-level custom securityContext propagates fsGroup")
		verifyFsGroup := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", persistClusterName),
				"-o", "jsonpath={.items[0].spec.securityContext.fsGroup}",
				"-n", pssRestrictedNamespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal("56849"))
		}
		Eventually(verifyFsGroup).Should(Succeed())

		By("verifying /data is backed by a PVC")
		verifyPVC := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", persistClusterName),
				"-o", "jsonpath={.items[0].spec.volumes[?(@.name=='data')].persistentVolumeClaim.claimName}",
				"-n", pssRestrictedNamespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())
		}
		Eventually(verifyPVC).Should(Succeed())

		By("verifying server and metrics-exporter containers have readOnlyRootFilesystem: true")
		verifyROFS := func(g Gomega) {
			for _, container := range []string{"server", "metrics-exporter"} {
				cmd := exec.Command("kubectl", "get", "pod",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", persistClusterName),
					"-o", fmt.Sprintf("jsonpath={.items[0].spec.containers[?(@.name=='%s')].securityContext.readOnlyRootFilesystem}", container),
					"-n", pssRestrictedNamespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal("true"),
					"container %q should have readOnlyRootFilesystem=true", container)
			}
		}
		Eventually(verifyROFS).Should(Succeed())

		By("verifying nodes.conf is writable under /data and the cluster is healthy")
		verifyHealthy := func(g Gomega) {
			podName, err := getPodName(persistClusterName)
			g.Expect(err).NotTo(HaveOccurred())
			podName = strings.TrimSpace(podName)

			cmd := exec.Command("kubectl", "exec", podName,
				"-n", pssRestrictedNamespace,
				"-c", "server", "--",
				"sh", "-c", "test -f /data/nodes.conf")
			_, err = utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "/data/nodes.conf should exist")

			cmd = exec.Command("kubectl", "exec", podName,
				"-n", pssRestrictedNamespace,
				"-c", "server", "--",
				"valkey-cli", "CLUSTER", "INFO")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("cluster_state:ok"))
		}
		Eventually(verifyHealthy).Should(Succeed())
	})
})

func getPodName(clusterName string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pod",
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "jsonpath={.items[0].metadata.name}",
		"-n", pssRestrictedNamespace)
	return utils.Run(cmd)
}

func getClusterStatus(name string) (*valkeyiov1alpha1.ValkeyCluster, error) {
	cmd := exec.Command("kubectl", "get", "valkeycluster", name, "-n", pssRestrictedNamespace, "-o", "json")
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, err
	}

	var cr valkeyiov1alpha1.ValkeyCluster
	if err := json.Unmarshal([]byte(output), &cr); err != nil {
		return nil, err
	}
	return &cr, nil
}
