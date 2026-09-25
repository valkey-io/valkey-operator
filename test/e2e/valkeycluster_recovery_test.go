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

// Scaling replicas down while a shard's primary sits above the new node-index
// bound puts the primary on a node the desired topology no longer includes, so
// completing the scale-down means removing the node currently serving the shard.
var _ = Describe("ValkeyCluster replica scale-down", Label("replica-scale-down"), func() {
	const clusterName = "valkeycluster-replica-scaledown-e2e"
	const keyPrefix = "e2e:scaledown"
	const seedKeys = 500

	// The cluster declares no users, so valkey-cli runs as the default user with
	// no password. The connect timeout bounds a stale MOVED redirect pointing at
	// the terminated pod.
	cliOpts := utils.ValkeyCLIOptions{ConnectTimeoutSeconds: 2}

	AfterEach(func() {
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
	})

	// podFor returns the pod name for a (shard, node) position.
	podFor := func(shard, node int) string {
		return fmt.Sprintf("valkey-%s-%d-%d-0", clusterName, shard, node)
	}

	// roleOf reads the ValkeyNode's reported role. Status.Role is maintained by
	// the RolePoller, so callers poll rather than read once.
	roleOf := func(shard, node int) (string, error) {
		GinkgoHelper()
		n, err := utils.GetValkeyNodeStatus(fmt.Sprintf("%s-%d-%d", clusterName, shard, node))
		if err != nil {
			return "", err
		}
		return n.Status.Role, nil
	}

	It("completes when the primary sits on a node the new replica count removes", func() {
		By("creating a 1-shard cluster with 2 replicas, so node-index 2 exists")
		manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 2
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

		By("seeding keys so the scale-down can be checked for data loss")
		Eventually(func(g Gomega) {
			g.Expect(utils.WriteValkeyKeys(podFor(0, 0), keyPrefix, seedKeys, cliOpts)).To(Succeed())
		}).Should(Succeed())

		By("failing over to node-index 2, moving the primary off index 0")
		// A graceful CLUSTER FAILOVER holds writes until the target has caught
		// up, so no keys are lost here.
		_, err = utils.ValkeyCLI(podFor(0, 2), cliOpts, "CLUSTER", "FAILOVER")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for node-index 2 to report primary")
		Eventually(func(g Gomega) {
			role, err := roleOf(0, 2)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(role).To(Equal("primary"))
		}).Should(Succeed())

		By("scaling replicas from 2 to 1, which puts the primary out of range")
		// nodesPerShard becomes 2, so the primary at node-index 2 is now on a
		// node the desired topology does not include.
		cmd = exec.Command("kubectl", "patch", "valkeycluster", clusterName,
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
			found, err := utils.CountValkeyKeys(podFor(0, 0), keyPrefix, seedKeys, cliOpts)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(Equal(seedKeys),
				"keys should survive removal of the primary-holding node")
		}).Should(Succeed())
	})
})
