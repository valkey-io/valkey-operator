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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster with StatefulSet", Ordered, func() {
	var valkeyClusterName string

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	Context("when a ValkeyCluster with StatefulSet workload is created", func() {
		It("should Create, Validate and Delete ValkeyCluster with StatefulSets", func() {
			valkeyClusterName = "valkeycluster-sts-test"
			manifest := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  workload:
    kind: StatefulSet
`, valkeyClusterName)
			manifestFile := filepath.Join(os.TempDir(), "valkeycluster-sts.yaml")
			err := os.WriteFile(manifestFile, []byte(manifest), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(manifestFile)

			By("creating the CR")
			cmd := exec.Command("kubectl", "create", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")

			By("validating the CR exists")
			verifyCrExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "ValkeyCluster", valkeyClusterName, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve ValkeyCluster CR")
				g.Expect(output).To(Equal(valkeyClusterName))
			}
			Eventually(verifyCrExists).Should(Succeed())

			By("validating the Service")
			verifyServiceExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyServiceExists).Should(Succeed())

			By("validating the ConfigMap")
			verifyConfigMapExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyConfigMapExists).Should(Succeed())

			By("validating StatefulSets exist")
			verifyStatefulSetsExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulsets",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				stsList := utils.GetNonEmptyLines(output)
				// 3 shards * (1 primary + 1 replica) = 6 nodes. Operator creates 1 STS per node.
				g.Expect(stsList).To(HaveLen(6), "Expected 6 StatefulSets")
			}
			Eventually(verifyStatefulSetsExists).Should(Succeed())

			By("validating Pods are Ready")
			verifyPodStatuses := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName),
					"-o", "go-template={{ range .items }}{{ range .status.conditions }}"+
						"{{ if and (eq .type \"Ready\") (eq .status \"True\")}}"+
						"{{ $.metadata.name}} {{ \"\\n\" }}"+
						"{{ end }}{{ end }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podStatuses := utils.GetNonEmptyLines(output)
				g.Expect(podStatuses).To(HaveLen(6), "Expected 6 Pods to be ready")
			}
			Eventually(verifyPodStatuses).Should(Succeed())

			By("validating the ValkeyCluster CR status")
			verifyCrStatus := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))
				g.Expect(cr.Status.Message).To(Equal("Cluster is healthy"))
				g.Expect(cr.Status.Shards).To(Equal(int32(3)))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))

				readyCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionReady)
				g.Expect(readyCond).NotTo(BeNil(), "Ready condition not found")
				g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))

				progressingCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
				g.Expect(progressingCond).NotTo(BeNil(), "Progressing condition not found")
				g.Expect(progressingCond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(progressingCond.Reason).To(Equal(valkeyiov1alpha1.ReasonReconcileComplete))

				degradedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
				g.Expect(degradedCond).To(BeNil(), "Degraded condition should not be present")

				clusterFormedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionClusterFormed)
				g.Expect(clusterFormedCond).NotTo(BeNil(), "ClusterFormed condition not found")
				g.Expect(clusterFormedCond.Status).To(Equal(metav1.ConditionTrue))

				slotsAssignedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionSlotsAssigned)
				g.Expect(slotsAssignedCond).NotTo(BeNil(), "SlotsAssigned condition not found")
				g.Expect(slotsAssignedCond.Status).To(Equal(metav1.ConditionTrue))
			}
			Eventually(verifyCrStatus).Should(Succeed())

			By("validating cluster access with CLUSTER INFO")
			verifyClusterAccess := func(g Gomega) {
				// Start a Valkey client pod to access the cluster and get its status.
				clusterFqdn := fmt.Sprintf("%s.default.svc.cluster.local", valkeyClusterName)

				cmd := exec.Command("kubectl", "run", "client",
					fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--",
					"valkey-cli", "-c", "-h", clusterFqdn, "CLUSTER", "INFO")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "wait", "pod/client",
					"--for=jsonpath={.status.phase}=Succeeded", "--timeout=30s")
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "logs", "client")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "delete", "pod", "client",
					"--wait=true", "--timeout=30s")
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				// The cluster should be ok.
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}
			Eventually(verifyClusterAccess).Should(Succeed())

			By("validating cluster operations with SET and GET commands")
			verifyGetSetOps := func(g Gomega) {
				clusterFqdn := fmt.Sprintf("%s.default.svc.cluster.local", valkeyClusterName)
				testPodName := fmt.Sprintf("%s-client", valkeyClusterName)
				key := "testkey"
				value := "testvalue"

				// Run a single pod that performs both SET and GET operations
				cmd := exec.Command("kubectl", "run", testPodName,
					fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--",
					"sh", "-c",
					fmt.Sprintf("valkey-cli -c -h %s SET %s %s && valkey-cli -c -h %s GET %s",
						clusterFqdn, key, value, clusterFqdn, key))
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				// Wait for the pod to complete
				cmd = exec.Command("kubectl", "wait", "pod/"+testPodName,
					"--for=jsonpath={.status.phase}=Succeeded", "--timeout=30s")
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				// Check the output contains the value we set
				cmd = exec.Command("kubectl", "logs", testPodName)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(value), "GET command should return the set value")

				// Cleanup
				_ = exec.Command("kubectl", "delete", "pod", testPodName, "--wait=false").Run()
			}
			Eventually(verifyGetSetOps).Should(Succeed())

			By("deleting the CR")
			cmd = exec.Command("kubectl", "delete", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("validating that the CR does not exist")
			verifyCrDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeycluster", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyCrDeleted).Should(Succeed())

			By("validating that service does not exist")
			verifyServiceRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyServiceRemoved).Should(Succeed())

			By("validating that configmap does not exist")
			verifyConfigMapRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyConfigMapRemoved).Should(Succeed())

			By("validating that statefulset does not exist")
			verifyStatefulSetRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyStatefulSetRemoved).Should(Succeed())
		})
	})

	Context("when valkey cluster with stateful experience degraded state", func() {
		It("should detect and recover when a statefulset is deleted", func() {
			valkeyClusterName = "valkeycluster-sts-degraded-test"
			manifest := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  workload:
    kind: StatefulSet
`, valkeyClusterName)
			manifestFile := filepath.Join(os.TempDir(), "valkeycluster-sts-degraded.yaml")
			err := os.WriteFile(manifestFile, []byte(manifest), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(manifestFile)

			By("creating the CR")
			cmd := exec.Command("kubectl", "create", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for cluster to be Ready")
			verifyClusterReady := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}
			Eventually(verifyClusterReady).Should(Succeed())

			By("getting a replica statefulset to delete")
			var stsToDelete string
			getStatefulSet := func(g Gomega) {
				// Selecting replica statefulset to delete as we have intermittent issue
				// with master statefulset deletion resulting in cluster not being able to recover
				var err error
				stsToDelete, err = utils.GetReplicaStatefulSet(fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName))
				g.Expect(err).NotTo(HaveOccurred(), "Failed to find a replica statefulset")
				g.Expect(stsToDelete).NotTo(BeEmpty())
			}
			Eventually(getStatefulSet).Should(Succeed())

			By(fmt.Sprintf("deleting statefulset %s to simulate shard loss", stsToDelete))
			cmd = exec.Command("kubectl", "delete", "statefulset", stsToDelete, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete statefulset")

			By("waiting for the operator to recreate the statefulset and recover the cluster")
			verifyRecovery := func(g Gomega) {
				// Verify all statefulsets are present (should be 6 total for 3 shards with 1 replica each)
				cmd := exec.Command("kubectl", "get", "statefulsets",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				statefulsets := utils.GetNonEmptyLines(output)
				g.Expect(statefulsets).To(HaveLen(6), "Expected 6 StatefulSets after operator recreates the deleted one")

				// Verify all pods are ready
				cmd = exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName),
					"-o", "go-template={{ range .items }}{{ range .status.conditions }}"+
						"{{ if and (eq .type \"Ready\") (eq .status \"True\")}}"+
						"{{ $.metadata.name}} {{ \"\\n\" }}"+
						"{{ end }}{{ end }}{{ end }}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podStatuses := utils.GetNonEmptyLines(output)
				g.Expect(podStatuses).To(HaveLen(6), "Expected 6 Pods to be ready after recovery")

				// Then verify the cluster returns to Ready state
				cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
					fmt.Sprintf("Expected cluster to recover to Ready state, but got: %s (reason: %s)", cr.Status.State, cr.Status.Reason))
				g.Expect(cr.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy),
					fmt.Sprintf("Expected ClusterHealthy reason after recovery but got: %s", cr.Status.Reason))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)), "All shards should be ready after recovery")

				// Verify Ready condition is True
				readyCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionReady)
				g.Expect(readyCond).NotTo(BeNil(), "Ready condition should be present")
				g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready condition should be True after recovery")
				g.Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))

				// Verify Degraded condition is no longer present or is False
				degradedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
				if degradedCond != nil {
					g.Expect(degradedCond.Status).To(Equal(metav1.ConditionFalse), "Degraded condition should be False after recovery")
				}
			}
			Eventually(verifyRecovery).Should(Succeed())

			By("cleaning up")
			_ = exec.Command("kubectl", "delete", "-f", manifestFile, "--wait=false").Run()
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeycluster", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}).Should(Succeed())
		})
	})
})
