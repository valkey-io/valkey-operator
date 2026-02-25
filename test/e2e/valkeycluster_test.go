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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster", Ordered, func() {
	var valkeyClusterName string

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	Context("when a ValkeyCluster CR is applied", func() {
		It("creates a Valkey Cluster deployment", func() {
			By("creating the CR")
			cmd := exec.Command("kubectl", "create", "-f", "config/samples/v1alpha1_valkeycluster.yaml")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")

			valkeyClusterName = "valkeycluster-sample"
			By("validating the CR")
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

			By("validating Deployments")
			verifyDeploymentsExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployments",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				deployments := utils.GetNonEmptyLines(output)
				g.Expect(deployments).To(HaveLen(6), "Expected 6 Deployments")
			}
			Eventually(verifyDeploymentsExists).Should(Succeed())

			By("validating Pods")
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

			By("validating valkey-server containers have resources configuration")
			cmd = exec.Command("kubectl", "get", "pods",
				"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName),
				"-o", "jsonpath={.items[0].spec.containers[?(@.name=='valkey-server')].resources}",
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to retrieve pod's information")
			Expect(output).To(MatchJSON(`{"limits":{"cpu":"500m","memory":"512Mi"},"requests":{"cpu":"100m","memory":"256Mi"}}`), "Incorrect pod resources configuration")

			By("validating internal secret was created")
			verifyInternalSecretsExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secrets", "internal-" + valkeyClusterName + "-acl")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyInternalSecretsExists).Should(Succeed())

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

			// NOTE: Kubernetes Events are best-effort and may be rate-limited, delayed by
			// `kubectl get events` / `kubectl describe` when many events are emitted for the same Custom Resource.
			// In particular, kubectl output can appear capped (~15–20) and events can show up late; see:
			// https://github.com/kubernetes/kubernetes/issues/136061
			// This test therefore asserts a minimal set of "must-have" events and uses cluster status as the
			// source of truth for readiness/replicas when optional events are missing.
			By("verifying key events were emitted (best-effort)")
			verifyAllEvents := func(g Gomega) {
				normalEvents, warningEvents, err := utils.GetEvents(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())

				// Infrastructure Events (Normal)
				g.Expect(normalEvents["ServiceCreated"]).To(BeTrue(), "ServiceCreated event should be emitted")
				g.Expect(normalEvents["ConfigMapCreated"]).To(BeTrue(), "ConfigMapCreated event should be emitted")
				g.Expect(normalEvents["DeploymentCreated"]).To(BeTrue(), "DeploymentCreated event should be emitted")

				// ReplicaCreated should be emitted for clusters with replicas > 0
				// Note: This event may not always be captured due to rate-limiting issues
				if !normalEvents["ReplicaCreated"] {
					// Verify cluster actually has replicas even if event wasn't captured
					cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
					g.Expect(err).NotTo(HaveOccurred())
					// The cluster should have 3 shards with 1 replica each (6 total pods)
					// If cluster is ready with correct shard count, replicas were created successfully
					g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)), "Cluster should have 3 ready shards with replicas (ReplicaCreated event may not have been captured)")
				}

				// Status Events (Normal) - May or may not be present depending on timing
				// WaitingForShards and WaitingForReplicas are emitted during reconciliation
				// but may not always be captured depending on how fast the cluster forms
				if normalEvents["WaitingForShards"] {
					// If present, verify it was emitted correctly
					g.Expect(normalEvents["WaitingForShards"]).To(BeTrue(), "WaitingForShards event was emitted")
				}
				if normalEvents["WaitingForReplicas"] {
					g.Expect(normalEvents["WaitingForReplicas"]).To(BeTrue(), "WaitingForReplicas event was emitted")
				}

				// ClusterReady event should be emitted when cluster becomes healthy
				// Note: This may be rate-limited by Kubernetes
				// We'll check for it but won't fail if it's missing due to rate-limiting and may be delayed
				if !normalEvents["ClusterReady"] {
					cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
					// Verify cluster is actually ready even if event was rate-limited
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady), "Cluster should be in Ready state (ClusterReady event may be rate-limited)")
				}

				// Critical infrastructure failures that should NEVER occur
				g.Expect(warningEvents["ServiceUpdateFailed"]).To(BeFalse(), "ServiceUpdateFailed event should not be emitted")
				g.Expect(warningEvents["ConfigMapUpdateFailed"]).To(BeFalse(), "ConfigMapUpdateFailed event should not be emitted")
				g.Expect(warningEvents["ConfigMapCreationFailed"]).To(BeFalse(), "ConfigMapCreationFailed event should not be emitted")
				g.Expect(warningEvents["DeploymentCreationFailed"]).To(BeFalse(), "DeploymentCreationFailed event should not be emitted")
				g.Expect(warningEvents["ClusterMeetFailed"]).To(BeFalse(), "ClusterMeetFailed event should not be emitted")
				g.Expect(warningEvents["SlotAssignmentFailed"]).To(BeFalse(), "SlotAssignmentFailed event should not be emitted")
				g.Expect(warningEvents["NodeForgetFailed"]).To(BeFalse(), "NodeForgetFailed event should not be emitted")

				// Transient errors that may occur during formation but should be resolved
				hasTransientErrors := warningEvents["NodeAddFailed"] || warningEvents["ReplicaCreationFailed"] || warningEvents["PrimaryLost"]
				if hasTransientErrors {
					// Verify cluster recovered and reached healthy state despite transient errors
					cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady), "Cluster should recover from transient errors and reach Ready state")
					g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)), "All shards should be ready despite transient errors during formation")
				}

				// StaleNodeForgotten is a Normal event that should not occur during initial cluster creation
				g.Expect(normalEvents["StaleNodeForgotten"]).To(BeFalse(), "StaleNodeForgotten event should not be emitted during initial creation")
			}
			Eventually(verifyAllEvents).Should(Succeed())

			By("verifying events are visible in kubectl describe")
			verifyDescribeEvents := func(g Gomega) {
				cmd := exec.Command("kubectl", "describe", "valkeycluster", valkeyClusterName)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Events:"), "Events section should be present in describe output")

				// Verify key events appear in describe output
				g.Expect(output).To(ContainSubstring("ServiceCreated"), "ServiceCreated event should appear in describe")
				g.Expect(output).To(ContainSubstring("ConfigMapCreated"), "ConfigMapCreated event should appear in describe")
				g.Expect(output).To(ContainSubstring("DeploymentCreated"), "DeploymentCreated event should appear in describe")
				// TODO PrimaryCreated, ClusterMeet events are not always captured due to rate-limiting issues
				// fix this removing events which are not important
				// ReplicaCreated and ClusterReady may not always appear in describe output due to:
				// - Rate limiting as described above
				// We verify these through cluster status instead of strictly requiring the events
			}
			Eventually(verifyDescribeEvents).Should(Succeed())

			By("validating cluster access")
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

			By("verifying created users")
			verifyCreatedUsers := func(g Gomega) {
				// Start a Valkey client pod to access the cluster and get its status.
				clusterFqdn := fmt.Sprintf("%s.default.svc.cluster.local", valkeyClusterName)

				cmd := exec.Command("kubectl", "run", "client",
					fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--",
					"valkey-cli", "-c", "-h", clusterFqdn, "ACL", "LIST")
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

				// There should be 3 defined users
				g.Expect(output).To(SatisfyAll(
					ContainSubstring("user alice on"),
					ContainSubstring("user bob on nopass"),
					ContainSubstring("user david on"),
				))
			}
			Eventually(verifyCreatedUsers).Should(Succeed())
		})

		It("rebalances slots on scale out", func() {
			const baseShards = 2
			const scaleOutShards = 3
			valkeyClusterName = "valkeycluster-scaleout"

			By("creating a smaller ValkeyCluster for scale-out")
			scaleOutManifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: %d
  replicas: 1
  resources:
    requests:
      memory: "256Mi"
      cpu: "100m"
    limits:
      memory: "512Mi"
      cpu: "500m"
`, valkeyClusterName, baseShards)
			manifestFile := filepath.Join(os.TempDir(), "valkeycluster-scaleout.yaml")
			err := os.WriteFile(manifestFile, []byte(scaleOutManifest), 0644)
			Expect(err).NotTo(HaveOccurred(), "Failed to write scale-out manifest file")
			defer os.Remove(manifestFile)

			cmd := exec.Command("kubectl", "delete", "valkeycluster", valkeyClusterName, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "apply", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply scale-out ValkeyCluster CR")

			By("waiting for the cluster to be ready before scaling")
			verifyReadyForScaleOut := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(baseShards)))
			}
			Eventually(verifyReadyForScaleOut, 10*time.Minute, 2*time.Second).Should(Succeed())

			By(fmt.Sprintf("scaling the cluster to %d shards", scaleOutShards))
			cmd = exec.Command("kubectl", "patch", "valkeycluster", valkeyClusterName,
				"--type=merge", "-p", fmt.Sprintf(`{"spec":{"shards":%d}}`, scaleOutShards))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch ValkeyCluster shards")

			By("verifying all primaries receive slots after scale out")
			verifySlotRebalance := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName),
					"-o", "jsonpath={.items[0].metadata.name}")
				podName, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(podName)).NotTo(BeEmpty(), "Expected a valkey pod")

				cmd = exec.Command("kubectl", "exec", strings.TrimSpace(podName), "--",
					"valkey-cli", "-c", "-h", "127.0.0.1", "CLUSTER", "NODES")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				lines := utils.GetNonEmptyLines(output)
				masterWithSlots := 0
				for _, line := range lines {
					fields := strings.Fields(line)
					if len(fields) < 9 {
						continue
					}
					if !strings.Contains(fields[2], "master") {
						continue
					}
					masterWithSlots++
				}
				g.Expect(masterWithSlots).To(Equal(scaleOutShards), "Expected all primaries to own slots after rebalance")
			}
			Eventually(verifySlotRebalance, 10*time.Minute, 2*time.Second).Should(Succeed())

			By(fmt.Sprintf("waiting for the cluster to report %d ready shards", scaleOutShards))
			verifyScaledOut := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.Shards).To(Equal(int32(scaleOutShards)))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(scaleOutShards)))
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			}
			Eventually(verifyScaledOut, 10*time.Minute, 2*time.Second).Should(Succeed())

			By("cleaning up the scale-out cluster")
			cmd = exec.Command("kubectl", "delete", "valkeycluster", valkeyClusterName, "--wait=false")
			_, _ = utils.Run(cmd)
		})
	})

	Context("when a ValkeyCluster CR is deleted", func() {
		It("deletes the Valkey Cluster deployment", func() {
			By("deleting the CR")
			cmd := exec.Command("kubectl", "delete", "-f", "config/samples/v1alpha1_valkeycluster.yaml")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete ValkeyCluster CR")

			By("validating that the CR does not exist")
			verifyCrRemoved := func(g Gomega) {
				// Get the name of the ValkeyCluster CR
				cmd := exec.Command("kubectl", "get", "ValkeyCluster", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyCrRemoved).Should(Succeed())

			By("validating that the Service does not exist")
			verifyServiceRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyServiceRemoved).Should(Succeed())

			By("validating that the ConfigMap does not exist")
			verifyConfigMapRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyConfigMapRemoved).Should(Succeed())

			By("validating that no Deployment exist")
			verifyDeploymentsRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployments",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", valkeyClusterName))
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve Deployments")
				g.Expect(output).To(ContainSubstring("No resources found"))
			}
			Eventually(verifyDeploymentsRemoved).Should(Succeed())
		})
	})

	Context("when a ValkeyCluster experiences degraded state", func() {
		var degradedClusterName string

		It("should detect and recover when a replica deployment is deleted", func() {
			By("creating a ValkeyCluster")
			degradedClusterManifest := `apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkeycluster-degraded-status-test
spec:
  shards: 3
  replicas: 1
`

			manifestFile := filepath.Join(os.TempDir(), "valkeycluster-degraded.yaml")
			err := os.WriteFile(manifestFile, []byte(degradedClusterManifest), 0644)
			Expect(err).NotTo(HaveOccurred(), "Failed to write manifest file")
			defer os.Remove(manifestFile)

			By("applying the CR")
			cmd := exec.Command("kubectl", "create", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")
			degradedClusterName = "valkeycluster-degraded-status-test"

			By("waiting for cluster to become ready first")
			verifyClusterReady := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(degradedClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}
			Eventually(verifyClusterReady).Should(Succeed())

			By("getting a replica deployment to delete")
			var deploymentToDelete string
			getDeployment := func(g Gomega) {
				var err error
				deploymentToDelete, err = utils.GetReplicaDeployment(fmt.Sprintf("app.kubernetes.io/instance=%s", degradedClusterName))
				g.Expect(err).NotTo(HaveOccurred(), "Failed to find a replica deployment")
				g.Expect(deploymentToDelete).NotTo(BeEmpty())
			}
			Eventually(getDeployment).Should(Succeed())

			By(fmt.Sprintf("deleting deployment %s to simulate replica loss", deploymentToDelete))
			cmd = exec.Command("kubectl", "delete", "deployment", deploymentToDelete, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete deployment")

			By("waiting for the cluster to detect the deployment loss and start recovery")
			verifyDegradedState := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(degradedClusterName)
				g.Expect(err).NotTo(HaveOccurred())

				// The Cluster should detect the deployment loss and not be in Ready state.
				// The operator immediately recreates missing deployments, so the cluster
				// transitions through Reconciling/AddingNodes states, and may briefly enter
				// Degraded state (with NodeAddFailed reason) if adding the node fails temporarily.
				g.Expect(cr.Status.State).To(Or(Equal(valkeyiov1alpha1.ClusterStateReconciling), Equal(valkeyiov1alpha1.ClusterStateDegraded)),
					fmt.Sprintf("Expected cluster to be reconciling or degraded after deployment deletion, but got: %s (reason: %s)", cr.Status.State, cr.Status.Reason))

				readyCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionReady)
				if readyCond != nil {
					g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse), "Ready condition should be False when deployment is being recreated")
				}
			}
			Eventually(verifyDegradedState).Should(Succeed())
			By("waiting for the operator to recreate the deployment and recover the cluster")
			verifyClusterRecovery := func(g Gomega) {
				// First, verify all deployments are present (should be 6 total for 3 shards with 1 replica each)
				cmd := exec.Command("kubectl", "get", "deployments",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", degradedClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				deployments := utils.GetNonEmptyLines(output)
				g.Expect(deployments).To(HaveLen(6), "Expected 6 Deployments after operator recreates the deleted one")

				// Verify all pods are ready
				cmd = exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", degradedClusterName),
					"-o", "go-template={{ range .items }}{{ range .status.conditions }}"+
						"{{ if and (eq .type \"Ready\") (eq .status \"True\")}}"+
						"{{ $.metadata.name}} {{ \"\\n\" }}"+
						"{{ end }}{{ end }}{{ end }}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podStatuses := utils.GetNonEmptyLines(output)
				g.Expect(podStatuses).To(HaveLen(6), "Expected 6 Pods to be ready after recovery")

				// Then verify the cluster returns to Ready state
				cr, err := utils.GetValkeyClusterStatus(degradedClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
					fmt.Sprintf("Expected cluster to recover to Ready state, but got: %s (reason: %s)", cr.Status.State, cr.Status.Reason))
				g.Expect(cr.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy),
					fmt.Sprintf("Expected ClusterHealthy reason after recovery but got: %s", cr.Status.Reason))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)), "All shards should be ready after recovery")

				readyCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionReady)
				g.Expect(readyCond).NotTo(BeNil(), "Ready condition should be present")
				g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready condition should be True after recovery")
				g.Expect(readyCond.Reason).To(Equal(valkeyiov1alpha1.ReasonClusterHealthy))

				degradedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
				if degradedCond != nil {
					g.Expect(degradedCond.Status).To(Equal(metav1.ConditionFalse), "Degraded condition should be False after recovery")
				}
			}
			Eventually(verifyClusterRecovery).Should(Succeed())

			By("cleaning up the degraded cluster")
			cmd = exec.Command("kubectl", "delete", "valkeycluster", degradedClusterName, "--wait=false")
			_, _ = utils.Run(cmd)

			By("waiting for cluster to be deleted")
			verifyClusterDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeycluster", degradedClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Cluster should be deleted")
			}
			Eventually(verifyClusterDeleted).Should(Succeed())
		})

		// This test was temporarily disabled in PR #54 because the operator
		// could not recover from a primary deletion (issue #43). The failover
		// fix (shardExistsInTopology + findShardPrimary) now handles this: when
		// Valkey promotes the replica, the replacement node-index=0 pod joins
		// as a replica of the promoted primary instead of trying to claim slots.
		It("should detect and recover when a primary deployment is deleted", func() {
			By("creating a ValkeyCluster")
			failoverClusterManifest := `apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkeycluster-failover-test
spec:
  shards: 3
  replicas: 1
`

			manifestFile := filepath.Join(os.TempDir(), "valkeycluster-failover.yaml")
			err := os.WriteFile(manifestFile, []byte(failoverClusterManifest), 0644)
			Expect(err).NotTo(HaveOccurred(), "Failed to write manifest file")
			defer os.Remove(manifestFile)

			By("applying the CR")
			cmd := exec.Command("kubectl", "create", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")
			failoverClusterName := "valkeycluster-failover-test"
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", failoverClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for cluster to become ready")
			verifyClusterReady := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(failoverClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}
			Eventually(verifyClusterReady).Should(Succeed())

			By("finding a primary (node-index=0) deployment to delete")
			var primaryDeployment string
			getPrimaryDeployment := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployments",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,valkey.io/node-index=0", failoverClusterName),
					"-o", "go-template={{ (index .items 0).metadata.name }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to find a primary deployment")
				g.Expect(output).NotTo(BeEmpty())
				primaryDeployment = output
			}
			Eventually(getPrimaryDeployment).Should(Succeed())

			By(fmt.Sprintf("deleting primary deployment %s to trigger Valkey failover", primaryDeployment))
			cmd = exec.Command("kubectl", "delete", "deployment", primaryDeployment, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete primary deployment")

			By("waiting for the operator to recreate the deployment and the cluster to recover")
			verifyClusterRecovery := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployments",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", failoverClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				deployments := utils.GetNonEmptyLines(output)
				g.Expect(deployments).To(HaveLen(6), "Expected 6 Deployments after operator recreates the deleted one")

				cmd = exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", failoverClusterName),
					"-o", "go-template={{ range .items }}{{ range .status.conditions }}"+
						"{{ if and (eq .type \"Ready\") (eq .status \"True\")}}"+
						"{{ $.metadata.name}} {{ \"\\n\" }}"+
						"{{ end }}{{ end }}{{ end }}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podStatuses := utils.GetNonEmptyLines(output)
				g.Expect(podStatuses).To(HaveLen(6), "Expected 6 Pods to be ready after failover recovery")

				cr, err := utils.GetValkeyClusterStatus(failoverClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
					fmt.Sprintf("Expected cluster to recover to Ready after failover, but got: %s (reason: %s)", cr.Status.State, cr.Status.Reason))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)), "All shards should be ready after failover recovery")

				readyCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionReady)
				g.Expect(readyCond).NotTo(BeNil(), "Ready condition should be present")
				g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready condition should be True after failover recovery")

				degradedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
				if degradedCond != nil {
					g.Expect(degradedCond.Status).To(Equal(metav1.ConditionFalse), "Degraded condition should be False after failover recovery")
				}
			}
			Eventually(verifyClusterRecovery).Should(Succeed())

			By("cleaning up the failover cluster")
			cmd = exec.Command("kubectl", "delete", "valkeycluster", failoverClusterName, "--wait=false")
			_, _ = utils.Run(cmd)

			By("waiting for cluster to be deleted")
			verifyClusterDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeycluster", failoverClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Cluster should be deleted")
			}
			Eventually(verifyClusterDeleted).Should(Succeed())
		})
	})
})
