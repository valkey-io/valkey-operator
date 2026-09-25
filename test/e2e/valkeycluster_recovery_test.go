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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

// Recovery from cluster states the operator has to repair on its own, without a
// spec change or manual intervention.
var _ = Describe("ValkeyCluster recovery", Label("valkeycluster", "recovery"), func() {
	// The clusters declare no users, so valkey-cli runs as the default user with
	// no password. The connect timeout bounds a stale MOVED redirect pointing at
	// a terminated pod.
	cliOpts := utils.ValkeyCLIOptions{ConnectTimeoutSeconds: 2}

	// podFor returns the pod name for a (shard, node) position.
	podFor := func(cluster string, shard, node int) string {
		return fmt.Sprintf("valkey-%s-%d-%d-0", cluster, shard, node)
	}

	// roleOf reads the ValkeyNode's reported role. Status.Role is maintained by
	// the RolePoller, so callers poll rather than read once.
	roleOf := func(cluster string, shard, node int) (string, error) {
		GinkgoHelper()
		n, err := utils.GetValkeyNodeStatus(fmt.Sprintf("%s-%d-%d", cluster, shard, node))
		if err != nil {
			return "", err
		}
		return n.Status.Role, nil
	}

	applyCluster := func(manifest string) {
		GinkgoHelper()
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	}

	waitForReady := func(cluster string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(cluster)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
				"cluster state: %s, reason: %s", cr.Status.State, cr.Status.Reason)
		}).Should(Succeed())
	}

	deleteCluster := func(cluster string) {
		cmd := exec.Command("kubectl", "delete", "valkeycluster", cluster, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
	}

	// collectOnFailure gathers operator logs and the events of the namespace
	// holding the cluster's pods.
	collectOnFailure := func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo("default")
			utils.CollectDebugInfo(namespace)
		}
	}

	// Scaling replicas down while a shard's primary sits above the new node-index
	// bound puts the primary on a node the desired topology no longer includes, so
	// completing the scale-down means removing the node currently serving the
	// shard.
	Context("when the primary sits on a node the new replica count removes", Label("replica-scale-down"), func() {
		const clusterName = "valkeycluster-replica-scaledown-e2e"
		const keyPrefix = "e2e:scaledown"
		const seedKeys = 500

		AfterEach(func() {
			collectOnFailure()
			deleteCluster(clusterName)
		})

		It("completes the scale-down and keeps the data", func() {
			By("creating a 1-shard cluster with 2 replicas, so node-index 2 exists")
			applyCluster(fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 2
`, clusterName))

			By("waiting for the cluster to become Ready")
			waitForReady(clusterName)

			By("seeding keys so the scale-down can be checked for data loss")
			Eventually(func(g Gomega) {
				g.Expect(utils.WriteValkeyKeys(podFor(clusterName, 0, 0), keyPrefix, seedKeys, cliOpts)).To(Succeed())
			}).Should(Succeed())

			By("failing over to node-index 2, moving the primary off index 0")
			// A graceful CLUSTER FAILOVER holds writes until the target has caught
			// up, so no keys are lost here.
			_, err := utils.ValkeyCLI(podFor(clusterName, 0, 2), cliOpts, "CLUSTER", "FAILOVER")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for node-index 2 to report primary")
			Eventually(func(g Gomega) {
				role, err := roleOf(clusterName, 0, 2)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(role).To(Equal("primary"))
			}).Should(Succeed())

			By("scaling replicas from 2 to 1, which puts the primary out of range")
			// nodesPerShard becomes 2, so the primary at node-index 2 is now on a
			// node the desired topology does not include.
			cmd := exec.Command("kubectl", "patch", "valkeycluster", clusterName,
				"--type=merge", "-p", `{"spec":{"replicas":1}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the cluster to return to Ready with two nodes")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
					"cluster state: %s, reason: %s", cr.Status.State, cr.Status.Reason)

				nodes, err := utils.GetValkeyClusterNodes(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(nodes.Items).To(HaveLen(2), "the excess ValkeyNode should be removed")
			}).Should(Succeed())

			By("verifying no keys were lost")
			// The removed node held the primary role, so its slots had to be handed
			// off during termination rather than dropped.
			Eventually(func(g Gomega) {
				found, err := utils.CountValkeyKeys(podFor(clusterName, 0, 0), keyPrefix, seedKeys, cliOpts)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(Equal(seedKeys),
					"keys should survive removal of the primary-holding node")
			}).Should(Succeed())
		})
	})

	// Losing primaries outright, with no replica willing to promote itself, leaves
	// their shards with no slot-bearing primary. Recovery needs the operator to
	// issue CLUSTER FAILOVER TAKEOVER to the orphaned replicas.
	Context("when primaries are lost and replicas will not self-promote", Label("quorum-loss"), func() {
		const clusterName = "valkeycluster-quorum-loss-e2e"
		// Shards 0 and 1 lose their primaries, leaving shard 2 holding the
		// majority so the surviving nodes cannot vote a failover through.
		lostShards := []int{0, 1}

		AfterEach(func() {
			collectOnFailure()
			deleteCluster(clusterName)
		})

		It("promotes the orphaned replicas and serves all slots again", func() {
			By("creating a 3-shard cluster with 1 replica and no persistence")
			applyCluster(fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
`, clusterName))

			By("waiting for the cluster to become Ready")
			waitForReady(clusterName)

			By("confirming the primaries are at node-index 0")
			// The scenario deletes node-index 0 of each lost shard, so a primary
			// that has moved would make the deletion target a replica instead.
			for _, shard := range lostShards {
				Eventually(func(g Gomega) {
					role, err := roleOf(clusterName, shard, 0)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(role).To(Equal("primary"))
				}).Should(Succeed())
			}

			By("stopping the replicas from promoting themselves")
			// Configure the replicas to not perform automatic failover, so that
			// only the operator can promote them. A manual CLUSTER FAILOVER
			// TAKEOVER still applies. Node-index 1 is the sole replica of each
			// shard at replicas: 1.
			for _, shard := range lostShards {
				_, err := utils.ValkeyCLI(podFor(clusterName, shard, 1), cliOpts,
					"CONFIG", "SET", "cluster-replica-no-failover", "yes")
				Expect(err).NotTo(HaveOccurred())
			}

			By("stopping the primaries from handing off on SIGTERM")
			// The operator sets shutdown-on-sigterm failover, so a terminating
			// primary hands the shard to its replica. "now" skips that wait, in
			// case the force-delete below does not outrun it.
			for _, shard := range lostShards {
				_, err := utils.ValkeyCLI(podFor(clusterName, shard, 0), cliOpts,
					"CONFIG", "SET", "shutdown-on-sigterm", "now")
				Expect(err).NotTo(HaveOccurred())
			}

			By("force-deleting both primary pods")
			args := []string{"delete", "pod", "--force", "--grace-period=0"}
			for _, shard := range lostShards {
				args = append(args, podFor(clusterName, shard, 0))
			}
			_, err := utils.Run(exec.Command("kubectl", args...))
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the orphaned replicas to be promoted")
			// Only the operator can promote them now, via CLUSTER FAILOVER
			// TAKEOVER in promoteOrphanedReplicas.
			for _, shard := range lostShards {
				Eventually(func(g Gomega) {
					role, err := roleOf(clusterName, shard, 1)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(role).To(Equal("primary"),
						"shard %d replica should be promoted", shard)
				}).Should(Succeed())
			}

			By("waiting for the cluster to become Ready again")
			waitForReady(clusterName)

			By("verifying the whole keyspace is served")
			// Data could be lost here (no persistence, and the primaries were
			// killed without a hand-off), so slot coverage is what recovery means.
			Eventually(func(g Gomega) {
				out, err := utils.ValkeyCLI(podFor(clusterName, 2, 0), cliOpts, "CLUSTER", "INFO")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("cluster_state:ok"))
				g.Expect(out).To(ContainSubstring("cluster_slots_ok:16384"))
			}).Should(Succeed())
		})
	})
})
