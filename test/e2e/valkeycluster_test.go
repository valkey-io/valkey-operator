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
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	controller "github.com/valkey-io/valkey-operator/internal/controller"
	"github.com/valkey-io/valkey-operator/test/utils"
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
			valkeyClusterName = "cluster-sample"

			By("creating the CR")
			cmd := exec.Command("kubectl", "delete", "-f", "config/samples/v1alpha1_valkeycluster.yaml", "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "create", "-f", "config/samples/v1alpha1_valkeycluster.yaml")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")
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
				cmd := exec.Command("kubectl", "get", "service", "valkey-"+valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyServiceExists).Should(Succeed())

			By("validating the ConfigMap")
			verifyConfigMapExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", controller.GetServerConfigMapName(valkeyClusterName))
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyConfigMapExists).Should(Succeed())

			By("validating the system user ACLs")
			verifySystemUserAcls := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secret",
					"internal-"+valkeyClusterName+"-system-passwords",
					"-o", "jsonpath={.data}",
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve system user ACLs secret")
				g.Expect(output).To(SatisfyAll(
					ContainSubstring("_operator"),
					ContainSubstring("_exporter"),
					ContainSubstring("_replication"),
				))
			}
			Eventually(verifySystemUserAcls).Should(Succeed())

			By("validating ValkeyNodes")
			verifyValkeyNodesExist := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeynodes",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				nodes := utils.GetNonEmptyLines(output)
				g.Expect(nodes).To(HaveLen(6), "Expected 6 ValkeyNodes")
			}
			Eventually(verifyValkeyNodesExist).Should(Succeed())

			By("validating Pods")
			verifyPodStatuses := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
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

			By("validating server containers have resources configuration")
			cmd = exec.Command("kubectl", "get", "pods",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
				"-o", "jsonpath={.items[0].spec.containers[?(@.name=='server')].resources}",
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to retrieve pod's information")
			Expect(output).To(MatchJSON(`{"limits":{"cpu":"500m","memory":"512Mi"},"requests":{"cpu":"100m","memory":"256Mi"}}`), "Incorrect pod resources configuration")

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

			By("validating each ValkeyNode reports a resolved role, one primary per shard")
			verifyNodeRoles := func(g Gomega) {
				nodes, err := utils.GetValkeyClusterNodes(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(nodes.Items).To(HaveLen(6), "Expected 6 ValkeyNodes")

				primariesPerShard := map[string]int{}
				for _, node := range nodes.Items {
					g.Expect(node.Status.Role).To(BeElementOf("primary", "replica"),
						"ValkeyNode %s should report a resolved role", node.Name)
					if node.Status.Role == "primary" {
						primariesPerShard[node.Labels["valkey.io/shard-index"]]++
					}
				}
				g.Expect(primariesPerShard).To(HaveLen(3), "expected all 3 shards to have a primary")
				for shard, count := range primariesPerShard {
					g.Expect(count).To(Equal(1), "shard %s should report exactly one primary", shard)
				}
			}
			Eventually(verifyNodeRoles).Should(Succeed())

			By("validating live ACL converges with only the unmanaged default user")
			// This cluster sets no custom users, so the only user on the server is
			// Valkey's own `default`, which the aclfile does not manage. The node
			// must still reach ACLApplied=True: aclObservablyInSync has to ignore
			// the unmanaged `default` rather than loop forever waiting for it to
			// match a spec.users entry that does not exist.
			verifyACLApplied := func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "valkeynodes",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
					"-o", "jsonpath={.items[*].metadata.name}"))
				g.Expect(err).NotTo(HaveOccurred())
				names := strings.Fields(out)
				g.Expect(names).To(HaveLen(6))
				for _, name := range names {
					node, err := utils.GetValkeyNodeStatus(name)
					g.Expect(err).NotTo(HaveOccurred())
					cond := utils.FindCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionACLApplied)
					g.Expect(cond).NotTo(BeNil(), "ACLApplied condition should be set on %s", name)
					g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "ACLApplied should converge to True on %s", name)
				}
			}
			// ACLApplied is only set once a node reports Ready, so give it the same
			// budget as cluster startup rather than a single pass.
			Eventually(verifyACLApplied, 5*time.Minute, 5*time.Second).Should(Succeed())

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
				g.Expect(normalEvents["ValkeyNodeCreated"]).To(BeTrue(), "ValkeyNodeCreated event should be emitted")

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
				g.Expect(warningEvents["ValkeyNodeFailed"]).To(BeFalse(), "ValkeyNodeFailed event should not be emitted")
				g.Expect(warningEvents["ClusterMeetFailed"]).To(BeFalse(), "ClusterMeetFailed event should not be emitted")
				g.Expect(warningEvents["SlotAssignmentFailed"]).To(BeFalse(), "SlotAssignmentFailed event should not be emitted")
				g.Expect(warningEvents["NodeForgetFailed"]).To(BeFalse(), "NodeForgetFailed event should not be emitted")

				// Transient errors that may occur during formation but should be resolved
				hasTransientErrors := warningEvents["ReplicaCreationFailed"]
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
				g.Expect(output).To(ContainSubstring("ValkeyNodeCreated"), "ValkeyNodeCreated event should appear in describe")
				g.Expect(output).To(ContainSubstring("InternalSecretsCreated"), "InternalSecretsCreated event should appear in describe")
				// PrimaryCreated, ClusterMeetBatch, ReplicaCreated and ClusterReady may not always
				// appear in describe output due to rate-limiting (see kubernetes/kubernetes#136061).
				// We verify these through cluster status instead of strictly requiring the events.
			}
			Eventually(verifyDescribeEvents).Should(Succeed())

			By("validating client commands")
			verifyClusterAccess := func(g Gomega, expected string, command ...string) {
				// Start a Valkey client pod to access the cluster and execute commands
				clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", valkeyClusterName)

				_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", "client", "--ignore-not-found=true", "--wait=true", "--timeout=30s"))

				// Append the client command to the overall kubectl run command
				cmd := exec.Command("kubectl", append([]string{"run", "client",
					fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--",
					"valkey-cli", "-c", "-h", clusterFqdn}, command...)...)
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
				g.Expect(output).To(ContainSubstring(expected))
			}
			Eventually(verifyClusterAccess).
				WithArguments("cluster_state:ok", "CLUSTER", "INFO").
				Should(Succeed(), "Failed CLUSTER INFO")
			Eventually(verifyClusterAccess).
				WithArguments("52428800", "CONFIG", "GET", "maxmemory").
				Should(Succeed(), "Failed CONFIG GET maxmemory")
			Eventually(verifyClusterAccess).
				WithArguments("_replication", "CONFIG", "GET", "primaryuser").
				Should(Succeed(), "Failed CONFIG GET primaryuser")

			By("validating CLUSTER SLOTS reports the pod IP")
			verifyClusterSlotsIP := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
					"-o", "jsonpath={.items[0].metadata.name}={.items[0].status.podIP}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "failed to list pods")
				parts := strings.SplitN(strings.TrimSpace(out), "=", 2)
				g.Expect(parts).To(HaveLen(2), "expected pod=ip output, got %q", out)
				podName, podIP := parts[0], parts[1]
				g.Expect(podIP).NotTo(BeEmpty(), "pod %q has no podIP", podName)

				cmd = exec.Command("kubectl", "exec", podName, "-c", "server", "--",
					"valkey-cli", "CLUSTER", "SLOTS")
				slotsOut, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "CLUSTER SLOTS failed")
				g.Expect(slotsOut).To(ContainSubstring(podIP),
					"regression: CLUSTER SLOTS did not report podIP %s\noutput:\n%s",
					podIP, slotsOut)
			}
			Eventually(verifyClusterSlotsIP).Should(Succeed())

			// A previous revision deleted the system-password Secret here and
			// asserted a pod roll on the internal-acl-hash annotation. ACL is no
			// longer part of the pod template (it applies live, without a roll),
			// so that annotation and that assertion are gone. The live-apply path
			// is covered by the "live ACL propagation" spec. Recovering an
			// operator locked out by a deleted password Secret is a separate
			// concern that needs a staged recovery through the cluster controller;
			// it is tracked as a follow-up rather than a template roll.
		})

		It("creates a single-shard zero-replica cluster", Label("single-node"), func() {
			const singleNodeClusterName = "cluster-single-node"

			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", singleNodeClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("creating the CR with 1 shard and 0 replicas")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 0
`, singleNodeClusterName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create single-node ValkeyCluster CR")

			By("validating ValkeyNodes")
			verifyValkeyNodesExist := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeynodes",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", singleNodeClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				nodes := utils.GetNonEmptyLines(output)
				g.Expect(nodes).To(HaveLen(1), "Expected 1 ValkeyNode")
			}
			Eventually(verifyValkeyNodesExist).Should(Succeed())

			By("validating the ValkeyCluster CR reaches Ready state")
			verifyCrStatus := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(singleNodeClusterName)
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.Shards).To(Equal(int32(1)))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(1)))

				slotsAssignedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionSlotsAssigned)
				g.Expect(slotsAssignedCond).NotTo(BeNil(), "SlotsAssigned condition not found")
				g.Expect(slotsAssignedCond.Status).To(Equal(metav1.ConditionTrue))
			}
			Eventually(verifyCrStatus, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("validating cluster access")
			verifyClusterAccess := func(g Gomega) {
				clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", singleNodeClusterName)

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

				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_slots_assigned:16384"))
				g.Expect(output).To(ContainSubstring("cluster_size:1"))
			}
			Eventually(verifyClusterAccess).Should(Succeed())

			By("validating CLUSTER SLOTS reports the pod IP")
			verifyClusterSlotsIP := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", singleNodeClusterName),
					"-o", "jsonpath={.items[0].metadata.name}={.items[0].status.podIP}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "failed to list pods")
				parts := strings.SplitN(strings.TrimSpace(out), "=", 2)
				g.Expect(parts).To(HaveLen(2), "expected pod=ip output, got %q", out)
				podName, podIP := parts[0], parts[1]
				g.Expect(podIP).NotTo(BeEmpty(), "pod %q has no podIP", podName)

				cmd = exec.Command("kubectl", "exec", podName, "-c", "server", "--",
					"valkey-cli", "CLUSTER", "SLOTS")
				slotsOut, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "CLUSTER SLOTS failed")
				g.Expect(slotsOut).To(ContainSubstring(podIP),
					"regression: CLUSTER SLOTS did not report podIP %s\noutput:\n%s",
					podIP, slotsOut)
			}
			Eventually(verifyClusterSlotsIP, 1*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("creates a cluster with custom users", func() {
			const withUserClusterName = "cluster-sample-with-users"
			const withUserSampleFile = "config/samples/v1alpha1_valkeycluster-with-user.yaml"

			defer func() {
				cmd := exec.Command("kubectl", "delete", "-f", withUserSampleFile, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("creating the CR with users")
			cmd := exec.Command("kubectl", "delete", "-f", withUserSampleFile, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "apply", "-f", withUserSampleFile)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster with users")

			By("waiting for the cluster to be ready")
			verifyReady := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(withUserClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			}
			Eventually(verifyReady).Should(Succeed())

			By("validating internal secrets were created")
			verifyInternalSecretExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secrets", "internal-"+withUserClusterName+"-acl")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "get", "secrets", "internal-"+withUserClusterName+"-system-passwords")
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyInternalSecretExists).Should(Succeed())

			By("verifying created users")
			verifyCreatedUsers := func(g Gomega) {
				clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", withUserClusterName)

				cmd := exec.Command("kubectl", "get", "secrets", "valkey-cluster-sample-users", "-o", "jsonpath={.data.defaultpw}")
				b64Password, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				b64Password = strings.TrimSpace(b64Password)
				decoded, err := base64.StdEncoding.DecodeString(b64Password)
				g.Expect(err).NotTo(HaveOccurred())
				operatorPassword := string(decoded)

				cmd = exec.Command("kubectl", "run", "client",
					fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--",
					"valkey-cli", "-c", "-h", clusterFqdn, "-a", operatorPassword, "ACL", "LIST")
				_, err = utils.Run(cmd)
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

				// the output of ACL LIST displays users' password(s) as a
				// "#" followed by a 64-character lowercase alphanumeric (from a-f and 0-9) string
				// https://github.com/valkey-io/valkey/blob/unstable/src/acl.c#L219
				passwordHashRegexp := "#[a-f0-9]{64}"

				g.Expect(output).To(SatisfyAll(
					ContainSubstring("user alice on"),
					ContainSubstring("user bob on nopass"),
					// user david is created with 2 passwords
					MatchRegexp("user david on .* %s %s", passwordHashRegexp, passwordHashRegexp),
					// user edward is created with resetpass flag, so its ACL entry should not contain a '#' character
					MatchRegexp("user edward on [^#]+"),
					ContainSubstring("user _exporter on"),
					ContainSubstring("user _operator on"),
				))
			}
			Eventually(verifyCreatedUsers).Should(Succeed())

			By("verifying allowed commands succeed for operator user")
			verifyAllowedPermissionsOfOperatorUser := func(g Gomega) {
				clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", withUserClusterName)

				cmd = exec.Command("kubectl", "get", "secrets", "internal-"+withUserClusterName+"-system-passwords",
					"-o", "jsonpath={.data._operator}",
				)

				b64Password, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				decoded, err := base64.StdEncoding.DecodeString(b64Password)
				g.Expect(err).NotTo(HaveOccurred())
				operatorPassword := string(decoded)

				_ = exec.Command("kubectl", "delete", "pod", "client",
					"--ignore-not-found=true", "--wait=true", "--timeout=30s").Run()

				// Only commands without arguments will be tested
				cmd = exec.Command("kubectl", "run", "client",
					fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never",
					"--", "sh", "-c",
					fmt.Sprintf(
						`valkey-cli -c -h "%s" --user _operator --pass "%s" <<EOF
PING
CLUSTER INFO
CLUSTER MYID
CLUSTER MYSHARDID
CLUSTER NODES
CLUSTER FAILOVER
INFO
CONFIG GET maxmemory
ROLE 
EOF`,
						clusterFqdn,
						operatorPassword,
					),
				)

				_, err = utils.Run(cmd)
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

				g.Expect(output).NotTo(ContainSubstring("NOPERM"))
			}
			Eventually(verifyAllowedPermissionsOfOperatorUser).Should(Succeed())

			By("verifying denied commands fail for operator user")
			verifyDeniedPermissionsOfOperatorUser := func(g Gomega) {
				clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", withUserClusterName)

				cmd = exec.Command("kubectl", "get", "secrets", "internal-"+withUserClusterName+"-system-passwords",
					"-o", "jsonpath={.data._operator}",
				)

				b64Password, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				decoded, err := base64.StdEncoding.DecodeString(b64Password)
				g.Expect(err).NotTo(HaveOccurred())
				operatorPassword := string(decoded)

				disallowedCommands := []string{
					"SET foo bar",
					"GET foo",
					"DEL foo",
					"KEYS *",
					"ACL LIST",
				}

				_ = exec.Command("kubectl", "delete", "pod", "client",
					"--ignore-not-found=true", "--wait=true", "--timeout=30s").Run()

				cmd = exec.Command("kubectl", "run", "client",
					fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never",
					"--", "sh", "-c",
					fmt.Sprintf(
						`valkey-cli -c -h "%s" --user _operator --pass "%s" <<EOF
%s
EOF`,
						clusterFqdn, operatorPassword, strings.Join(disallowedCommands, "\n"),
					))
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "wait", "pod/client",
					"--for=jsonpath={.status.phase}=Succeeded", "--timeout=30s")
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "logs", "client")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				_ = exec.Command("kubectl", "delete", "pod", "client",
					"--ignore-not-found=true", "--wait=true", "--timeout=30s").Run()

				g.Expect(strings.Count(output, "NOPERM")).To(Equal(len(disallowedCommands)),
					"expected all %d disallowed commands to be denied but got: %s",
					len(disallowedCommands), output)
			}
			Eventually(verifyDeniedPermissionsOfOperatorUser).Should(Succeed())

		})

		It("rebalances slots on scale out", func() {
			const baseShards = 2
			const scaleOutShards = 3
			const seedKeys = 500
			valkeyClusterName = "valkeycluster-scaleout"

			By("creating a smaller ValkeyCluster for scale-out")
			scaleOutManifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: %d
  replicas: 1
`, valkeyClusterName, baseShards)
			manifestFile := filepath.Join(os.TempDir(), "valkeycluster-scaleout.yaml")
			err := os.WriteFile(manifestFile, []byte(scaleOutManifest), 0644)
			Expect(err).NotTo(HaveOccurred(), "Failed to write scale-out manifest file")
			defer os.Remove(manifestFile)

			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", valkeyClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()
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
			Eventually(verifyReadyForScaleOut, 10*time.Minute).Should(Succeed())

			By("populating the cluster with data so slot migration snapshots real keys")
			// the _replication user must be allowed to run SELECT and write commands
			// or the slot migration will loop forever.
			seedData := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
					"-o", "jsonpath={.items[0].metadata.name}")
				podName, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(podName)).NotTo(BeEmpty(), "Expected a valkey pod")

				cmd = exec.Command("kubectl", "exec", strings.TrimSpace(podName), "-c", "server", "--",
					"sh", "-c",
					fmt.Sprintf("unset VALKEYCLI_AUTH REDISCLI_AUTH; awk 'BEGIN{for(i=1;i<=%d;i++) print \"SET key:\"i\" val:\"i}' | valkey-cli -c -h 127.0.0.1 | grep -c '^OK$'", seedKeys))
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal(fmt.Sprintf("%d", seedKeys)), "all seed writes should succeed across shards")
			}
			Eventually(seedData).Should(Succeed())

			By(fmt.Sprintf("scaling the cluster to %d shards", scaleOutShards))
			cmd = exec.Command("kubectl", "patch", "valkeycluster", valkeyClusterName,
				"--type=merge", "-p", fmt.Sprintf(`{"spec":{"shards":%d}}`, scaleOutShards))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch ValkeyCluster shards")

			By("verifying all primaries receive slots after scale out")
			verifySlotRebalance := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
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
			Eventually(verifySlotRebalance, 10*time.Minute).Should(Succeed())

			By(fmt.Sprintf("waiting for the cluster to report %d ready shards", scaleOutShards))
			verifyScaledOut := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.Shards).To(Equal(int32(scaleOutShards)))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(scaleOutShards)))
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			}
			Eventually(verifyScaledOut).Should(Succeed())

			By("verifying all seeded keys remain readable after slot migration")
			verifySeededData := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
					"-o", "jsonpath={.items[0].metadata.name}")
				podName, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(podName)).NotTo(BeEmpty(), "Expected a valkey pod")

				cmd = exec.Command("kubectl", "exec", strings.TrimSpace(podName), "-c", "server", "--",
					"sh", "-c",
					fmt.Sprintf("unset VALKEYCLI_AUTH REDISCLI_AUTH; awk 'BEGIN{for(i=1;i<=%d;i++) print \"GET key:\"i}' | valkey-cli -c -h 127.0.0.1 | grep -c '^val:'", seedKeys))
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal(fmt.Sprintf("%d", seedKeys)), "all seeded keys should survive after rebalance")
			}
			Eventually(verifySeededData).Should(Succeed())
		})

		It("drains slots on scale in", func() {
			const initialShards = 3
			const scaleInShards = 2
			valkeyClusterName = "valkeycluster-scalein"

			By("creating a ValkeyCluster with 3 shards")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: %d
  replicas: 1
`, valkeyClusterName, initialShards)
			manifestFile := filepath.Join(os.TempDir(), "valkeycluster-scalein.yaml")
			err := os.WriteFile(manifestFile, []byte(manifest), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(manifestFile)

			cmd := exec.Command("kubectl", "delete", "valkeycluster", valkeyClusterName, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", valkeyClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()
			cmd = exec.Command("kubectl", "apply", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ValkeyCluster CR")

			By("waiting for the cluster to be ready")
			verifyReady := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(initialShards)))
			}
			Eventually(verifyReady, 10*time.Minute, 2*time.Second).Should(Succeed())

			By(fmt.Sprintf("scaling the cluster in to %d shards", scaleInShards))
			cmd = exec.Command("kubectl", "patch", "valkeycluster", valkeyClusterName,
				"--type=merge", "-p", fmt.Sprintf(`{"spec":{"shards":%d}}`, scaleInShards))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch ValkeyCluster shards")

			By("verifying that only 2 primaries own slots after scale in")
			verifySlotDrain := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
					"-o", "jsonpath={.items[0].metadata.name}")
				podName, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(podName)).NotTo(BeEmpty())

				cmd = exec.Command("kubectl", "exec", strings.TrimSpace(podName), "--",
					"valkey-cli", "-c", "-h", "127.0.0.1", "CLUSTER", "NODES")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				primariesWithSlots := 0
				for _, line := range utils.GetNonEmptyLines(output) {
					fields := strings.Fields(line)
					if len(fields) < 8 || !strings.Contains(fields[2], "master") {
						continue
					}
					// Slot ranges appear after the 8 fixed fields (id, addr, flags,
					// master, ping, pong, epoch, state). A master owns slots only
					// when additional fields are present.
					if len(fields) > 8 {
						primariesWithSlots++
					}
				}
				g.Expect(primariesWithSlots).To(Equal(scaleInShards), "Expected only %d primaries to own slots after scale in", scaleInShards)
			}
			Eventually(verifySlotDrain, 10*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying ValkeyNodes for excess shard are deleted")
			verifyValkeyNodes := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeynodes",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				nodes := utils.GetNonEmptyLines(output)
				expectedCount := scaleInShards * (1 + 1) // shards * (1 primary + 1 replica)
				g.Expect(nodes).To(HaveLen(expectedCount),
					"Expected %d ValkeyNodes after scale in, got %d: %v", expectedCount, len(nodes), nodes)
			}
			Eventually(verifyValkeyNodes, 5*time.Minute, 2*time.Second).Should(Succeed())

			By(fmt.Sprintf("waiting for the cluster to report %d ready shards", scaleInShards))
			verifyScaledIn := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(scaleInShards)))
			}
			Eventually(verifyScaledIn, 10*time.Minute, 2*time.Second).Should(Succeed())
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
				cmd := exec.Command("kubectl", "get", "service", "valkey-"+valkeyClusterName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyServiceRemoved).Should(Succeed())

			By("validating that the ConfigMap does not exist")
			verifyConfigMapRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", controller.GetServerConfigMapName(valkeyClusterName))
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyConfigMapRemoved).Should(Succeed())

			By("validating that no ValkeyNode exists")
			verifyValkeyNodesRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeynodes",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName))
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve ValkeyNodes")
				g.Expect(output).To(ContainSubstring("No resources found"))
			}
			Eventually(verifyValkeyNodesRemoved).Should(Succeed())
		})
	})

	Context("when a ValkeyCluster experiences degraded state", func() {
		var degradedClusterName string

		It("should detect and recover when a replica deployment is deleted", func() {
			degradedClusterName = "valkeycluster-degraded-status-test"
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", degradedClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

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
			cmd := exec.Command("kubectl", "delete", "valkeycluster", degradedClusterName, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "create", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")

			By("waiting for cluster to become ready first")
			verifyClusterReady := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(degradedClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}
			Eventually(verifyClusterReady).Should(Succeed())

			By("getting a replica statefulset to delete")
			var statefulsetToDelete string
			getStatefulset := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulsets",
					"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/node-index=1", degradedClusterName),
					"-o", "go-template={{ (index .items 0).metadata.name }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to find a replica statefulset")
				g.Expect(output).NotTo(BeEmpty())
				statefulsetToDelete = output
			}
			Eventually(getStatefulset).Should(Succeed())

			By(fmt.Sprintf("deleting statefulset %s to simulate replica loss", statefulsetToDelete))
			cmd = exec.Command("kubectl", "delete", "statefulset", statefulsetToDelete, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete statefulset")

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
				// First, verify all ValkeyNodes are present (should be 6 total for 3 shards with 1 replica each)
				cmd := exec.Command("kubectl", "get", "valkeynodes",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", degradedClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				nodes := utils.GetNonEmptyLines(output)
				g.Expect(nodes).To(HaveLen(6), "Expected 6 ValkeyNodes after operator recreates the deleted one")

				// Verify all pods are ready
				cmd = exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", degradedClusterName),
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
		})

		// This test was temporarily disabled in PR #54 because the operator
		// could not recover from a primary deletion (issue #43). The failover
		// fix (shardExistsInTopology + findShardPrimary) now handles this: when
		// Valkey promotes the replica, the replacement node-index=0 pod joins
		// as a replica of the promoted primary instead of trying to claim slots.
		//
		// It also covers the shutdown-on-sigterm handoff (#268/#270): the
		// StatefulSet deletion terminates the primary pod gracefully, and the
		// test verifies the replica is promoted within the termination grace
		// period, that writes acknowledged during the disruption survive it,
		// and that keys written before the disruption remain readable.
		It("should detect and recover when a primary deployment is deleted", func() {
			By("creating a ValkeyCluster with a password-protected default user")
			failoverClusterName := "valkeycluster-failover-test"
			failoverClusterManifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %[1]s-users
data:
  defaultpw: %[2]s
---
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %[1]s
spec:
  shards: 3
  replicas: 1
  users:
    - name: default
      enabled: true
      permissions: "+@all ~* &*"
      passwordSecret:
        name: %[1]s-users
        keys: [defaultpw]
`, failoverClusterName, base64.StdEncoding.EncodeToString([]byte(failoverDefaultPassword)))

			manifestFile := filepath.Join(os.TempDir(), "valkeycluster-failover.yaml")
			err := os.WriteFile(manifestFile, []byte(failoverClusterManifest), 0644)
			Expect(err).NotTo(HaveOccurred(), "Failed to write manifest file")
			defer os.Remove(manifestFile)

			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", failoverClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
				cmd = exec.Command("kubectl", "delete", "secret", failoverClusterName+"-users", "--ignore-not-found=true")
				_, _ = utils.Run(cmd)
			}()

			By("applying the CR")
			cmd := exec.Command("kubectl", "delete", "valkeycluster", failoverClusterName, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "create", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")

			By("waiting for cluster to become ready")
			verifyClusterReady := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(failoverClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}
			Eventually(verifyClusterReady).Should(Succeed())

			By("finding a primary (node-index=0) statefulset to delete")
			var primaryStatefulset string
			getPrimaryStatefulset := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulsets",
					"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/node-index=0", failoverClusterName),
					"-o", "go-template={{ (index .items 0).metadata.name }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to find a primary statefulset")
				g.Expect(output).NotTo(BeEmpty())
				primaryStatefulset = output
			}
			Eventually(getPrimaryStatefulset).Should(Succeed())

			By("identifying the shard's primary and replica pods")
			cmd = exec.Command("kubectl", "get", "statefulset", primaryStatefulset,
				"-o", "jsonpath={.metadata.labels.valkey\\.io/shard-index}")
			shardIndex, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get shard index of the primary statefulset")
			var primaryPod, replicaPod string
			Eventually(func(g Gomega) {
				primaryPod, replicaPod = getShardRoles(g, failoverClusterName, shardIndex)
				g.Expect(primaryPod).NotTo(BeEmpty(), "shard has no primary")
				g.Expect(replicaPod).NotTo(BeEmpty(), "shard has no in-sync replica")
			}).WithTimeout(3 * time.Minute).Should(Succeed())

			By("writing keys across the keyspace")
			writeTestKeys(primaryPod)

			By("starting a continuous writer to run through the disruption")
			writer := startContinuousWriter(replicaPod)

			By(fmt.Sprintf("deleting primary statefulset %s to trigger Valkey failover", primaryStatefulset))
			cmd = exec.Command("kubectl", "delete", "statefulset", primaryStatefulset, "--wait=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete primary statefulset")

			// With shutdown-on-sigterm failover the handover must complete
			// inside terminationGracePeriodSeconds (default 30s), so the
			// replica has to report role:master within that window.
			By("asserting the replica is promoted within the termination grace period")
			Eventually(func(g Gomega) {
				output, err := execValkeyPodShell(replicaPod, "valkey-cli INFO replication")
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get replication info from replica")
				g.Expect(output).To(ContainSubstring("role:master"),
					"replica was not promoted to primary")
			}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

			// The graceful handoff is a coordinated failover driven by the
			// terminating primary (CLUSTER FAILOVER FORCE REPLICAID), which
			// logs "Forced failover primary request accepted" on the promoted
			// replica; the crash path goes through FAIL detection instead.
			// valkey 9.0's replica selection on shutdown is best-effort (it
			// requires exact ack-offset equality at the shutdown instant and
			// intermittently falls back to the crash path, ~1 in 3 under
			// write load in local testing), so the path is reported rather
			// than hard-asserted until the promotion is deterministic.
			By("detecting which failover path drove the promotion")
			cmd = exec.Command("kubectl", "logs", replicaPod, "-c", "server")
			logs, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get promoted replica logs")
			handoffEngaged := strings.Contains(strings.ToLower(logs), "forced failover primary request accepted")

			By("asserting every write acknowledged during the disruption is readable")
			acked, maxGap := writer.stop()
			Expect(acked).NotTo(BeEmpty(), "continuous writer recorded no acknowledged writes")
			_, _ = fmt.Fprintf(GinkgoWriter,
				"handoff engaged=%t, %d acknowledged writes, longest writer gap: %.2fs\n",
				handoffEngaged, len(acked), maxGap)
			verifyAcknowledgedWrites(replicaPod, acked)
			if handoffEngaged {
				// The orderly handoff keeps a writer available throughout
				// the disruption; the bound is generous to absorb CI noise.
				Expect(maxGap).To(BeNumerically("<", 10.0),
					fmt.Sprintf("shard had no writer for %.2fs during the graceful handoff", maxGap))
			}

			By("waiting for the operator to recreate the deployment and the cluster to recover")
			verifyClusterRecovery := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeynodes",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", failoverClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				nodes := utils.GetNonEmptyLines(output)
				g.Expect(nodes).To(HaveLen(6), "Expected 6 ValkeyNodes after operator recreates the deleted one")

				cmd = exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", failoverClusterName),
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

			By("asserting the keys written before the disruption are still readable")
			verifySeededKeys(replicaPod)
		})
	})

	Context("when a ValkeyCluster uses Deployment workload type", func() {
		const deploymentClusterName = "cluster-sample-deployment"
		const deploymentSampleFile = "config/samples/v1alpha1_valkeycluster-deployment.yaml"

		AfterEach(func() {
			specReport := CurrentSpecReport()
			if specReport.Failed() {
				utils.CollectDebugInfo(namespace)
			}
		})

		It("creates a functioning cluster backed by Deployments", func() {
			defer func() {
				cmd := exec.Command("kubectl", "delete", "-f", deploymentSampleFile, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("creating the CR")
			cmd := exec.Command("kubectl", "delete", "-f", deploymentSampleFile, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "apply", "-f", deploymentSampleFile)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Deployment-backed ValkeyCluster CR")

			By("validating ValkeyNodes are created")
			verifyValkeyNodesExist := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeynodes",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", deploymentClusterName),
					"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				nodes := utils.GetNonEmptyLines(output)
				g.Expect(nodes).To(HaveLen(6), "Expected 6 ValkeyNodes")
			}
			Eventually(verifyValkeyNodesExist).Should(Succeed())

			By("validating Pods become ready")
			verifyPodStatuses := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", deploymentClusterName),
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

			By("validating the ValkeyCluster CR reaches Ready state")
			verifyCrStatus := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(deploymentClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}
			Eventually(verifyCrStatus, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("validating cluster access")
			verifyClusterAccess := func(g Gomega) {
				clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", deploymentClusterName)

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

				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			}
			Eventually(verifyClusterAccess).Should(Succeed())
		})
	})
})

