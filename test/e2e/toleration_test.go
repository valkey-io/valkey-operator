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
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("Valkey Tolerations", Label("toleration"), func() {
	var taintedNode string
	Context("Deployment have tolerations", Label("toleration-enabled"), func() {
		It("should deploy ValkeyCluster with metrics exporter sidecar by default", func() {})
	})

	Context("Deployment do not have tolerations", Label("toleration-disabled"), func() {
		It("should not schedule on tainted node", func() {
			By("getting a worker node to taint")
			cmd := exec.Command("kubectl", "get", "nodes",
				"--selector=!node-role.kubernetes.io/control-plane",
				"-o", "jsonpath={.items[1].metadata.name}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get worker node to taint: %s", output))
			taintedNode = output

			By("tainting the worker node")
			taint := "dedicated=valkey:NoSchedule"
			output, err = taintNode(taintedNode, taint)
			Expect(err).NotTo(HaveOccurred(), "Failed to taint worker node: %s", output)

			By("creating a ValkeyCluster")
			valkeyName := "valkey-cluster-no-tolerations"
			valkeyYaml := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  tolerations: []
`, valkeyName)

			manifestFile := filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.yaml", valkeyName, time.Now().UnixNano()))
			err = os.WriteFile(manifestFile, []byte(valkeyYaml), 0644)
			Expect(err).NotTo(HaveOccurred(), "Failed to write manifest file")
			defer func() {
				Expect(os.Remove(manifestFile)).To(Succeed())
			}()

			By("applying the CR")
			cmd = exec.Command("kubectl", "create", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")

			By("verifying the pods placement")
			verifyPodToleration := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyName),
					"-o", "go-template={{ range .items}}{{ .spec.nodeName }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get pods' node placement")
				g.Expect(output).NotTo(ContainSubstring(taintedNode), "Pod got scheduled on the tainted node: %s", output)
			}
			Eventually(verifyPodToleration).Should(Succeed())

			By("removing the taint")
			taintNode(taintedNode, taint+"-")
		})
	})
})

func taintNode(nodeName string, taint string) (string, error) {
	By("tainting worker node")
	cmd := exec.Command("kubectl", "taint", "nodes", nodeName, taint, "--overwrite=true")
	return utils.Run(cmd)
}
