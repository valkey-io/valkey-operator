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
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster pause", Label("cluster-lifecycle", "pause"), func() {
	It("defers topology, config and ACL changes until reconciliation resumes", func() {
		const name = "valkeycluster-pause-e2e"
		const annotation = "valkey.io/pause-reconciliation"
		const selector = "valkey.io/cluster=" + name
		kubectl := func(g Gomega, args ...string) string {
			out, err := utils.Run(exec.Command("kubectl", args...))
			g.Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(out)
		}
		cli := func(g Gomega, args ...string) string {
			command := []string{"exec", "valkey-" + name + "-0-0-0", "-c", "server", "--",
				"env", "-u", "VALKEYCLI_AUTH", "-u", "REDISCLI_AUTH", "valkey-cli"}
			return kubectl(g, append(command, args...)...)
		}
		nodes := func(g Gomega) []valkeyiov1alpha1.ValkeyNode {
			var list valkeyiov1alpha1.ValkeyNodeList
			g.Expect(json.Unmarshal([]byte(kubectl(g, "get", "valkeynodes", "-l", selector, "-o", "json")), &list)).To(Succeed())
			return list.Items
		}
		// Identity and generation catch both pod rolls and workload updates.
		snapshot := func(g Gomega) []string {
			return []string{
				kubectl(g, "get", "pods", "-l", selector, "--sort-by=.metadata.name", "-o", "jsonpath={.items[*].metadata.uid}"),
				kubectl(g, "get", "statefulsets", "-l", selector, "--sort-by=.metadata.name", "-o", "jsonpath={.items[*].metadata.generation}"),
				kubectl(g, "get", "configmap", "valkey-"+name, "-o", "jsonpath={.metadata.resourceVersion}"),
				kubectl(g, "get", "secret", "internal-"+name+"-acl", "-o", "jsonpath={.metadata.resourceVersion}"),
			}
		}
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", name, "--ignore-not-found=true", "--wait=false"))
		})
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: ` + name + `
spec:
  shards: 1
  replicasPerShard: 1
  exporter:
    enabled: false
  config:
    maxmemory-policy: noeviction
  acl:
    users:
      - name: default
        password:
          type: Any
        permissions: "+@all ~* &*"
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      memory: 256Mi
`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			cluster, err := utils.GetValkeyClusterStatus(name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(nodes(g)).To(HaveLen(2))
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("pausing the cluster and waiting for propagation to both nodes")
		kubectl(Default, "annotate", "valkeycluster", name, annotation+"=true")
		Eventually(func(g Gomega) {
			cluster, err := utils.GetValkeyClusterStatus(name)
			g.Expect(err).NotTo(HaveOccurred())
			condition := utils.FindCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused)
			g.Expect(condition).NotTo(BeNil())
			g.Expect(string(condition.Status)).To(Equal("True"))
			for _, node := range nodes(g) {
				g.Expect(node.Annotations).To(HaveKeyWithValue(annotation, "true"))
			}
		}).Should(Succeed())
		before := snapshot(Default)

		By("changing topology, live config and ACL while paused")
		kubectl(Default, "patch", "valkeycluster", name, "--type=merge", "-p",
			`{"spec":{"shards":2,"config":{"maxmemory-policy":"allkeys-lru"},"acl":{"users":[{"name":"default","password":{"type":"Any"},"permissions":"+@all ~* &*"},{"name":"app","password":{"type":"Any"},"commands":{"allow":["ping"]}}]}}}`)
		Consistently(func(g Gomega) {
			g.Expect(nodes(g)).To(HaveLen(2))
			g.Expect(snapshot(g)).To(Equal(before))
			g.Expect(cli(g, "CONFIG", "GET", "maxmemory-policy")).To(ContainSubstring("noeviction"))
			g.Expect(cli(g, "ACL", "GETUSER", "app")).To(BeEmpty())
		}, 35*time.Second, 5*time.Second).Should(Succeed())

		By("resuming and applying the pending changes")
		kubectl(Default, "annotate", "valkeycluster", name, annotation+"-")
		Eventually(func(g Gomega) {
			cluster, err := utils.GetValkeyClusterStatus(name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cluster.Status.ReadyShards).To(Equal(int32(2)))
			g.Expect(utils.FindCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused)).To(BeNil())
			current := nodes(g)
			g.Expect(current).To(HaveLen(4))
			for _, node := range current {
				g.Expect(node.Annotations).NotTo(HaveKey(annotation))
			}
			g.Expect(cli(g, "CONFIG", "GET", "maxmemory-policy")).To(ContainSubstring("allkeys-lru"))
			g.Expect(cli(g, "ACL", "GETUSER", "app")).NotTo(BeEmpty())
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
	})
})