var _ = Describe("ValkeyCluster spec propagation", func() {
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	Context("workloadType immutability", func() {
		const clusterName = "valkeycluster-immutable-e2e"

		It("rejects a change to workloadType after creation", func() {
			By("creating a ValkeyCluster with StatefulSet workload type")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 0
  workloadType: StatefulSet
`, clusterName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster")
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("attempting to change workloadType to Deployment")
			patchCmd := exec.Command("kubectl", "patch", "valkeycluster", clusterName,
				"--type=merge", "-p", `{"spec":{"workloadType":"Deployment"}}`)
			output, err := utils.Run(patchCmd)
			Expect(err).To(HaveOccurred(), "patch should be rejected")
			Expect(output).To(ContainSubstring("workloadType is immutable"),
				"error should mention that workloadType is immutable")
		})
	})

	Context("persistence mutation rules", func() {
		const addClusterName = "valkeycluster-persistence-add-e2e"
		const shrinkClusterName = "valkeycluster-persistence-shrink-e2e"

		It("rejects adding persistence after creation", func() {
			By("creating a ValkeyCluster without persistence")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 0
`, addClusterName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster")
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", addClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("attempting to add persistence after creation")
			patchCmd := exec.Command("kubectl", "patch", "valkeycluster", addClusterName,
				"--type=merge", "-p", `{"spec":{"persistence":{"size":"1Gi"}}}`)
			output, err := utils.Run(patchCmd)
			Expect(err).To(HaveOccurred(), "patch should be rejected")
			Expect(output).To(ContainSubstring("persistence cannot be added after creation"),
				"error should mention that persistence cannot be added after creation")
		})

		It("rejects shrinking persistence size after creation", func() {
			By("creating a ValkeyCluster with persistence")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 0
  persistence:
    size: 1Gi
`, shrinkClusterName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create persistent ValkeyCluster")
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", shrinkClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("attempting to shrink persistence size")
			patchCmd := exec.Command("kubectl", "patch", "valkeycluster", shrinkClusterName,
				"--type=merge", "-p", `{"spec":{"persistence":{"size":"512Mi"}}}`)
			output, err := utils.Run(patchCmd)
			Expect(err).To(HaveOccurred(), "patch should be rejected")
			Expect(output).To(ContainSubstring("persistence.size may only be expanded"),
				"error should mention that persistence.size may only be expanded")
		})
	})

	Context("topology spread constraints", Label("topology-spread"), func() {
		const clusterName = "valkeycluster-topology-spread-e2e"

		It("spreads pods from the same shard across different nodes", func() {
			By("creating a ValkeyCluster with node.spread.shards set to Required")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  scheduling:
    node:
      spread:
        shard:
          mode: Required
`, clusterName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster with topology spread constraints")
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for the ValkeyCluster to become ready")
			verifyCrStatus := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}
			Eventually(verifyCrStatus, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying each shard is spread across two different nodes")
			verifyShardPlacement := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
					"-o", "go-template={{ range .items }}{{ index .metadata.labels \"valkey.io/shard-index\" }} {{ .spec.nodeName }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				shardNodes := map[string]map[string]struct{}{}
				for _, line := range utils.GetNonEmptyLines(output) {
					fields := strings.Fields(line)
					g.Expect(fields).To(HaveLen(2), "expected shard-index and node-name")
					if _, exists := shardNodes[fields[0]]; !exists {
						shardNodes[fields[0]] = map[string]struct{}{}
					}
					shardNodes[fields[0]][fields[1]] = struct{}{}
				}

				g.Expect(shardNodes).To(HaveLen(3), "expected placement data for 3 shards")
				for shardIndex, nodes := range shardNodes {
					g.Expect(nodes).To(HaveLen(2), "expected shard %s to span two Kubernetes nodes", shardIndex)
				}
			}
			Eventually(verifyShardPlacement).Should(Succeed())
		})

		It("surfaces scheduler failures when strict topology spread cannot be satisfied", func() {
			const (
				unschedulableClusterName = "vkc-tspread-unsched"
				eligibleNodeLabelKey     = "valkey.io/e2e-topology-spread-node"
				eligibleNodeLabelValue   = "true"
			)

			By("labeling one worker node as the only eligible node")
			cmd := exec.Command("kubectl", "get", "nodes",
				"--selector=!node-role.kubernetes.io/control-plane",
				"-o", "jsonpath={.items[0].metadata.name}")
			eligibleNode, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get worker node: %s", eligibleNode))
			Expect(eligibleNode).NotTo(BeEmpty(), "expected at least one worker node")

			cmd = exec.Command("kubectl", "label", "node", eligibleNode,
				fmt.Sprintf("%s=%s", eligibleNodeLabelKey, eligibleNodeLabelValue), "--overwrite=true")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to label worker node: %s", output))
			defer func() {
				cmd := exec.Command("kubectl", "label", "node", eligibleNode, eligibleNodeLabelKey+"-", "--overwrite=true")
				_, _ = utils.Run(cmd)
			}()

			By("creating a ValkeyCluster whose shard requires at least two topology domains")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 1
  scheduling:
    nodeSelector:
      %s: "%s"
    topologySpreadConstraints:
    - maxSkew: 1
      minDomains: 2
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: DoNotSchedule
      labelSelector:
        matchLabels:
          valkey.io/cluster: %s
`, unschedulableClusterName, eligibleNodeLabelKey, eligibleNodeLabelValue, unschedulableClusterName)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to create unschedulable ValkeyCluster: %s", output))
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", unschedulableClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for the ValkeyCluster status to report PodUnschedulable")
			verifyUnschedulableStatus := func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(unschedulableClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateDegraded))
				g.Expect(cr.Status.Reason).To(Equal(valkeyiov1alpha1.ReasonPodUnschedulable))
				g.Expect(cr.Status.Message).To(ContainSubstring("unschedulable"))

				degradedCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
				g.Expect(degradedCond).NotTo(BeNil(), "Degraded condition not found")
				g.Expect(degradedCond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(degradedCond.Reason).To(Equal(valkeyiov1alpha1.ReasonPodUnschedulable))
			}
			Eventually(verifyUnschedulableStatus, 5*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("spreads shard primaries across different nodes", func() {
			const primariesClusterName = "vkc-tspread-primaries"

			By("creating a ValkeyCluster with node.spread.primaries set to Required")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 2
  replicas: 1
  scheduling:
    node:
      spread:
        primaries:
          mode: Required
`, primariesClusterName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster with primaries spread")
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", primariesClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for the ValkeyCluster to become ready")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(primariesClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(2)))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying each shard's node-index-0 pod lands on a distinct node")
			verifyPrimaryPlacement := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/node-index=0", primariesClusterName),
					"-o", "go-template={{ range .items }}{{ .spec.nodeName }}{{ \"\\n\" }}{{ end }}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				nodes := map[string]struct{}{}
				lines := utils.GetNonEmptyLines(output)
				for _, line := range lines {
					node := strings.TrimSpace(line)
					g.Expect(node).NotTo(BeEmpty(), "expected a scheduled node name for each primary")
					nodes[node] = struct{}{}
				}

				g.Expect(lines).To(HaveLen(2), "expected one node-index-0 pod per shard")
				g.Expect(nodes).To(HaveLen(2), "expected the two shard primaries on distinct nodes")
			}
			Eventually(verifyPrimaryPlacement).Should(Succeed())
		})

		It("keeps pods schedulable when a preferred spread cannot be satisfied", func() {
			const (
				preferredClusterName = "vkc-tspread-preferred"
				eligibleNodeLabelKey = "valkey.io/e2e-topology-preferred-node"
			)

			By("labeling one worker node as the only eligible node")
			cmd := exec.Command("kubectl", "get", "nodes",
				"--selector=!node-role.kubernetes.io/control-plane",
				"-o", "jsonpath={.items[0].metadata.name}")
			eligibleNode, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get worker node: %s", eligibleNode))
			Expect(eligibleNode).NotTo(BeEmpty(), "expected at least one worker node")

			cmd = exec.Command("kubectl", "label", "node", eligibleNode,
				fmt.Sprintf("%s=true", eligibleNodeLabelKey), "--overwrite=true")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to label worker node: %s", output))
			defer func() {
				cmd := exec.Command("kubectl", "label", "node", eligibleNode, eligibleNodeLabelKey+"-", "--overwrite=true")
				_, _ = utils.Run(cmd)
			}()

			By("creating a ValkeyCluster pinned to one node with a Preferred pods spread")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 1
  scheduling:
    nodeSelector:
      %s: "true"
    node:
      spread:
        pods:
          mode: Preferred
`, preferredClusterName, eligibleNodeLabelKey)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to create ValkeyCluster with preferred spread: %s", output))
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", preferredClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for the ValkeyCluster to become ready despite only one eligible node")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(preferredClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(1)))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("spreads a shard's pods across availability zones", func() {
			const zoneClusterName = "vkc-tspread-zone"

			By("labeling the two worker nodes with distinct zones")
			cmd := exec.Command("kubectl", "get", "nodes",
				"--selector=!node-role.kubernetes.io/control-plane",
				"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to list worker nodes: %s", out))
			workers := utils.GetNonEmptyLines(out)
			Expect(len(workers)).To(BeNumerically(">=", 2), "expected at least two worker nodes")

			zones := []string{"e2e-az-a", "e2e-az-b"}
			for i := 0; i < 2; i++ {
				w := strings.TrimSpace(workers[i])
				// Preserve any pre-existing zone label so cleanup restores it rather
				// than blindly removing it.
				original, _ := utils.Run(exec.Command("kubectl", "get", "node", w,
					"-o", "jsonpath={.metadata.labels['topology.kubernetes.io/zone']}"))
				original = strings.TrimSpace(original)
				c := exec.Command("kubectl", "label", "node", w,
					fmt.Sprintf("topology.kubernetes.io/zone=%s", zones[i]), "--overwrite=true")
				o, e := utils.Run(c)
				Expect(e).NotTo(HaveOccurred(), fmt.Sprintf("Failed to label node %s: %s", w, o))
				defer func(node, original string) {
					var c *exec.Cmd
					if original != "" {
						c = exec.Command("kubectl", "label", "node", node,
							"topology.kubernetes.io/zone="+original, "--overwrite=true")
					} else {
						c = exec.Command("kubectl", "label", "node", node, "topology.kubernetes.io/zone-")
					}
					_, _ = utils.Run(c)
				}(w, original)
			}

			By("creating a ValkeyCluster with zone.spread.shard set to Required")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 1
  scheduling:
    zone:
      spread:
        shard:
          mode: Required
`, zoneClusterName)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			out, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to create zone-spread ValkeyCluster: %s", out))
			defer func() {
				c := exec.Command("kubectl", "delete", "valkeycluster", zoneClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(c)
			}()

			By("waiting for the ValkeyCluster to become ready")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(zoneClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(1)))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying the shard's two pods land in distinct zones")
			verifyZoneSpread := func(g Gomega) {
				c := exec.Command("kubectl", "get", "nodes",
					"-o", "go-template={{ range .items }}{{ .metadata.name }} {{ index .metadata.labels \"topology.kubernetes.io/zone\" }}{{ \"\\n\" }}{{ end }}")
				o, e := utils.Run(c)
				g.Expect(e).NotTo(HaveOccurred())
				nodeZone := map[string]string{}
				for _, line := range utils.GetNonEmptyLines(o) {
					f := strings.Fields(line)
					if len(f) == 2 {
						nodeZone[f[0]] = f[1]
					}
				}

				c = exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/shard-index=0", zoneClusterName),
					"-o", "go-template={{ range .items }}{{ .spec.nodeName }}{{ \"\\n\" }}{{ end }}")
				o, e = utils.Run(c)
				g.Expect(e).NotTo(HaveOccurred())
				podNodes := utils.GetNonEmptyLines(o)
				g.Expect(podNodes).To(HaveLen(2), "expected two pods for the shard")

				seenZones := map[string]struct{}{}
				for _, n := range podNodes {
					z := nodeZone[strings.TrimSpace(n)]
					g.Expect(z).NotTo(BeEmpty(), "pod's node must carry a zone label")
					seenZones[z] = struct{}{}
				}
				g.Expect(seenZones).To(HaveLen(2), "expected the shard's pods in two distinct zones")
			}
			Eventually(verifyZoneSpread).Should(Succeed())
		})

		It("keeps pods schedulable when a preferred zone spread cannot be satisfied", func() {
			const (
				zonePreferredClusterName = "vkc-tspread-zone-preferred"
				eligibleNodeLabelKey     = "valkey.io/e2e-zone-preferred-node"
			)

			By("labeling one worker node as the only eligible node")
			cmd := exec.Command("kubectl", "get", "nodes",
				"--selector=!node-role.kubernetes.io/control-plane",
				"-o", "jsonpath={.items[0].metadata.name}")
			eligibleNode, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get worker node: %s", eligibleNode))
			Expect(eligibleNode).NotTo(BeEmpty(), "expected at least one worker node")

			cmd = exec.Command("kubectl", "label", "node", eligibleNode,
				fmt.Sprintf("%s=true", eligibleNodeLabelKey), "--overwrite=true")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to label worker node: %s", output))
			defer func() {
				c := exec.Command("kubectl", "label", "node", eligibleNode, eligibleNodeLabelKey+"-", "--overwrite=true")
				_, _ = utils.Run(c)
			}()

			By("creating a ValkeyCluster pinned to one node with a Preferred zone pods spread")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 1
  scheduling:
    nodeSelector:
      %s: "true"
    zone:
      spread:
        pods:
          mode: Preferred
`, zonePreferredClusterName, eligibleNodeLabelKey)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to create ValkeyCluster with preferred zone spread: %s", output))
			defer func() {
				c := exec.Command("kubectl", "delete", "valkeycluster", zonePreferredClusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(c)
			}()

			By("waiting for the ValkeyCluster to become ready despite the unsatisfiable preferred spread")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(zonePreferredClusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(1)))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())
		})
	})

	Context("rolling update", func() {
		const clusterName = "valkeycluster-rolling-e2e"

		It("propagates spec changes one node at a time and returns to Ready", func() {
			By("creating a ValkeyCluster with 2 shards and 1 replica")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 2
  replicas: 1
`, clusterName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster")
			defer func() {
				cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for the cluster to become Ready")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(2)))
			}, 10*time.Minute, 5*time.Second).Should(Succeed())

			By("patching the cluster with new memory requests to trigger a rolling update")
			patchCmd := exec.Command("kubectl", "patch", "valkeycluster", clusterName,
				"--type=merge", "-p",
				`{"spec":{"resources":{"requests":{"cpu":"100m","memory":"384Mi"},"limits":{"cpu":"500m","memory":"512Mi"}}}}`)
			_, err = utils.Run(patchCmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to patch ValkeyCluster resources")

			By("waiting for the cluster to enter the UpdatingNodes progressing state")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				progressingCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
				g.Expect(progressingCond).NotTo(BeNil(), "Progressing condition should be set")
				g.Expect(progressingCond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(progressingCond.Reason).To(Equal(valkeyiov1alpha1.ReasonUpdatingNodes))
			}, 2*time.Minute, time.Second).Should(Succeed())

			By("waiting for all ValkeyNodes to reflect the updated memory request")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "valkeynodes",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
					"-o", "jsonpath={.items[*].spec.resources.requests.memory}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				fields := strings.Fields(output)
				g.Expect(fields).To(HaveLen(4), "expected 4 ValkeyNodes (2 shards × 2 nodes each)")
				for _, mem := range fields {
					g.Expect(mem).To(Equal("384Mi"), "each ValkeyNode should have the updated memory request")
				}
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("waiting for the cluster to return to Ready with Progressing=False")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				progressingCond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionProgressing)
				g.Expect(progressingCond).NotTo(BeNil())
				g.Expect(progressingCond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(progressingCond.Reason).To(Equal(valkeyiov1alpha1.ReasonReconcileComplete))
			}, 10*time.Minute, 5*time.Second).Should(Succeed())
		})
	})

	Context("single-node cluster scale-up", func() {
		const clusterName = "valkeycluster-scaleup-e2e"

		AfterEach(func() {
			cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false")
			_, _ = utils.Run(cmd)
		})

		It("scales from 1 shard 0 replicas to 1 shard 1 replica", func() {
			By("creating a single-node cluster")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 0
`, clusterName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the cluster to become Ready")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			}).Should(Succeed())

			By("scaling to 1 replica")
			cmd = exec.Command("kubectl", "patch", "valkeycluster", clusterName,
				"--type=merge", "-p", `{"spec":{"replicas":1}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the cluster to return to Ready with the replica joined")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))

				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
					"-o", "jsonpath={.items[0].metadata.name}")
				podName, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "exec", strings.TrimSpace(podName), "-c", "server", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_known_nodes:2"))
			}).Should(Succeed())
		})

		It("scales from 1 shard 0 replicas to 2 shards 0 replicas", func() {
			By("creating a single-node cluster")
			manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 0
`, clusterName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the cluster to become Ready")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			}).Should(Succeed())

			By("scaling to 2 shards")
			cmd = exec.Command("kubectl", "patch", "valkeycluster", clusterName,
				"--type=merge", "-p", `{"spec":{"shards":2}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the cluster to return to Ready with 2 shards")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(2)))

				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
					"-o", "jsonpath={.items[0].metadata.name}")
				podName, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "exec", strings.TrimSpace(podName), "-c", "server", "--",
					"valkey-cli", "CLUSTER", "INFO")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("cluster_state:ok"))
				g.Expect(output).To(ContainSubstring("cluster_known_nodes:2"))
				g.Expect(output).To(ContainSubstring("cluster_size:2"))
			}).Should(Succeed())
		})
	})

	Context("live ACL propagation", func() {
		const clusterName = "valkeycluster-live-acl-e2e"
		const usersSecret = "valkey-live-acl-users"
		const aclClientPod = "live-acl-client"
		const (
			defaultPassword = "live-acl-default-pw"
			alicePassword   = "live-acl-alice-pw"
			frankPassword   = "live-acl-frank-pw"
		)
		clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", clusterName)

		// buildManifest renders the users Secret and the ValkeyCluster CR. When
		// withFrank is true it adds a "frank" user (and its password): that is
		// the ACL-only change the test applies once the cluster is up.
		buildManifest := func(withFrank bool) string {
			frankSecret, frankUser := "", ""
			if withFrank {
				frankSecret = "  frankpw: " + frankPassword + "\n"
				frankUser = `    - name: frank
      enabled: true
      passwordSecret:
        name: ` + usersSecret + `
        keys: [frankpw]
      commands:
        allow: ["@read", "@connection"]
      keys:
        readOnly: ["frank:*"]
      permissions: "+ping"
`
			}
			return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
type: Opaque
stringData:
  defaultpw: %s
  alicepw: %s
%s---
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  resources:
    requests:
      cpu: "100m"
      memory: "256Mi"
    limits:
      cpu: "500m"
      memory: "512Mi"
  users:
    - name: default
      enabled: true
      permissions: "+@all ~* &*"
      passwordSecret:
        name: %s
        keys: [defaultpw]
    - name: alice
      enabled: true
      passwordSecret:
        name: %s
        keys: [alicepw]
      commands:
        allow: ["@read", "@write", "@connection"]
      keys:
        readWrite: ["app:*"]
%s`, usersSecret, defaultPassword, alicePassword, frankSecret, clusterName, usersSecret, usersSecret, frankUser)
		}

		applyManifest := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "failed to apply manifest")
		}

		// podIdentities returns one "name=uid" token per server pod, sorted by
		// name. A rolling restart recreates pods with fresh UIDs, so any change
		// to this set is the signal that a roll happened.
		podIdentities := func(g Gomega) []string {
			out, err := utils.Run(exec.Command("kubectl", "get", "pods",
				"-l", "valkey.io/cluster="+clusterName, "--sort-by=.metadata.name",
				"-o", "jsonpath={range .items[*]}{.metadata.name}={.metadata.uid} {end}"))
			g.Expect(err).NotTo(HaveOccurred())
			return strings.Fields(out)
		}

		// valkeyCLI runs a one-shot valkey-cli command from a throwaway client
		// pod and returns its combined output. The command is wrapped so a
		// non-zero exit (e.g. WRONGPASS before an ACL has propagated) still lets
		// the pod complete and its output be read, rather than hanging the wait.
		valkeyCLI := func(g Gomega, cliArgs string) string {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", aclClientPod,
				"--ignore-not-found=true", "--wait=true", "--timeout=30s"))
			cmd := exec.Command("kubectl", "run", aclClientPod,
				"--image="+valkeyClientImage, "--restart=Never", "--",
				"sh", "-c", "valkey-cli "+cliArgs+" 2>&1 || true")
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "wait", "pod/"+aclClientPod,
				"--for=jsonpath={.status.phase}=Succeeded", "--timeout=60s"))
			g.Expect(err).NotTo(HaveOccurred())
			out, err := utils.Run(exec.Command("kubectl", "logs", aclClientPod))
			g.Expect(err).NotTo(HaveOccurred())
			_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", aclClientPod,
				"--ignore-not-found=true", "--wait=false"))
			return out
		}

		aclList := func(g Gomega) string {
			return valkeyCLI(g, fmt.Sprintf("-c -h %s --user default --pass %s --no-auth-warning ACL LIST",
				clusterFqdn, defaultPassword))
		}

		It("applies a user ACL change live without rolling the pods", Label("live-acl"), func() {
			defer func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false"))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", usersSecret, "--ignore-not-found=true", "--wait=false"))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", aclClientPod, "--ignore-not-found=true", "--wait=false"))
			}()

			By("creating a ValkeyCluster with an initial custom user set")
			applyManifest(buildManifest(false))

			By("waiting for the cluster to become Ready")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}, 10*time.Minute, 5*time.Second).Should(Succeed())

			By("recording the server pod identities before the ACL change")
			var beforePods []string
			Eventually(func(g Gomega) {
				beforePods = podIdentities(g)
				// 3 shards x (1 primary + 1 replica)
				g.Expect(beforePods).To(HaveLen(6))
			}).Should(Succeed())

			By("confirming the new user is absent before the change")
			Expect(aclList(Default)).NotTo(ContainSubstring("user frank on"))

			By("adding a user to the cluster's ACL to trigger a live update")
			applyManifest(buildManifest(true))

			By("the new user appears in the running ACL without a pod restart")
			Eventually(func(g Gomega) {
				g.Expect(aclList(g)).To(ContainSubstring("user frank on"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("the new user's credentials authenticate against the live server")
			Eventually(func(g Gomega) {
				out := valkeyCLI(g, fmt.Sprintf("-c -h %s --user frank --pass %s --no-auth-warning PING",
					clusterFqdn, frankPassword))
				g.Expect(out).To(ContainSubstring("PONG"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("every ValkeyNode reports ACLApplied=True")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "valkeynodes",
					"-l", "valkey.io/cluster="+clusterName,
					"-o", "jsonpath={.items[*].metadata.name}"))
				g.Expect(err).NotTo(HaveOccurred())
				names := strings.Fields(out)
				g.Expect(names).To(HaveLen(6))
				for _, name := range names {
					node, err := utils.GetValkeyNodeStatus(name)
					g.Expect(err).NotTo(HaveOccurred())
					cond := utils.FindCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionACLApplied)
					g.Expect(cond).NotTo(BeNil(), "ACLApplied condition should be set on %s", name)
					g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "ACLApplied should be True on %s", name)
				}
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying no server pod was rolled by the ACL change")
			Expect(podIdentities(Default)).To(Equal(beforePods),
				"server pods must not be recreated by a live ACL change")
			Consistently(func(g Gomega) {
				g.Expect(podIdentities(g)).To(Equal(beforePods))
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})
	})

	Context("acl-hash annotation migration", func() {
		const clusterName = "valkeycluster-aclhash-migration-e2e"
		const usersSecret = "valkey-aclhash-users"
		const (
			defaultPassword = "aclhash-default-pw"
			alicePassword   = "aclhash-alice-pw"
		)

		manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
type: Opaque
stringData:
  defaultpw: %s
  alicepw: %s
---
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  resources:
    requests:
      cpu: "100m"
      memory: "256Mi"
    limits:
      cpu: "500m"
      memory: "512Mi"
  users:
    - name: default
      enabled: true
      permissions: "+@all ~* &*"
      passwordSecret:
        name: %s
        keys: [defaultpw]
    - name: alice
      enabled: true
      passwordSecret:
        name: %s
        keys: [alicepw]
      commands:
        allow: ["@read", "@write", "@connection"]
      keys:
        readWrite: ["app:*"]
`, usersSecret, defaultPassword, alicePassword, clusterName, usersSecret, usersSecret)

		serverStatefulSets := func(g Gomega) []string {
			out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
				"-l", "valkey.io/cluster="+clusterName,
				"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}"))
			g.Expect(err).NotTo(HaveOccurred())
			return utils.GetNonEmptyLines(out)
		}

		expectACLAppliedTrue := func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "valkeynodes",
				"-l", "valkey.io/cluster="+clusterName,
				"-o", "jsonpath={.items[*].metadata.name}"))
			g.Expect(err).NotTo(HaveOccurred())
			names := strings.Fields(out)
			g.Expect(names).To(HaveLen(6))
			for _, name := range names {
				node, err := utils.GetValkeyNodeStatus(name)
				g.Expect(err).NotTo(HaveOccurred())
				cond := utils.FindCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionACLApplied)
				g.Expect(cond).NotTo(BeNil(), "ACLApplied condition should be set on %s", name)
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "ACLApplied should be True on %s", name)
			}
		}

		It("removes a legacy internal-acl-hash annotation from the pod template", Label("acl-hash-migration"), func() {
			defer func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false"))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", usersSecret, "--ignore-not-found=true", "--wait=false"))
			}()

			By("creating a ValkeyCluster with a custom user set")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the cluster to become Ready with live ACL applied")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
			}, 10*time.Minute, 5*time.Second).Should(Succeed())
			Eventually(expectACLAppliedTrue, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("stamping the legacy internal-acl-hash annotation on every server StatefulSet")
			// Operator versions before live ACL stamped the ACL hash on the pod
			// template. Reproduce that pre-upgrade state and assert the current
			// operator reconciles it away. Removing the annotation is the one-time
			// roll that, on a real version upgrade, restarts the pod onto the new
			// aclfile and thereby grants _operator the ACL commands. This spec runs
			// against the current operator (which already grants them), so it pins
			// the reconcile trigger and that ACL stays live across the roll, not
			// the permission bootstrap itself, which needs the old operator build.
			var stsNames []string
			Eventually(func(g Gomega) {
				stsNames = serverStatefulSets(g)
				g.Expect(stsNames).To(HaveLen(6))
			}).Should(Succeed())
			for _, sts := range stsNames {
				_, err := utils.Run(exec.Command("kubectl", "patch", "statefulset", sts, "--type", "merge",
					"-p", `{"spec":{"template":{"metadata":{"annotations":{"valkey.io/internal-acl-hash":"simulated-legacy"}}}}}`))
				Expect(err).NotTo(HaveOccurred())
			}

			By("the operator strips the annotation from every StatefulSet (the migration roll)")
			Eventually(func(g Gomega) {
				for _, sts := range serverStatefulSets(g) {
					out, err := utils.Run(exec.Command("kubectl", "get", "statefulset", sts,
						"-o", "jsonpath={.spec.template.metadata.annotations.valkey\\.io/internal-acl-hash}"))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(strings.TrimSpace(out)).To(BeEmpty(),
						"operator must strip the legacy ACL-hash annotation from %s", sts)
				}
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("the cluster returns to Ready and ACL stays live after the migration")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			}, 10*time.Minute, 5*time.Second).Should(Succeed())
			Eventually(expectACLAppliedTrue, 5*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
})

