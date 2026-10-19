//go:build e2e

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

// Covers https://github.com/valkey-io/valkey-operator/issues/533: without
// persistence the server container of a crashed primary restarts in place
// with its emptyDir, reloads nodes.conf and would come back as the same node
// with an empty dataset, and its replica would full-sync to nothing. The
// entrypoint drops that identity instead, so the replica is promoted and keeps
// the shard's data, and the restarted node rejoins as its replica.
var _ = Describe("ValkeyCluster primary crash without persistence", Ordered, Label("ValkeyCluster", "CrashRecovery"), func() {
	const clusterName = "cluster-crash-primary"
	const keyCount = 60

	primaryPod := fmt.Sprintf("valkey-%s-0-0-0", clusterName)
	replicaPod := fmt.Sprintf("valkey-%s-0-1-0", clusterName)

	// valkeyCLI runs a shell command in the server container of a pod with
	// the probe credentials unset, so valkey-cli uses the default user.
	valkeyCLI := func(pod, command string) (string, error) {
		cmd := exec.Command("kubectl", "exec", pod, "-c", "server", "--",
			"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; "+command)
		return utils.Run(cmd)
	}

	dbsize := func(g Gomega, pod string) int {
		output, err := valkeyCLI(pod, "valkey-cli DBSIZE")
		g.Expect(err).NotTo(HaveOccurred())
		n, err := strconv.Atoi(strings.TrimSpace(output))
		g.Expect(err).NotTo(HaveOccurred(), output)
		return n
	}

	role := func(g Gomega, pod string) string {
		output, err := valkeyCLI(pod, "valkey-cli ROLE | head -n1")
		g.Expect(err).NotTo(HaveOccurred())
		return strings.TrimSpace(output)
	}

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	It("keeps the shard's data when a primary's server crashes", func() {
		By("creating the cluster without persistence")
		manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  config:
    enable-debug-command: local
`, clusterName)
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName,
			"--ignore-not-found=true", "--wait=true", "--timeout=120s")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to remove a leftover ValkeyCluster CR")

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the ValkeyCluster CR")

		By("waiting for the cluster to reach Ready")
		verifyClusterReady := func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
		}
		Eventually(verifyClusterReady, 5*time.Minute).Should(Succeed())

		By("writing keys to every shard")
		// Few enough writes that no default save rule fires: a snapshot
		// would mask the empty restart this test is about.
		Eventually(func(g Gomega) {
			output, err := valkeyCLI(primaryPod, fmt.Sprintf(
				"for i in $(seq 1 %d); do valkey-cli -c set crash-key-$i $i; done", keyCount))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.Count(output, "OK")).To(Equal(keyCount))
		}).Should(Succeed())

		By("recording the shard's key count once the replica has it")
		var keysBefore int
		Eventually(func(g Gomega) {
			keysBefore = dbsize(g, primaryPod)
			g.Expect(keysBefore).To(BeNumerically(">", 0))
			g.Expect(dbsize(g, replicaPod)).To(Equal(keysBefore))
		}).Should(Succeed())

		restartCount := func(g Gomega) int {
			cmd := exec.Command("kubectl", "get", "pod", primaryPod, "-o",
				`jsonpath={.status.containerStatuses[?(@.name=="server")].restartCount}`)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			n, err := strconv.Atoi(strings.TrimSpace(output))
			g.Expect(err).NotTo(HaveOccurred(), output)
			return n
		}
		var restartsBefore int
		Eventually(func(g Gomega) { restartsBefore = restartCount(g) }).Should(Succeed())

		By("crashing the primary's server process")
		// The connection dies with the server, so the command itself fails.
		_, _ = valkeyCLI(primaryPod, "valkey-cli DEBUG SEGFAULT")

		By("waiting for the server container to restart in place")
		Eventually(func(g Gomega) {
			g.Expect(restartCount(g)).To(BeNumerically(">", restartsBefore))
		}, 2*time.Minute).Should(Succeed())

		By("waiting for the cluster to return to Ready")
		Eventually(verifyClusterReady, 5*time.Minute).Should(Succeed())

		By("verifying the former replica is the primary and kept every key")
		Eventually(func(g Gomega) {
			g.Expect(role(g, replicaPod)).To(Equal("master"))
			g.Expect(dbsize(g, replicaPod)).To(Equal(keysBefore))
		}, 3*time.Minute).Should(Succeed())

		By("verifying the restarted node rejoined as its replica")
		Eventually(func(g Gomega) {
			g.Expect(role(g, primaryPod)).To(Equal("slave"))
			g.Expect(dbsize(g, primaryPod)).To(Equal(keysBefore))
		}, 3*time.Minute).Should(Succeed())

		By("verifying every key is readable through the cluster")
		Eventually(func(g Gomega) {
			output, err := valkeyCLI(replicaPod, fmt.Sprintf(
				"n=0; for i in $(seq 1 %d); do [ \"$(valkey-cli -c get crash-key-$i)\" = \"$i\" ] && n=$((n+1)); done; echo $n", keyCount))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal(strconv.Itoa(keyCount)))
		}).Should(Succeed())
	})

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName,
			"--ignore-not-found=true", "--wait=true", "--timeout=120s")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete the ValkeyCluster during teardown")
	})
})
