//go:build e2e
// +build e2e

/*
Copyright 2026 Valkey Contributors.

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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	// holding the cluster's pods, plus each Valkey server's own view and log.
	collectOnFailure := func(cluster string) {
		if !CurrentSpecReport().Failed() {
			return
		}
		utils.CollectDebugInfo("default")
		utils.CollectDebugInfo(namespace)
		utils.CollectValkeyDebugInfo(cluster, cliOpts)
	}

	// A replica scale-down removes the highest node-index on every shard. When one
	// shard's primary has failed over onto that index, completing the scale-down
	// means removing the node currently serving it. That one shard's remediation
	// must not hold back the others: the shards whose primary did not move still
	// scale down, so the cluster settles at two nodes per shard.
	Context("when one shard's primary sits on a node the new replica count removes", Label("replica-scale-down"), func() {
		const clusterName = "valkeycluster-replica-scaledown-e2e"
		const keyPrefix = "e2e:scaledown"
		const seedKeys = 500

		AfterEach(func() {
			collectOnFailure(clusterName)
			deleteCluster(clusterName)
		})

		It("scales every shard down even though one shard's primary is out of range", func() {
			By("creating a 3-shard cluster with 2 replicas, so node-index 2 exists on each shard")
			applyCluster(fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 2
`, clusterName))

			By("waiting for the cluster to become Ready")
			waitForReady(clusterName)

			By("seeding keys across all shards so the scale-down can be checked for data loss")
			Eventually(func(g Gomega) {
				g.Expect(utils.WriteValkeyKeys(podFor(clusterName, 0, 0), keyPrefix, seedKeys, cliOpts)).To(Succeed())
			}).Should(Succeed())

			By("failing over only shard 0 to node-index 2, moving its primary off index 0")
			// A graceful CLUSTER FAILOVER on the index-2 replica promotes it and
			// holds writes until it has caught up, so no keys are lost.
			_, err := utils.ValkeyCLI(podFor(clusterName, 0, 2), cliOpts, "CLUSTER", "FAILOVER")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for shard 0's node-index 2 to report primary")
			Eventually(func(g Gomega) {
				role, err := roleOf(clusterName, 0, 2)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(role).To(Equal("primary"), "shard 0 node-index 2 should be primary")
			}).Should(Succeed())

			By("scaling replicas from 2 to 1, which puts shard 0's primary out of range")
			cmd := exec.Command("kubectl", "patch", "valkeycluster", clusterName,
				"--type=merge", "-p", `{"spec":{"replicas":1}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the whole cluster to return to Ready with two nodes per shard")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
					"cluster state: %s, reason: %s", cr.Status.State, cr.Status.Reason)

				nodes, err := utils.GetValkeyClusterNodes(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(nodes.Items).To(HaveLen(3*2),
					"all three shards should scale down to two nodes")
			}).Should(Succeed())

			By("confirming shard 0 recovered a primary on a remaining node")
			Eventually(func(g Gomega) {
				role0, err := roleOf(clusterName, 0, 0)
				g.Expect(err).NotTo(HaveOccurred())
				role1, err := roleOf(clusterName, 0, 1)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect([]string{role0, role1}).To(ContainElement("primary"),
					"one of shard 0's remaining nodes should be promoted")
			}).Should(Succeed())

			By("verifying no keys were lost")
			// Shard 1's node 0 is an untouched primary; -c redirects from there
			// read every shard's keys, including shard 0's handed-off slots.
			Eventually(func(g Gomega) {
				found, err := utils.CountValkeyKeys(podFor(clusterName, 1, 0), keyPrefix, seedKeys, cliOpts)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(Equal(seedKeys),
					"keys should survive the scale-down")
			}).Should(Succeed())
		})
	})

	// A primary drained to zero slots stays a primary and keeps its replica,
	// since the operator sets cluster-allow-replica-migration no. A scale-in
	// reverted right after its drain leaves such a shard behind. The operator
	// treats it as an empty shard: the status names it, and the rebalancer
	// moves slots onto it as it does for a new shard.
	Context("when a shard's primary owns no slots but keeps its replica", Label("empty-shard"), func() {
		const clusterName = "valkeycluster-empty-shard-e2e"
		const keyPrefix = "e2e:emptyshard"
		const seedKeys = 300

		AfterEach(func() {
			collectOnFailure(clusterName)
			deleteCluster(clusterName)
		})

		// shell runs a script in the server container of pod with the probe
		// credentials unset, so valkey-cli uses the default user.
		shell := func(pod, script string) (string, error) {
			return utils.Run(exec.Command("kubectl", "exec", pod, "-c", "server", "--",
				"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; "+script))
		}

		// ownedSlots counts the slots the node owns, from its own line of
		// CLUSTER NODES. Migration markers in brackets are not owned slots.
		ownedSlots := func(g Gomega, pod string) int {
			out, err := shell(pod, "valkey-cli CLUSTER NODES")
			g.Expect(err).NotTo(HaveOccurred())
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Fields(line)
				if len(fields) < 8 || !strings.Contains(fields[2], "myself") {
					continue
				}
				n := 0
				for _, r := range fields[8:] {
					if strings.HasPrefix(r, "[") {
						continue
					}
					lo, hi, ok := strings.Cut(r, "-")
					if !ok {
						hi = lo
					}
					a, err := strconv.Atoi(lo)
					g.Expect(err).NotTo(HaveOccurred(), r)
					b, err := strconv.Atoi(hi)
					g.Expect(err).NotTo(HaveOccurred(), r)
					n += b - a + 1
				}
				return n
			}
			g.Expect(out).To(ContainSubstring("myself"), "no own line in CLUSTER NODES of %s", pod)
			return 0
		}

		It("names the shard and rebalances slots back onto it", func() {
			primary := podFor(clusterName, 3, 0)
			replica := podFor(clusterName, 3, 1)

			By("creating a 4-shard cluster with 1 replica")
			applyCluster(fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 4
  replicas: 1
`, clusterName))

			By("waiting for the cluster to become Ready")
			waitForReady(clusterName)

			By("seeding keys across all shards")
			Eventually(func(g Gomega) {
				g.Expect(utils.WriteValkeyKeys(podFor(clusterName, 0, 0), keyPrefix, seedKeys, cliOpts)).To(Succeed())
			}).Should(Succeed())

			By("draining shard 3's primary by hand while the operator is paused")
			withOperatorPaused(func() {
				id, err := shell(primary, "valkey-cli CLUSTER MYID")
				Expect(err).NotTo(HaveOccurred())
				// Weight 0 moves every slot off the node. It stays a primary,
				// with its replica attached, as after the operator's own drain.
				out, err := shell(primary, "valkey-cli --cluster rebalance 127.0.0.1:6379 --cluster-weight "+strings.TrimSpace(id)+"=0")
				Expect(err).NotTo(HaveOccurred(), out)
				Eventually(func(g Gomega) {
					g.Expect(ownedSlots(g, primary)).To(BeZero())
					info, err := shell(primary, "valkey-cli INFO replication")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(info).To(ContainSubstring("role:master"))
					g.Expect(info).To(ContainSubstring("connected_slaves:1"))
					info, err = shell(replica, "valkey-cli INFO replication")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(info).To(ContainSubstring("master_link_status:up"))
				}).Should(Succeed())
			})

			By("waiting for the status to name the shard being refilled")
			// Shard 3 is the only shard under its share, so every batch
			// goes to it and the message names it throughout.
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				ready := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionReady)
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(ready.Message).To(ContainSubstring("Rebalancing slots onto shard 3"))
			}, 2*time.Minute).Should(Succeed())

			By("waiting for the cluster to return to Ready with slots back on shard 3")
			waitForReady(clusterName)
			Eventually(func(g Gomega) {
				g.Expect(ownedSlots(g, primary)).To(BeNumerically(">=", 4095))
			}).Should(Succeed())

			By("verifying no keys were lost")
			Eventually(func(g Gomega) {
				found, err := utils.CountValkeyKeys(podFor(clusterName, 0, 0), keyPrefix, seedKeys, cliOpts)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(Equal(seedKeys))
			}).Should(Succeed())
		})
	})

	// Two of three primaries are lost one after another. Depending on timing,
	// the second dies before or after the first shard is recovered, so quorum is
	// either lost or already restored when it is detected. The replicas have
	// native failover disabled, so every promotion is the operator's: a shard
	// that recovered on its own once quorum returned would hide one the operator
	// stranded.
	Context("when primaries are lost one after another", Label("quorum-loss"), func() {
		const clusterName = "valkeycluster-quorum-loss-e2e"
		// Shards 0 and 1 lose their primaries; shard 2's stays up and reports
		// the failures.
		lostShards := []int{0, 1}

		AfterEach(func() {
			collectOnFailure(clusterName)
			deleteCluster(clusterName)
		})

		It("promotes the orphaned replicas and serves all slots again", func() {
			By("creating a 3-shard cluster with 1 replica and no persistence (native failover disabled)")
			applyCluster(fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  config:
    cluster-replica-no-failover: "yes"
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

			By("confirming the replicas report native failover disabled")
			for _, shard := range lostShards {
				out, err := utils.ValkeyCLI(podFor(clusterName, shard, 1), cliOpts, "CLUSTER", "NODES")
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(MatchRegexp(`(?m)^\S+ \S+ myself,\S*nofailover\s`), "replica of shard %d does not report nofailover", shard)
			}

			By("stopping the primaries from handing off on SIGTERM")
			// The operator sets shutdown-on-sigterm failover, so a terminating
			// primary hands the shard to its replica. "now" skips that wait, and
			// "nosave" suppresses any final RDB.
			for _, shard := range lostShards {
				_, err := utils.ValkeyCLI(podFor(clusterName, shard, 0), cliOpts,
					"CONFIG", "SET", "shutdown-on-sigterm", `"nosave now"`)
				Expect(err).NotTo(HaveOccurred())
			}
			firstId, err := utils.ValkeyCLI(podFor(clusterName, lostShards[0], 0), cliOpts, "CLUSTER", "MYID")
			Expect(err).NotTo(HaveOccurred())
			firstId = strings.TrimSpace(firstId)

			By("force-deleting the first primary pod")
			forceDelete := func(pod string) {
				GinkgoHelper()
				_, err := utils.Run(exec.Command("kubectl", "delete", "pod", pod, "--force", "--grace-period=0"))
				Expect(err).NotTo(HaveOccurred())
			}
			forceDelete(podFor(clusterName, lostShards[0], 0))

			By("waiting until the first primary is failed or already replaced")
			// Deleting the second primary only once the first is down makes the
			// failures land in different scrapes. A promotion that restores
			// quorum must then not strand the shard detected after it.
			Eventually(func(g Gomega) {
				if role, err := roleOf(clusterName, lostShards[0], 1); err == nil && role == "primary" {
					return
				}
				out, err := utils.ValkeyCLI(podFor(clusterName, 2, 0), cliOpts, "CLUSTER", "NODES")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(MatchRegexp(`(?m)^`+firstId+` \S+ (\S+,)?fail\??(,|\s)`), "shard 2 does not report the first primary failed yet")
			}).Should(Succeed())

			By("force-deleting the second primary pod")
			forceDelete(podFor(clusterName, lostShards[1], 0))

			By("waiting for the orphaned replicas to be promoted")
			// With native failover disabled, each shard recovers only through
			// the operator's takeover, whether or not quorum holds at the time.
			Eventually(func(g Gomega) {
				for _, shard := range lostShards {
					role, err := roleOf(clusterName, shard, 1)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(role).To(Equal("primary"),
						"shard %d replica should be promoted", shard)
				}
			}).Should(Succeed())

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