// ---------------------------------------------------------------------------
// Failover test helpers (shutdown-on-sigterm handoff instrumentation, #270).
// ---------------------------------------------------------------------------

// failoverDefaultPassword is the password configured for the default user of
// the failover test cluster (via a passwordSecret, following the pattern
// introduced in #292), so valkey-cli commands run authenticated.
const failoverDefaultPassword = "e2eFailoverPassw0rd"

// failoverKeyCount is the number of keys written across the keyspace before
// the disruption, and verified afterwards to prove no data was lost.
const failoverKeyCount = 50

// writerDurationSeconds is how long the continuous writer samples the shard.
const writerDurationSeconds = 20

// execValkeyPodShell runs a shell script inside the pod's server container
// with VALKEYCLI_AUTH set to the default user's password, so every valkey-cli
// invocation in the script runs authenticated. The operator injects
// VALKEYCLI_AUTH with the _operator user's password for the probe scripts;
// it must be overridden here because valkey-cli auto-sends it as the default
// user's AUTH credential.
func execValkeyPodShell(pod string, script string) (string, error) {
	cmd := exec.Command("kubectl", "exec", pod, "-c", "server", "--",
		"sh", "-c", fmt.Sprintf("export VALKEYCLI_AUTH=%q; ", failoverDefaultPassword)+script)
	return utils.Run(cmd)
}

