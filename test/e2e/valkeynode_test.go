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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyNode", func() {
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	// createStandaloneValkeyNode applies a ValkeyNode manifest and returns a cleanup func.
	// workloadType must be "StatefulSet" or "Deployment".
	createStandaloneValkeyNode := func(name, workloadType string) func() {
		manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyNode
metadata:
  name: %s
spec:
  workloadType: %s
`, name, workloadType)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyNode %s", name)

		return func() {
			cmd := exec.Command("kubectl", "delete", "valkeynode", name, "--ignore-not-found=true", "--wait=false")
			_, _ = utils.Run(cmd)
		}
	}

	// waitForValkeyNodeReady polls until the ValkeyNode reports Ready=true.
	waitForValkeyNodeReady := func(name string) {
		Eventually(func(g Gomega) {
			node, err := utils.GetValkeyNodeStatus(name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(node.Status.Ready).To(BeTrue(), "ValkeyNode %s should be ready", name)
		}).Should(Succeed())
	}

	Context("standalone StatefulSet", func() {
		const nodeName = "valkeynode-sts-e2e"

		It("creates owned resources and populates status with role", func() {
			defer createStandaloneValkeyNode(nodeName, "StatefulSet")()

			By("waiting for the ConfigMap to be created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", nodeName+"-config")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "ConfigMap %s-config should exist", nodeName)
			}).Should(Succeed())

			By("verifying the ConfigMap contains the required script keys")
			cmd := exec.Command("kubectl", "get", "configmap", nodeName+"-config",
				"-o", "jsonpath={.data}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("liveness-check.sh"))
			Expect(output).To(ContainSubstring("readiness-check.sh"))
			Expect(output).To(ContainSubstring("valkey.conf"))

			By("waiting for the StatefulSet to be created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", "valkey-"+nodeName)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "StatefulSet valkey-%s should exist", nodeName)
			}).Should(Succeed())

			By("waiting for the ValkeyNode to become ready")
			waitForValkeyNodeReady(nodeName)

			By("verifying status fields are populated")
			node, err := utils.GetValkeyNodeStatus(nodeName)
			Expect(err).NotTo(HaveOccurred())
			Expect(node.Status.PodName).NotTo(BeEmpty(), "status.podName should be set")
			Expect(node.Status.PodIP).NotTo(BeEmpty(), "status.podIP should be set")

			By("verifying the role is reported as primary")
			Expect(node.Status.Role).To(Equal("primary"),
				"standalone ValkeyNode should report role=primary")

			By("verifying the Ready condition is set correctly")
			readyCond := utils.FindCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
			Expect(readyCond).NotTo(BeNil(), "Ready condition should be present")
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonPodRunning))
		})

		It("owned StatefulSet resourceVersion stabilises after the node is ready", func() {
			const stableName = "valkeynode-sts-stable-e2e"
			defer createStandaloneValkeyNode(stableName, "StatefulSet")()

			By("waiting for the ValkeyNode to become ready")
			waitForValkeyNodeReady(stableName)

			By("capturing the StatefulSet resourceVersion once ready")
			rvCmd := exec.Command("kubectl", "get", "statefulset", "valkey-"+stableName,
				"-o", "jsonpath={.metadata.resourceVersion}")
			rv, err := utils.Run(rvCmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(rv).NotTo(BeEmpty())

			By("verifying the StatefulSet is not updated by spurious reconciles for 30s")
			Consistently(func(g Gomega) {
				checkCmd := exec.Command("kubectl", "get", "statefulset", "valkey-"+stableName,
					"-o", "jsonpath={.metadata.resourceVersion}")
				current, err := utils.Run(checkCmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(current).To(Equal(rv), "StatefulSet resourceVersion should not change after stabilising")
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})
	})

	Context("standalone Deployment", func() {
		const nodeName = "valkeynode-deploy-e2e"

		It("creates owned resources and populates status with role", func() {
			defer createStandaloneValkeyNode(nodeName, "Deployment")()

			By("waiting for the Deployment to be created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "valkey-"+nodeName)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Deployment valkey-%s should exist", nodeName)
			}).Should(Succeed())

			By("waiting for the ValkeyNode to become ready")
			waitForValkeyNodeReady(nodeName)

			By("verifying status fields are populated")
			node, err := utils.GetValkeyNodeStatus(nodeName)
			Expect(err).NotTo(HaveOccurred())
			Expect(node.Status.PodName).NotTo(BeEmpty(), "status.podName should be set")
			Expect(node.Status.PodIP).NotTo(BeEmpty(), "status.podIP should be set")
			Expect(node.Status.Role).To(Equal("primary"),
				"standalone ValkeyNode should report role=primary")

			By("verifying no StatefulSet was created")
			cmd := exec.Command("kubectl", "get", "statefulset", "valkey-"+nodeName)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "no StatefulSet should exist for Deployment workload type")
		})

		It("owned Deployment resourceVersion stabilises after the node is ready", func() {
			const stableName = "valkeynode-deploy-stable-e2e"
			defer createStandaloneValkeyNode(stableName, "Deployment")()

			By("waiting for the ValkeyNode to become ready")
			waitForValkeyNodeReady(stableName)

			By("capturing the Deployment resourceVersion once ready")
			rvCmd := exec.Command("kubectl", "get", "deployment", "valkey-"+stableName,
				"-o", "jsonpath={.metadata.resourceVersion}")
			rv, err := utils.Run(rvCmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(rv).NotTo(BeEmpty())

			By("verifying the Deployment is not updated by spurious reconciles for 30s")
			Consistently(func(g Gomega) {
				checkCmd := exec.Command("kubectl", "get", "deployment", "valkey-"+stableName,
					"-o", "jsonpath={.metadata.resourceVersion}")
				current, err := utils.Run(checkCmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(current).To(Equal(rv), "Deployment resourceVersion should not change after stabilising")
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})
	})

	Context("pod deletion recovery", func() {
		const nodeName = "valkeynode-recovery-e2e"

		It("status tracks pod lifecycle and recovers after pod deletion", func() {
			defer createStandaloneValkeyNode(nodeName, "StatefulSet")()

			By("waiting for the ValkeyNode to become ready")
			waitForValkeyNodeReady(nodeName)

			By("recording the initial pod name")
			node, err := utils.GetValkeyNodeStatus(nodeName)
			Expect(err).NotTo(HaveOccurred())
			initialPodName := node.Status.PodName
			Expect(initialPodName).NotTo(BeEmpty())

			By("deleting the pod to simulate a crash")
			cmd := exec.Command("kubectl", "delete", "pod", initialPodName, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete pod %s", initialPodName)

			By("waiting for status to reflect the pod is gone")
			Eventually(func(g Gomega) {
				node, err := utils.GetValkeyNodeStatus(nodeName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(node.Status.Ready).To(BeFalse(),
					"ValkeyNode should not be ready while pod is terminating/restarting")
			}, 30*time.Second, time.Second).Should(Succeed())

			By("waiting for the ValkeyNode to recover to ready")
			waitForValkeyNodeReady(nodeName)

			By("verifying the pod was recreated")
			node, err = utils.GetValkeyNodeStatus(nodeName)
			Expect(err).NotTo(HaveOccurred())
			Expect(node.Status.Ready).To(BeTrue())
			Expect(node.Status.PodName).NotTo(BeEmpty())
		})
	})

	Context("external ConfigMap", func() {
		const nodeName = "valkeynode-extcm-e2e"
		const cmName = "valkeynode-extcm-scripts"

		It("uses an externally provided ConfigMap instead of creating one", func() {
			By("creating the external ConfigMap with required scripts")
			// Use the scripts embedded by the operator's own ConfigMap builder as a baseline —
			// create a minimal stub that satisfies the probe scripts.
			livenessScript := `#!/bin/bash
response=$(valkey-cli -h 127.0.0.1 -p 6379 PING 2>/dev/null || true)
if echo "$response" | grep -qE "^(PONG|LOADING|MASTERDOWN)"; then exit 0; fi
exit 1
`
			readinessScript := `#!/bin/bash
response=$(valkey-cli -h 127.0.0.1 -p 6379 PING 2>/dev/null || true)
if [ "$response" = "PONG" ]; then exit 0; fi
exit 1
`
			cmManifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
data:
  valkey.conf: ""
  liveness-check.sh: |
%s
  readiness-check.sh: |
%s
`, cmName, indentLines(livenessScript, "    "), indentLines(readinessScript, "    "))

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cmManifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create external ConfigMap")
			defer func() {
				cmd := exec.Command("kubectl", "delete", "configmap", cmName, "--ignore-not-found=true")
				_, _ = utils.Run(cmd)
			}()

			By("creating a ValkeyNode that references the external ConfigMap")
			nodeManifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyNode
