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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("Valkey Tolerations", Label("toleration"), func() {
	var taintedNode string
	AfterEach(func() {
		By("getting node's taints")
		cmd := exec.Command("kubectl", "get", "nodes", taintedNode,
			"-o", "go-template="+
				"{{ range .spec.taints }}"+
				"{{ printf \"%s=%s:%s\" .key .value .effect }}"+
				"{{ \"\\n\" }}"+
				"{{ end}}",
		)
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get node's taints: %s", output))

		By("removing the taints")
		taints := utils.GetNonEmptyLines(output)
		for _, taint := range taints {
			applyTaint(taintedNode, strings.TrimSpace(taint)+"-")
		}
	})

	Context("Deployment have tolerations", Label("toleration-enabled"), func() {
		It("should works with single toleration", Label("single-toleration"), func() {
			By("getting a worker node to taint")
			cmd := exec.Command("kubectl", "get", "nodes",
				"--selector=!node-role.kubernetes.io/control-plane",
				"-o", "jsonpath={.items[1].metadata.name}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get worker node to taint: %s", output))
			taintedNode = output

			By("tainting the worker node")
			taint := "dedicated=valkey:NoSchedule"
			applyTaint(taintedNode, taint)

			By("creating a ValkeyCluster")
			valkeyName := "valkey-cluster-single-toleration"
			valkeyYaml := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  tolerations:
  - key: "dedicated"
    operator: "Equal"
    value: "valkey"
    effect: "NoSchedule"
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

			By("validating that the pods are running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyName),
					"-o", "jsonpath={.items[*].status.phase}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get pod status: %s", output))
				g.Expect(output).To(ContainSubstring("Running"))
			}).Should(Succeed())

			By("verifying the pods' have toleration")
			output, err = getPodToleration(fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyName))
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get pod toleration: %s", output))
			Expect(output).To(MatchJSON(`[
			{"effect":"NoSchedule","key":"dedicated","operator":"Equal","value":"valkey"},
			{"effect":"NoExecute","key":"node.kubernetes.io/not-ready","operator":"Exists","tolerationSeconds":300},
			{"effect":"NoExecute","key":"node.kubernetes.io/unreachable","operator":"Exists","tolerationSeconds":300}]
			`), fmt.Sprintf("Pod's toleration does not matches taint: %s", output))

			By("verifying the pods' placement")
			cmd = exec.Command("kubectl", "get", "pods",
				"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyName),
				"-o", "go-template={{ range .items}}{{ .spec.nodeName }} | {{ end }}",
			)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get pod's placement: %s", output))
			Expect(output).To(ContainSubstring(taintedNode), fmt.Sprintf("Pods did not get scheduled on tainted node: %s", output))
		})

		It("should works with multiple tolerations", Label("multi-tolerations"), func() {
			By("getting a worker node to taint")
			cmd := exec.Command("kubectl", "get", "nodes",
				"--selector=!node-role.kubernetes.io/control-plane",
				"-o", "jsonpath={.items[1].metadata.name}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get worker node to taint: %s", output))
			taintedNode = output

			By("tainting the worker node")
			taints := [2]string{"dedicated=valkey:PreferNoSchedule", "high-memory=valkey:NoExecute"}
			for i := 0; i < len(taints); i++ {
				applyTaint(taintedNode, taints[i])
			}

			By("creating a ValkeyCluster")
			valkeyName := "valkey-cluster-multi-tolerations"
			valkeyYaml := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  tolerations:
    - key: "dedicated"
      operator: "Equal"
      value: "valkey"
      effect: "PreferNoSchedule"
    - key: "high-memory"
      operator: "Exists"
      effect: "NoExecute"
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

			By("validating that the pods are running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyName),
					"-o", "jsonpath={.items[*].status.phase}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get pod status: %s", output))
				g.Expect(output).To(ContainSubstring("Running"))
			}).Should(Succeed())

			By("verifying the pods' have toleration")
			output, err = getPodToleration(fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyName))
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get pod toleration: %s", output))
			Expect(output).To(MatchJSON(`[
			{"effect":"PreferNoSchedule","key":"dedicated","operator":"Equal","value":"valkey"},
			{"effect":"NoExecute","key":"high-memory","operator":"Exists"},
			{"effect":"NoExecute","key":"node.kubernetes.io/not-ready","operator":"Exists","tolerationSeconds":300},
			{"effect":"NoExecute","key":"node.kubernetes.io/unreachable","operator":"Exists","tolerationSeconds":300}
			]`), fmt.Sprintf("Pod's toleration does not matches taint: %s", output))

			By("verifying the pods' placement")
			cmd = exec.Command("kubectl", "get", "pods",
				"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyName),
				"-o", "go-template={{ range .items}}{{ .spec.nodeName }} | {{ end }}",
			)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get pod's placement: %s", output))
			Expect(output).To(ContainSubstring(taintedNode), fmt.Sprintf("Pods did not get scheduled on tainted node: %s", output))
		})

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
			applyTaint(taintedNode, taint)

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

			By("validating that the pods are running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyName),
					"-o", "jsonpath={.items[*].status.phase}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get pod status: %s", output))
				g.Expect(output).To(ContainSubstring("Running"))
			}).Should(Succeed())

			By("verifying the pods placement")
			verifyPodToleration := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyName),
					"-o", "go-template={{ range .items}}{{ .spec.nodeName }} | {{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get pods' node placement")
				g.Expect(output).NotTo(ContainSubstring(taintedNode), fmt.Sprintf("Pod got scheduled on the tainted node: %s", output))
			}
			Eventually(verifyPodToleration).Should(Succeed())
		})
	})
})

// applyTaint adds or updates a taint on a specific Kubernetes node
func applyTaint(nodeName string, taint string) {
	By("applying taint to node")
	cmd := exec.Command("kubectl", "taint", "nodes", nodeName, taint, "--overwrite=true")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to taint node: %s", output))
}

// getPodToleration returns the tolerations of the first pod in the list found by pods' label
func getPodToleration(podLabel string) (string, error) {
	By("getting a pod name")
	cmd := exec.Command("kubectl", "get", "pods",
		"-l", podLabel,
		"-o", "jsonpath={.items[0].metadata.name}",
	)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get pod's name: %s", output))
	podName := output

	By("getting pod's toleration")
	cmd = exec.Command("kubectl", "get", "pods", podName,
		"-o", "jsonpath={.spec.tolerations}",
	)
	return utils.Run(cmd)
}