// getShardRoles returns the pod names of the primary and replica of the given
// shard. Roles are read live from INFO replication on each pod rather than
// from ValkeyNode status, which can lag behind role changes (see #261). The
// replica is only accepted once its replication link is up, so the failover
// is not attempted against a still-syncing replica.
func getShardRoles(g Gomega, clusterName string, shardIndex string) (primaryPod, replicaPod string) {
	cmd := exec.Command("kubectl", "get", "pods",
		"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/shard-index=%s", clusterName, shardIndex),
		"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to list shard pods")

	for _, pod := range utils.GetNonEmptyLines(output) {
		pod = strings.TrimSpace(pod)
		info, err := execValkeyPodShell(pod, "valkey-cli INFO replication")
		g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get replication info from %s", pod))
		switch {
		case strings.Contains(info, "role:master"):
			primaryPod = pod
		case strings.Contains(info, "role:slave") && strings.Contains(info, "master_link_status:up"):
			replicaPod = pod
		}
	}
	return primaryPod, replicaPod
}

// writeTestKeys writes failoverKeyCount keys across the keyspace via the
// given pod.
func writeTestKeys(pod string) {
	GinkgoHelper()

	// All SETs are piped through a single valkey-cli instance; each
	// successful SET prints exactly "OK" on its own line in raw mode.
	script := fmt.Sprintf(
		"ok=$(for i in $(seq 1 %d); do echo \"set e2e:failover:$i v$i\"; done "+
			"| valkey-cli -t 2 -c 2>/dev/null | grep -c '^OK$'); echo written=$ok", failoverKeyCount)
	output, err := execValkeyPodShell(pod, script)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to write keys: %s", output))
	Expect(output).To(ContainSubstring(fmt.Sprintf("written=%d", failoverKeyCount)),
		fmt.Sprintf("Not all keys were written: %s", output))
}

