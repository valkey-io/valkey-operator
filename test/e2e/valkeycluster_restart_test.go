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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

// Covers https://github.com/valkey-io/valkey-operator/issues/275: a
// persistent cluster whose pods all restart at once comes back with every
// pod IP changed while each node's persisted nodes.conf still holds the
// peers' old addresses. No node is isolated (all IDs are known), so only
// the stale-address heal phase can re-form the cluster - without it, the
// cluster stays in Reconciling with cluster_state:fail until a manual
// CLUSTER MEET.
var _ = Describe("ValkeyCluster full restart recovery", Ordered, Label("ValkeyCluster", "Persistence", "RestartRecovery"), func() {
	const clusterName = "cluster-persistent-restart"
	const canaryKey = "restart-canary"
	const canaryValue = "survives-full-restart"

	podSelector := fmt.Sprintf("valkey.io/cluster=%s", clusterName)

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	It("re-forms the cluster after all pods restart simultaneously", func() {
		By("creating the persistent cluster")
		manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 1
  persistence:
    size: 1Gi
  config:
    appendonly: "yes"
`, clusterName)
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create persistent ValkeyCluster CR")

		By("waiting for the cluster to reach Ready")
		verifyClusterReady := func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
		}
		Eventually(verifyClusterReady, 5*time.Minute).Should(Succeed())

		By("writing a canary key")
		writeCanary := func(g Gomega) {
			cmd := exec.Command("kubectl", "exec", fmt.Sprintf("valkey-%s-0-0-0", clusterName), "-c", "server", "--",
				"sh", "-c",
				fmt.Sprintf("unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli -c set %s %s", canaryKey, canaryValue))
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("OK"))
		}
		Eventually(writeCanary).Should(Succeed())

		// Events are matched by the CR's UID, not just its name, so
		// retained events from an earlier same-named cluster can neither
		// satisfy the heal assertion nor fail the forget assertion. The
		// pre-restart counts scope both assertions to the restart window.
		cmd = exec.Command("kubectl", "get", "valkeycluster", clusterName, "-o", "jsonpath={.metadata.uid}")
		clusterUID, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		clusterUID = strings.TrimSpace(clusterUID)
		Expect(clusterUID).NotTo(BeEmpty())

		countEvents := func(reason string) int {
			cmd := exec.Command("kubectl", "get", "events",
				"--field-selector", fmt.Sprintf("reason=%s,involvedObject.uid=%s", reason, clusterUID),
				"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			return len(utils.GetNonEmptyLines(output))
		}
		healEventsBefore := countEvents("StaleAddressesHealed")
		forgottenEventsBefore := countEvents("StaleNodeForgotten")

		By("deleting all pods at once so every pod IP changes")
		cmd = exec.Command("kubectl", "delete", "pod",
			"-l", podSelector, "--wait=false")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete the cluster's pods")

		By("waiting for the stale-address heal to re-introduce the moved members")
		verifyHealEvent := func(g Gomega) {
			g.Expect(countEvents("StaleAddressesHealed")).To(BeNumerically(">", healEventsBefore),
				"expected a new StaleAddressesHealed event after the full restart")
		}
		Eventually(verifyHealEvent, 5*time.Minute).Should(Succeed())

		By("waiting for the cluster to return to Ready without manual intervention")
		verifyClusterRecovered := func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
		}
		Eventually(verifyClusterRecovered, 5*time.Minute).Should(Succeed())

		By("verifying cluster_state is ok and the canary key survived on disk")
		verifyDataSurvived := func(g Gomega) {
			cmd := exec.Command("kubectl", "exec", fmt.Sprintf("valkey-%s-0-0-0", clusterName), "-c", "server", "--",
				"sh", "-c",
				fmt.Sprintf("unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli cluster info | head -1; valkey-cli -c get %s", canaryKey))
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("cluster_state:ok"))
			g.Expect(output).To(ContainSubstring(canaryValue))
		}
		Eventually(verifyDataSurvived).Should(Succeed())

		By("verifying no live member was forgotten during recovery")
		// The heal's companion guard: forgetStaleNodes must not CLUSTER
		// FORGET members whose address changed - a forget here would ban
		// the node from rejoining for 60s and fight the recovery.
		Expect(countEvents("StaleNodeForgotten")).To(Equal(forgottenEventsBefore),
			"no member should be forgotten while recovering from an all-pods restart")
	})

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "pvc",
			"-l", podSelector, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
	})
})
