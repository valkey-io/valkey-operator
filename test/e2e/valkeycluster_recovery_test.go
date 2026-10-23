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

	// Without persistence the server container of a crashed primary restarts in
	// place with its emptyDir, reloads nodes.conf and would come back as the
	// same node with an empty dataset, and its replica would full-sync to
	// nothing. The start script drops that identity instead, so the replica is
	// promoted and keeps the shard's data, and the restarted node rejoins as its
	// replica.
	Context("when a primary's server crashes without persistence", Label("primary-crash"), func() {
		const clusterName = "valkeycluster-primary-crash-e2e"
		const keyPrefix = "e2e:crash"
		const seedKeys = 60

		AfterEach(func() {
			collectOnFailure(clusterName)
			deleteCluster(clusterName)
		})

		// dbsizeOf reads the key count of one node.
		dbsizeOf := func(g Gomega, pod string) int {
			out, err := utils.ValkeyCLI(pod, cliOpts, "DBSIZE")
			g.Expect(err).NotTo(HaveOccurred())
			n, err := strconv.Atoi(strings.TrimSpace(out))
			g.Expect(err).NotTo(HaveOccurred(), out)
			return n
		}

		// restartsOf reads the restart count of the server container of a pod.
		restartsOf := func(g Gomega, pod string) int {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", pod, "-o",
				`jsonpath={.status.containerStatuses[?(@.name=="server")].restartCount}`))
			g.Expect(err).NotTo(HaveOccurred())
			n, err := strconv.Atoi(strings.TrimSpace(out))
			g.Expect(err).NotTo(HaveOccurred(), out)
			return n
		}

		It("keeps the shard's data and rejoins the restarted node as a replica", func() {
			primary := podFor(clusterName, 0, 0)
			replica := podFor(clusterName, 0, 1)

			By("creating a 3-shard cluster with 1 replica and no persistence")
			applyCluster(fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  config:
    enable-debug-command: local
`, clusterName))

			By("waiting for the cluster to become Ready")
			waitForReady(clusterName)

			By("seeding keys across all shards")
			// Few enough writes that no default save rule fires: a snapshot
			// would mask the empty restart this test is about.
			Eventually(func(g Gomega) {
				g.Expect(utils.WriteValkeyKeys(primary, keyPrefix, seedKeys, cliOpts)).To(Succeed())
			}).Should(Succeed())

			By("recording shard 0's key count once the replica has it")
			var keysBefore, restartsBefore int
			Eventually(func(g Gomega) {
				keysBefore = dbsizeOf(g, primary)
				g.Expect(keysBefore).To(BeNumerically(">", 0))
				g.Expect(dbsizeOf(g, replica)).To(Equal(keysBefore))
				restartsBefore = restartsOf(g, primary)
			}).Should(Succeed())

			By("crashing shard 0's primary server")
			// The connection dies with the server, so the command itself fails.
			_, _ = utils.ValkeyCLI(primary, cliOpts, "DEBUG", "SEGFAULT")

			By("waiting for the server container to restart in place")
			Eventually(func(g Gomega) {
				g.Expect(restartsOf(g, primary)).To(BeNumerically(">", restartsBefore))
			}, 2*time.Minute).Should(Succeed())

			By("waiting for the cluster to return to Ready")
			waitForReady(clusterName)

			By("verifying the former replica is the primary and kept every key")
			Eventually(func(g Gomega) {
				role, err := roleOf(clusterName, 0, 1)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(role).To(Equal("primary"))
				g.Expect(dbsizeOf(g, replica)).To(Equal(keysBefore))
			}).Should(Succeed())

			By("verifying the restarted node rejoined as its replica with the same keys")
			Eventually(func(g Gomega) {
				role, err := roleOf(clusterName, 0, 0)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(role).To(Equal("replica"))
				g.Expect(dbsizeOf(g, primary)).To(Equal(keysBefore))
			}).Should(Succeed())

			By("verifying no keys were lost")
			Eventually(func(g Gomega) {
				found, err := utils.CountValkeyKeys(podFor(clusterName, 1, 0), keyPrefix, seedKeys, cliOpts)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(Equal(seedKeys))
			}).Should(Succeed())
		})
	})
})
