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
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster external access", Ordered, Label("ValkeyCluster", "ExternalAccess"), func() {
	const clusterName = "cluster-external-access-sample"
	const manifestPath = "config/samples/v1alpha1_valkeycluster-external-access.yaml"

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	AfterAll(func() {
		utils.Run(exec.Command("kubectl", "delete", "-f", manifestPath, "--ignore-not-found=true"))
	})

	It("exposes each shard through a NodePort Service and reports the ports in status", func() {
		By("creating the external-access cluster manifest")
		utils.Run(exec.Command("kubectl", "delete", "-f", manifestPath, "--ignore-not-found=true"))
		_, err := utils.Run(exec.Command("kubectl", "create", "-f", manifestPath))
		Expect(err).NotTo(HaveOccurred(), "Failed to create external-access ValkeyCluster CR")

		By("verifying the cluster reaches Ready")
		verifyClusterReady := func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
		}
		Eventually(verifyClusterReady, 10*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying a NodePort Service exists per shard")
		verifyServices := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "svc",
				"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/shard-index", clusterName),
				"-o", "jsonpath={range .items[*]}{.metadata.name}:{.spec.type}{\"\\n\"}{end}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			lines := utils.GetNonEmptyLines(output)
			g.Expect(lines).To(HaveLen(3))
			for _, line := range lines {
				g.Expect(line).To(ContainSubstring(":NodePort"))
			}
		}
		Eventually(verifyServices).Should(Succeed())

		By("verifying the status reports allocated external ports for every shard")
		verifyStatus := func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.ExternalEndpoints).To(HaveLen(3))
			for _, ep := range cr.Status.ExternalEndpoints {
				// One primary + one replica per shard.
				g.Expect(ep.NodePorts).To(HaveLen(2))
				for _, port := range ep.NodePorts {
					g.Expect(port).To(BeNumerically(">=", 30000))
					g.Expect(port).To(BeNumerically("<=", 32767))
				}
			}
		}
		Eventually(verifyStatus).Should(Succeed())

		By("verifying each shard Service port resolves to exactly one endpoint")
		verifyEndpoints := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "endpointslices",
				"-l", fmt.Sprintf("kubernetes.io/service-name=%s", controllerShardServiceName(clusterName, 0)),
				"-o", "jsonpath={range .items[*].endpoints[*]}{.addresses[0]}{\"\\n\"}{end}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(utils.GetNonEmptyLines(output)).NotTo(BeEmpty())
		}
		Eventually(verifyEndpoints).Should(Succeed())

		By("verifying the announced shard hostname appears in CLUSTER NODES")
		verifyHostname := func(g Gomega) {
			cmd := exec.Command("kubectl", "exec",
				fmt.Sprintf("valkey-%s-0-0-0", clusterName), "-c", "server", "--",
				"valkey-cli", "CLUSTER", "NODES")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("shard-0.valkey.example.com"))
		}
		Eventually(verifyHostname).Should(Succeed())
	})
})

// controllerShardServiceName mirrors controller.shardServiceName, which is
// unexported, so the e2e test can address the per-shard Service by name.
func controllerShardServiceName(clusterName string, shardIndex int) string {
	return fmt.Sprintf("valkey-%s-shard-%d", clusterName, shardIndex)
}
