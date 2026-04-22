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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/controller"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster persistence", Ordered, Label("ValkeyCluster", "Persistence"), func() {
	const persistentClusterName = "cluster-persistent-sample"
	const manifestPath = "config/samples/v1alpha1_valkeycluster-persistent.yaml"

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	It("creates managed PVCs and a ready persistent cluster", func() {
		By("creating the persistent cluster manifest")
		cmd := exec.Command("kubectl", "delete", "-f", manifestPath, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "-f", manifestPath)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create persistent ValkeyCluster CR")

		By("verifying the cluster reaches Ready")
		verifyClusterReady := func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(persistentClusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(1)))
		}
		Eventually(verifyClusterReady).Should(Succeed())

		By("verifying the ConfigMap contains persistence settings")
		verifyConfigMap := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "configmap", controller.GetServerConfigMapName(persistentClusterName), "-o", "jsonpath={.data.valkey\\.conf}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("dir /data"))
			g.Expect(output).To(ContainSubstring("cluster-config-file /data/nodes.conf"))
		}
		Eventually(verifyConfigMap).Should(Succeed())

		By("verifying the ValkeyNodes inherit persistence")
		verifyValkeyNodes := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "valkeynodes",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", persistentClusterName),
				"-o", "jsonpath={range .items[*]}{.spec.persistence.size}{\"\\n\"}{end}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			lines := utils.GetNonEmptyLines(output)
			g.Expect(lines).To(HaveLen(2))
			for _, line := range lines {
				g.Expect(line).To(Equal("1Gi"))
			}
		}
		Eventually(verifyValkeyNodes).Should(Succeed())

		By("verifying managed PVCs are created and bound")
		verifyPVCs := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pvc",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", persistentClusterName),
				"-o", "jsonpath={range .items[*]}{.metadata.name}:{.status.phase}:{.spec.resources.requests.storage}{\"\\n\"}{end}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			lines := utils.GetNonEmptyLines(output)
			g.Expect(lines).To(HaveLen(2))
			for _, line := range lines {
				g.Expect(line).To(ContainSubstring(":Bound:1Gi"))
			}
		}
		Eventually(verifyPVCs).Should(Succeed())

		By("verifying pods mount the managed data PVC")
		verifyPodMount := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", persistentClusterName),
				"-o", "jsonpath={.items[0].spec.containers[?(@.name=='server')].volumeMounts[?(@.name=='data')].mountPath}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("/data"))
		}
		Eventually(verifyPodMount).Should(Succeed())
	})

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "-f", manifestPath, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
	})
})
