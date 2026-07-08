//go:build e2e

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

// keyCount is the number of keys written across the keyspace before the
// failover, and verified afterwards to prove no data was lost.
const keyCount = 50

var _ = Describe("Shutdown-on-SIGTERM failover", Label("failover"), func() {
	var valkeyName string

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}

		By("cleaning up test resources")
		cmd := exec.Command("kubectl", "delete", "valkeycluster", valkeyName, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)
	})

	It("promotes a replica before a gracefully terminated primary exits", func() {
		valkeyName = "failover-sigterm"
		createFailoverCluster(valkeyName)

		By("identifying the primary and replica of shard 0")
		var primaryPod, replicaPod string
		Eventually(func(g Gomega) {
			primaryPod, replicaPod = getShardRoles(g, valkeyName, 0)
			g.Expect(primaryPod).NotTo(BeEmpty(), "shard 0 has no primary")
			g.Expect(replicaPod).NotTo(BeEmpty(), "shard 0 has no in-sync replica")
		}).WithTimeout(3 * time.Minute).Should(Succeed())

		By("recording the primary pod UID so its replacement can be detected")
		cmd := exec.Command("kubectl", "get", "pod", primaryPod, "-o", "jsonpath={.metadata.uid}")
		oldPrimaryUID, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to get primary pod UID")
		Expect(oldPrimaryUID).NotTo(BeEmpty())

		By("writing keys across the keyspace")
		writeTestKeys(primaryPod)

		By("gracefully terminating the primary pod (SIGTERM with default grace period)")
		cmd = exec.Command("kubectl", "delete", "pod", primaryPod, "--wait=false")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete primary pod")

		// The handover must complete inside terminationGracePeriodSeconds
		// (default 30s), so the replica has to report role:master within that
		// window; otherwise SIGKILL would have cut the handoff short.
		By("asserting the replica is promoted within the grace period")
		Eventually(func(g Gomega) {
			output, err := execValkeyPodShell(replicaPod, "valkey-cli INFO replication")
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get replication info from replica")
			g.Expect(output).To(ContainSubstring("role:master"),
				"replica was not promoted to primary")
		}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		By("asserting the shard keeps serving writes through the disruption")
		Eventually(func(g Gomega) {
			output, err := execValkeyPodShell(replicaPod, "valkey-cli -c set e2e:failover:post v-post")
			g.Expect(err).NotTo(HaveOccurred(), "Failed to write through promoted primary")
			g.Expect(output).To(ContainSubstring("OK"))
		}).Should(Succeed())

		By("waiting for the replaced pod to come back and rejoin as replica")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", primaryPod, "-o", "jsonpath={.metadata.uid}")
			uid, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "replacement pod does not exist yet")
			g.Expect(uid).NotTo(Equal(oldPrimaryUID), "old pod is still terminating")

			output, err := execValkeyPodShell(primaryPod, "valkey-cli INFO replication")
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get replication info from replaced pod")
			g.Expect(output).To(ContainSubstring("role:slave"),
				"replaced pod did not rejoin the shard as a replica")
		}).WithTimeout(4 * time.Minute).Should(Succeed())

		verifyClusterHealthyAndKeysIntact(valkeyName, replicaPod)
	})

	It("recovers when the primary's StatefulSet is deleted", func() {
		valkeyName = "failover-sts-delete"
		createFailoverCluster(valkeyName)

		By("identifying the primary and replica of shard 0")
		var primaryPod, replicaPod string
		Eventually(func(g Gomega) {
			primaryPod, replicaPod = getShardRoles(g, valkeyName, 0)
			g.Expect(primaryPod).NotTo(BeEmpty(), "shard 0 has no primary")
			g.Expect(replicaPod).NotTo(BeEmpty(), "shard 0 has no in-sync replica")
		}).WithTimeout(3 * time.Minute).Should(Succeed())

		By("writing keys across the keyspace")
		writeTestKeys(primaryPod)

		By("deleting the primary's StatefulSet to deschedule the whole workload")
		// Pods are named <statefulset>-0.
		primarySts := strings.TrimSuffix(primaryPod, "-0")
		cmd := exec.Command("kubectl", "delete", "statefulset", primarySts, "--wait=false")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete primary StatefulSet")

		By("asserting the replica is promoted")
		Eventually(func(g Gomega) {
			output, err := execValkeyPodShell(replicaPod, "valkey-cli INFO replication")
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get replication info from replica")
			g.Expect(output).To(ContainSubstring("role:master"),
				"replica was not promoted to primary")
		}).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())

		By("waiting for the operator to recreate the StatefulSet and the pod to rejoin as replica")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "statefulset", primarySts, "-o", "jsonpath={.status.readyReplicas}")
			ready, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "recreated StatefulSet does not exist yet")
			g.Expect(ready).To(Equal("1"), "recreated StatefulSet has no ready replicas")

			output, err := execValkeyPodShell(primaryPod, "valkey-cli INFO replication")
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get replication info from recreated pod")
			g.Expect(output).To(ContainSubstring("role:slave"),
				"recreated pod did not rejoin the shard as a replica")
		}).WithTimeout(4 * time.Minute).Should(Succeed())

		verifyClusterHealthyAndKeysIntact(valkeyName, replicaPod)
	})
})