metadata:
  name: %s
spec:
  serverConfigMapName: %s
`, nodeName, cmName)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(nodeManifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyNode with external ConfigMap")
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeynode", nodeName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("verifying the controller did NOT create an owned ConfigMap")
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", nodeName+"-config")
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(),
					"controller should not create a ConfigMap when ServerConfigMapName is set")
			}, 10*time.Second, 2*time.Second).Should(Succeed())

			By("waiting for the ValkeyNode to become ready using the external ConfigMap")
			waitForValkeyNodeReady(nodeName)
		})
	})

	Context("ObservedGeneration tracking", func() {
		const nodeName = "valkeynode-obsgen-e2e"

		It("status.observedGeneration is populated and tracks spec changes", func() {
			defer createStandaloneValkeyNode(nodeName, "StatefulSet")()

			By("waiting for the ValkeyNode to become ready")
			waitForValkeyNodeReady(nodeName)

			By("verifying observedGeneration equals the current generation after first reconcile")
			node, err := utils.GetValkeyNodeStatus(nodeName)
			Expect(err).NotTo(HaveOccurred())
			initialGen := node.Generation
			Expect(node.Status.ObservedGeneration).To(Equal(initialGen),
				"observedGeneration should equal generation after reconcile")

			By("patching the ValkeyNode spec to increment the generation")
			patchCmd := exec.Command("kubectl", "patch", "valkeynode", nodeName,
				"--type=merge", "-p", `{"spec":{"image":"valkey/valkey:9.0.0"}}`)
			_, err = utils.Run(patchCmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for observedGeneration to reflect the new generation")
			Eventually(func(g Gomega) {
				updated, err := utils.GetValkeyNodeStatus(nodeName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updated.Status.ObservedGeneration).To(BeNumerically(">", initialGen),
					"observedGeneration should advance after spec change")
			}).Should(Succeed())
		})
	})

	Context("rolling update readiness gate", func() {
		const nodeName = "valkeynode-rollgate-e2e"

		It("holds Ready=false during StatefulSet rolling update", func() {
			defer createStandaloneValkeyNode(nodeName, "StatefulSet")()

			By("waiting for the ValkeyNode to become ready")
			waitForValkeyNodeReady(nodeName)

			By("patching the ValkeyNode with new resource requests to trigger a rolling update")
			patchCmd := exec.Command("kubectl", "patch", "valkeynode", nodeName,
				"--type=merge", "-p",
				`{"spec":{"resources":{"requests":{"memory":"384Mi"},"limits":{"memory":"512Mi"}}}}`)
			_, err := utils.Run(patchCmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the ValkeyNode reports Ready=false during the rolling update")
			Eventually(func(g Gomega) {
				node, err := utils.GetValkeyNodeStatus(nodeName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(node.Status.Ready).To(BeFalse(),
					"ValkeyNode should report Ready=false while StatefulSet is rolling out")
			}, 30*time.Second, time.Second).Should(Succeed())

			By("waiting for the ValkeyNode to recover to ready once the rollout completes")
			waitForValkeyNodeReady(nodeName)
		})
	})
})

// indentLines prefixes every line of s with indent.
func indentLines(s, indent string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}
	return strings.Join(lines, "\n")
}