// readKeysBatch reads the given key prefix for indices [from, to] through a
// single valkey-cli instance: the GETs are piped over stdin with an
// "echo KEY:<index>" marker before each one, so one process serves the whole
// batch and the output can be correlated per key regardless of extra lines.
func readKeysBatch(pod, keyPrefix string, from, to int) (map[string]string, error) {
	script := fmt.Sprintf(
		"for i in $(seq %d %d); do echo \"echo KEY:$i\"; echo \"get %s$i\"; done | valkey-cli -t 2 -c 2>/dev/null",
		from, to, keyPrefix)
	output, err := execValkeyPodShell(pod, script)
	if err != nil {
		return nil, fmt.Errorf("reading keys back: %w (output: %s)", err, output)
	}

	values := map[string]string{}
	current := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "KEY:"); ok {
			current = after
			continue
		}
		// The last non-empty line before the next marker is the value;
		// a missing key prints an empty line and leaves no entry.
		if current != "" && line != "" {
			values[current] = line
		}
	}
	return values, nil
}

// verifySeededKeys asserts every key written by writeTestKeys is still
// readable with the expected value.
func verifySeededKeys(pod string) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		values, err := readKeysBatch(pod, "e2e:failover:", 1, failoverKeyCount)
		g.Expect(err).NotTo(HaveOccurred())
		for i := 1; i <= failoverKeyCount; i++ {
			idx := fmt.Sprintf("%d", i)
			g.Expect(values[idx]).To(Equal("v"+idx),
				fmt.Sprintf("seeded key e2e:failover:%s was lost across the disruption", idx))
		}
	}).Should(Succeed())
}

