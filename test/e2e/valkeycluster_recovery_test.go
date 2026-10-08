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
	// holding the cluster's pods, plus each Valkey server's own view and log.
	collectOnFailure := func(cluster string) {
		if !CurrentSpecReport().Failed() {
			return
		}
		utils.CollectDebugInfo("default")
		utils.CollectDebugInfo(namespace)
		utils.CollectValkeyDebugInfo(cluster, cliOpts)
	}

	// Losing a majority of primaries at once leaves their shards with no
	// slot-bearing primary and no failover quorum, so Valkey cannot elect a
	// replacement until the operator breaks the deadlock.
	Context("when a majority of primaries are lost at once", Label("quorum-loss"), func() {
		const clusterName = "valkeycluster-quorum-loss-e2e"
		// Shards 0 and 1 lose their primaries, leaving shard 2 holding the
		// majority so the surviving nodes cannot vote a failover through.
		lostShards := []int{0, 1}

		AfterEach(func() {
			collectOnFailure(clusterName)
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

			By("stopping the primaries from handing off on SIGTERM")
			// The operator sets shutdown-on-sigterm failover, so a terminating
			// primary hands the shard to its replica. "now" skips that wait, in
			// case the force-delete below does not outrun it. "nosave" suppresses
			// any final RDB.
			for _, shard := range lostShards {
				_, err := utils.ValkeyCLI(podFor(clusterName, shard, 0), cliOpts,
					"CONFIG", "SET", "shutdown-on-sigterm", `"nosave now"`)
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
			// Both primaries are down, so quorum is lost and no election can
			// complete until the operator forces a takeover. That first promotion
			// restores quorum, and the shard promoted after it is the one a
			// cluster-wide gate would strand: the assertion below only passes once
			// every shard has recovered, not just the first.
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