// createFailoverCluster creates a ValkeyCluster with one replica per shard and
// waits for it to become Ready.
func createFailoverCluster(valkeyName string) {
	GinkgoHelper()

	By("creating a ValkeyCluster with one replica per shard")
	valkeyYaml := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
`, valkeyName)

	manifestFile := filepath.Join(os.TempDir(), fmt.Sprintf("%s.yaml", valkeyName))
	err := os.WriteFile(manifestFile, []byte(valkeyYaml), 0644)
	Expect(err).NotTo(HaveOccurred(), "Failed to write manifest file")
	defer func() {
		Expect(os.Remove(manifestFile)).To(Succeed())
	}()

	cmd := exec.Command("kubectl", "create", "-f", manifestFile)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")

	By("waiting for the cluster to become Ready")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "valkeycluster", valkeyName,
			"-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to get ValkeyCluster Ready condition")
		g.Expect(output).To(Equal("True"), "ValkeyCluster is not Ready")
	}).WithTimeout(4 * time.Minute).Should(Succeed())
}

// writeTestKeys writes keyCount keys across the keyspace via the given pod.
func writeTestKeys(pod string) {
	GinkgoHelper()

	script := fmt.Sprintf(
		"ok=0; for i in $(seq 1 %d); do "+
			"r=$(valkey-cli -c set e2e:failover:$i v$i); "+
			"[ \"$r\" = \"OK\" ] && ok=$((ok+1)); "+
			"done; echo written=$ok", keyCount)
	output, err := execValkeyPodShell(pod, script)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to write keys: %s", output))
	Expect(output).To(ContainSubstring(fmt.Sprintf("written=%d", keyCount)),
		fmt.Sprintf("Not all keys were written: %s", output))
}

// verifyClusterHealthyAndKeysIntact asserts the ValkeyCluster returns to
// Ready, the cluster reports a healthy state, and every test key survived the
// disruption.
func verifyClusterHealthyAndKeysIntact(valkeyName, pod string) {
	GinkgoHelper()

	By("asserting the ValkeyCluster returns to Ready")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "valkeycluster", valkeyName,
			"-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to get ValkeyCluster Ready condition")
		g.Expect(output).To(Equal("True"), "ValkeyCluster did not return to Ready")
	}).Should(Succeed())

	By("asserting the cluster reports a healthy state")
	Eventually(func(g Gomega) {
		output, err := execValkeyPodShell(pod, "valkey-cli CLUSTER INFO")
		g.Expect(err).NotTo(HaveOccurred(), "Failed to get cluster info")
		g.Expect(output).To(ContainSubstring("cluster_state:ok"))
	}).Should(Succeed())

	By("asserting no keys were lost across the disruption")
	script := fmt.Sprintf(
		"ok=0; for i in $(seq 1 %d); do "+
			"v=$(valkey-cli -c get e2e:failover:$i); "+
			"[ \"$v\" = \"v$i\" ] && ok=$((ok+1)); "+
			"done; echo readable=$ok", keyCount)
	Eventually(func(g Gomega) {
		output, err := execValkeyPodShell(pod, script)
		g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to read keys back: %s", output))
		g.Expect(output).To(ContainSubstring(fmt.Sprintf("readable=%d", keyCount)),
			fmt.Sprintf("Some keys were lost across the disruption: %s", output))
	}).Should(Succeed())
}

// execValkeyPodShell runs a shell script inside the pod's server container
// with VALKEYCLI_AUTH unset. The operator injects that variable for the probe
// scripts, and valkey-cli would otherwise automatically send AUTH as the
// default user — which fails and pollutes the command output. Commands run as
// the default (nopass) user, like the rest of the e2e suite.
func execValkeyPodShell(pod string, script string) (string, error) {
	cmd := exec.Command("kubectl", "exec", pod, "-c", "server", "--",
		"sh", "-c", "unset VALKEYCLI_AUTH; "+script)
	return utils.Run(cmd)
}

// getShardRoles returns the pod names of the primary and replica of the given
// shard. Roles are read live from INFO replication on each pod rather than
// from ValkeyNode status, which can lag behind role changes (see #261). The
// replica is only accepted once its replication link is up, so the failover
// is not attempted against a still-syncing replica.
func getShardRoles(g Gomega, clusterName string, shardIndex int) (primaryPod, replicaPod string) {
	cmd := exec.Command("kubectl", "get", "pods",
		"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/shard-index=%d", clusterName, shardIndex),
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