// continuousWriter tracks a background write loop running inside a pod.
type continuousWriter struct {
	cmd    *exec.Cmd
	output *strings.Builder
}

// startContinuousWriter starts a background loop inside the pod that writes
// uniquely-numbered keys through the cluster for writerDurationSeconds,
// recording a timestamped ack/fail line per attempt. It is started against
// the shard's replica so cluster-mode redirects follow the primary across the
// handoff.
func startContinuousWriter(pod string) *continuousWriter {
	GinkgoHelper()

	// POSIX-sh only: the image's /bin/sh is dash, which has no $SECONDS.
	// The final "end" sentinel line closes the observation window so a
	// write outage lasting until the end of the loop still counts as a gap.
	// The -t 2 connection timeout keeps the writer sampling when a stale
	// MOVED redirect points at the terminated primary's unroutable IP;
	// without it a single connect can hang for the ~130s TCP SYN timeout.
	script := fmt.Sprintf(
		"end=$(($(date +%%s)+%d)); i=0; "+
			"while [ \"$(date +%%s)\" -lt \"$end\" ]; do "+
			"r=$(valkey-cli -t 2 -c set e2e:cw:$i v$i 2>/dev/null | tail -n 1); "+
			"if [ \"$r\" = \"OK\" ]; then echo \"ack $i $(date +%%s.%%N)\"; "+
			"else echo \"fail $i $(date +%%s.%%N)\"; fi; "+
			"i=$((i+1)); "+
			"done; echo \"end - $(date +%%s.%%N)\"", writerDurationSeconds)
	cmd := exec.Command("kubectl", "exec", pod, "-c", "server", "--",
		"sh", "-c", fmt.Sprintf("export VALKEYCLI_AUTH=%q; ", failoverDefaultPassword)+script)

	w := &continuousWriter{cmd: cmd, output: &strings.Builder{}}
	cmd.Stdout = w.output
	cmd.Stderr = w.output
	Expect(cmd.Start()).To(Succeed(), "Failed to start continuous writer")
	return w
}

