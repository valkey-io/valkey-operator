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

	// writeKeys writes keys <prefix>1..<prefix>n with values v1..vn through the
	// cluster from pod, piping them into one valkey-cli.
	writeKeys := func(pod, prefix string, n int) {
		GinkgoHelper()
		out, err := utils.ValkeyShell(pod, fmt.Sprintf(
			"n=$(for i in $(seq 1 %d); do echo \"set %s$i v$i\"; done "+
				"| valkey-cli -t 2 -c 2>/dev/null | grep -c '^OK$'); echo written=$n", n, prefix))
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring(fmt.Sprintf("written=%d", n)))
	}

	// countKeys returns how many of the keys written by writeKeys the given
	// primaries hold, and a summary of unexpected replies (such as errors) to
	// explain a shortfall. Each primary is asked without following redirects,
	// so a key counts once, on the shard that owns it, and a stale address in
	// one node's view of another cannot stall the count.
	countKeys := func(g Gomega, primaries []string, prefix string, n int) (int, string) {
		total := 0
		var other []string
		for _, pod := range primaries {
			out, err := utils.ValkeyShell(pod, fmt.Sprintf(
				"out=$(for i in $(seq 1 %d); do echo \"get %s$i\"; done | valkey-cli -t 2 2>&1); "+
					"echo \"$out\" | grep -c '^v'; echo \"$out\" | grep -v '^v' | grep -vE '^(\\(error\\) )?MOVED ' | sort | uniq -c | head -5",
				n, prefix))
			g.Expect(err).NotTo(HaveOccurred(), out)
			lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
			found := 0
			_, scanErr := fmt.Sscanf(lines[0], "%d", &found)
			g.Expect(scanErr).NotTo(HaveOccurred(), out)
			total += found
			if len(lines) > 1 {
				other = append(other, pod+":\n"+lines[1])
			}
		}
		return total, strings.Join(other, "\n")
	}

	dbSize := func(g Gomega, pod string) string {
		out, err := utils.ValkeyCLI(pod, cliOpts, "DBSIZE")
		g.Expect(err).NotTo(HaveOccurred(), out)
		return strings.TrimSpace(out)
	}

	// shardRoles returns the shard's primary pod and a replica pod whose
	// replication link is up, read live from INFO replication.
	shardRoles := func(g Gomega, cluster string, shard int) (primaryPod, replicaPod string) {
		out, err := utils.Run(exec.Command("kubectl", "get", "pods",
			"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/shard-index=%d", cluster, shard),
			"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`))
		g.Expect(err).NotTo(HaveOccurred())
		for _, pod := range utils.GetNonEmptyLines(out) {
			info, err := utils.ValkeyCLI(pod, cliOpts, "INFO", "replication")
			g.Expect(err).NotTo(HaveOccurred())
			switch {
			case strings.Contains(info, "role:master"):
				primaryPod = pod
			case strings.Contains(info, "role:slave") && strings.Contains(info, "master_link_status:up"):
				replicaPod = pod
			}
		}
		g.Expect(primaryPod).NotTo(BeEmpty(), "shard %d has no primary", shard)
		g.Expect(replicaPod).NotTo(BeEmpty(), "shard %d has no synced replica", shard)
		return primaryPod, replicaPod
	}

	// When every primary is lost at once, the recreated primary pods start
	// before any replica is promoted, so no primary is reachable when the
	// operator introduces them to the cluster. They must still join the existing
	// cluster, not form a separate one around one of themselves, and be attached
	// as replicas of the promoted primaries.
	Context("when every primary is lost at once", Label("all-primaries-lost"), func() {
		const clusterName = "valkeycluster-all-primaries-lost-e2e"
		const shards = 3
		const keyCount = 150
		const keyPrefix = "e2e:allprimaries:"

		AfterEach(func() {
			collectOnFailure(clusterName)
			deleteCluster(clusterName)
		})

		It("re-forms one cluster when every primary's StatefulSet is deleted at once", func() {
			By("creating a 3-shard cluster with 1 replica and no persistence")
			// A long cluster-node-timeout delays the replica promotions well past
			// the recreated pods' start, so they are introduced to the cluster
			// while no primary is reachable.
			applyCluster(fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: %d
  replicas: 1
  config:
    cluster-node-timeout: "60000"
`, clusterName, shards))

			By("waiting for the cluster to become Ready")
			waitForReady(clusterName)

			By("finding each shard's primary and replica")
			primaries := make([]string, shards)
			replicas := make([]string, shards)
			Eventually(func(g Gomega) {
				for shard := range shards {
					primaries[shard], replicas[shard] = shardRoles(g, clusterName, shard)
				}
			}).Should(Succeed())

			By("writing test keys across the cluster")
			writeKeys(primaries[0], keyPrefix, keyCount)

			By("waiting for every replica to hold its primary's keys")
			Eventually(func(g Gomega) {
				for shard := range shards {
					primaryKeys := dbSize(g, primaries[shard])
					g.Expect(primaryKeys).NotTo(Equal("0"), "shard %d got none of the test keys", shard)
					g.Expect(dbSize(g, replicas[shard])).To(Equal(primaryKeys), "shard %d's replica is not in sync", shard)
				}
			}).Should(Succeed())

			By("stopping the primaries from handing off on SIGTERM")
			// With the operator's shutdown-on-sigterm failover, a terminating
			// primary can hand its slots to its replica before it exits, leaving
			// a live primary to introduce the recreated pods to.
			for _, pod := range primaries {
				out, err := utils.ValkeyCLI(pod, cliOpts, "CONFIG", "SET", "shutdown-on-sigterm", `"nosave now"`)
				Expect(err).NotTo(HaveOccurred(), out)
			}

			By("deleting every primary's StatefulSet at once")
			jsonpath := func(pod, path string) string {
				out, err := utils.Run(exec.Command("kubectl", "get", "pod", pod, "-o", "jsonpath="+path))
				Expect(err).NotTo(HaveOccurred())
				return strings.TrimSpace(out)
			}
			replicaIDs := make([]string, 0, shards)
			for _, pod := range replicas {
				out, err := utils.ValkeyCLI(pod, cliOpts, "CLUSTER", "MYID")
				Expect(err).NotTo(HaveOccurred(), out)
				replicaIDs = append(replicaIDs, strings.TrimSpace(out))
			}
			oldUIDs := make(map[string]string, shards)
			var statefulSets []string
			for _, pod := range primaries {
				oldUIDs[pod] = jsonpath(pod, "{.metadata.uid}")
				sts := jsonpath(pod, `{.metadata.ownerReferences[?(@.kind=="StatefulSet")].name}`)
				Expect(sts).NotTo(BeEmpty(), "pod %s has no StatefulSet owner", pod)
				statefulSets = append(statefulSets, sts)
			}
			for _, sts := range statefulSets {
				_, err := utils.Run(exec.Command("kubectl", "delete", "statefulset", sts, "--wait=false"))
				Expect(err).NotTo(HaveOccurred())
			}

			By("waiting for the primary pods to be recreated")
			// The status still reads Ready right after the deletion, so wait for
			// the replacement pods before waiting for Ready.
			Eventually(func(g Gomega) {
				for _, pod := range primaries {
					out, err := utils.Run(exec.Command("kubectl", "get", "pod", pod, "-o",
						`jsonpath={.metadata.uid} {.status.containerStatuses[?(@.name=="server")].ready}`))
					g.Expect(err).NotTo(HaveOccurred())
					fields := strings.Fields(out)
					g.Expect(fields).To(HaveLen(2), "pod %s has no server container status yet", pod)
					g.Expect(fields[0]).NotTo(Equal(oldUIDs[pod]), "pod %s has not been recreated yet", pod)
					g.Expect(fields[1]).To(Equal("true"), "pod %s is not ready", pod)
				}
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("checking the recreated pods join the existing cluster before any replica is promoted")
			// The scenario under test needs every primary to be down when the
			// recreated pods are MEETed. If a replica wins the race, the test
			// would pass without exercising it, so stop instead. A recreated pod
			// that knows a surviving replica joined the existing cluster rather
			// than only meeting the other recreated pods.
			Eventually(func(g Gomega) {
				for _, pod := range replicas {
					info, err := utils.ValkeyCLI(pod, cliOpts, "INFO", "replication")
					g.Expect(err).NotTo(HaveOccurred(), info)
					if strings.Contains(info, "role:master") {
						StopTrying(fmt.Sprintf("replica %s was promoted before every recreated pod joined the cluster; "+
							"raise cluster-node-timeout", pod)).Now()
					}
				}
				for _, pod := range primaries {
					nodes, err := utils.ValkeyCLI(pod, cliOpts, "CLUSTER", "NODES")
					g.Expect(err).NotTo(HaveOccurred(), nodes)
					g.Expect(replicaIDs).To(ContainElement(Satisfy(func(id string) bool {
						return strings.Contains(nodes, id)
					})), "pod %s has not joined the existing cluster yet", pod)
				}
			}, time.Minute, time.Second).Should(Succeed())

			By("waiting for the cluster to become Ready again")
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
					"cluster state: %s, reason: %s: %s", cr.Status.State, cr.Status.Reason, cr.Status.Message)
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(shards)))
			}, 8*time.Minute, 5*time.Second).Should(Succeed(), "cluster did not recover after every primary was replaced")

			By("verifying every node is in the same cluster")
			Eventually(func(g Gomega) {
				for _, pod := range append(append([]string{}, primaries...), replicas...) {
					info, err := utils.ValkeyCLI(pod, cliOpts, "CLUSTER", "INFO")
					g.Expect(err).NotTo(HaveOccurred(), info)
					g.Expect(info).To(ContainSubstring("cluster_state:ok"), "pod %s", pod)
					g.Expect(info).To(ContainSubstring(fmt.Sprintf("cluster_known_nodes:%d", 2*shards)), "pod %s", pod)
				}
			}).Should(Succeed())

			By("verifying every test key survived")
			// Count on each shard's current primary: depending on whether a
			// replica was promoted, that is the old primary or the old replica.
			Eventually(func(g Gomega) {
				current := make([]string, shards)
				for shard := range shards {
					current[shard], _ = shardRoles(g, clusterName, shard)
				}
				found, other := countKeys(g, current, keyPrefix, keyCount)
				g.Expect(found).To(Equal(keyCount),
					"test keys were lost after every primary was replaced; other replies:\n%s", other)
			}, time.Minute).Should(Succeed())
		})
	})
})