// stop waits for the writer loop to finish and returns the map of
// acknowledged key indices to their expected values, plus the longest gap in
// seconds between consecutive acknowledged writes (the shard's effective
// write-unavailability window).
func (w *continuousWriter) stop() (acked map[string]string, maxGap float64) {
	GinkgoHelper()

	Expect(w.cmd.Wait()).To(Succeed(), "continuous writer failed: %s", w.output.String())

	acked = map[string]string{}
	lastAckTime := -1.0
	for _, line := range utils.GetNonEmptyLines(w.output.String()) {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		status, idx := fields[0], fields[1]
		var ts float64
		if _, err := fmt.Sscanf(fields[2], "%f", &ts); err != nil {
			continue
		}
		// The "end" sentinel closes the window: a write outage running
		// through the end of the loop counts as a gap instead of being
		// silently dropped.
		if status != "ack" && status != "end" {
			continue
		}
		if lastAckTime >= 0 && ts-lastAckTime > maxGap {
			maxGap = ts - lastAckTime
		}
		if status == "end" {
			break
		}
		lastAckTime = ts
		acked[idx] = "v" + idx
	}
	return acked, maxGap
}

// verifyAcknowledgedWrites asserts that every key the continuous writer got
// an OK for is readable with the expected value — i.e. no acknowledged write
// was dropped during the handoff.
func verifyAcknowledgedWrites(pod string, acked map[string]string) {
	GinkgoHelper()

	maxIdx := 0
	for idx := range acked {
		var i int
		_, err := fmt.Sscanf(idx, "%d", &i)
		Expect(err).NotTo(HaveOccurred())
		if i > maxIdx {
			maxIdx = i
		}
	}

	readable, err := readKeysBatch(pod, "e2e:cw:", 0, maxIdx)
	Expect(err).NotTo(HaveOccurred(), "Failed to read back acknowledged writes")

	var lost []string
	for idx, want := range acked {
		if readable[idx] != want {
			lost = append(lost, idx)
		}
	}
	Expect(lost).To(BeEmpty(),
		fmt.Sprintf("%d acknowledged write(s) were lost across the handoff: %v", len(lost), lost))
}
