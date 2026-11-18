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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

// A cluster created by a release that named its Secrets internal-<cluster>-acl
// and internal-<cluster>-system-passwords. Only such a release renders pods on
// those names, so the test pauses the cluster and moves each node onto them by
// hand, one at a time, the way the older operator left them. Persistence keeps
// each node's identity through that restart.
var _ = Describe("ValkeyCluster on the internal- Secret names", Label("cluster-lifecycle", "secret-rename"), func() {
	It("moves onto the valkey- names, keeping the operator's password and rolling each node once", func() {
		const name = "valkeycluster-secret-rename-e2e"
		const pause = "valkey.io/pause-reconciliation"
		const selector = "valkey.io/cluster=" + name
		// Each Secret's name, mapped to the name the older release gave it.
		legacyNames := map[string]string{
			"valkey-" + name + "-acl":              "internal-" + name + "-acl",
			"valkey-" + name + "-system-passwords": "internal-" + name + "-system-passwords",
		}
		kubectl := func(g Gomega, stdin string, args ...string) string {
			cmd := exec.Command("kubectl", args...)
			if stdin != "" {
				cmd.Stdin = strings.NewReader(stdin)
			}
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(out)
		}
		secretExists := func(g Gomega, secret string) bool {
			out := kubectl(g, "", "get", "secret", secret, "--ignore-not-found", "-o", "name")
			return out != ""
		}
		nodes := func(g Gomega) []valkeyiov1alpha1.ValkeyNode {
			var list valkeyiov1alpha1.ValkeyNodeList
			out := kubectl(g, "", "get", "valkeynodes", "-l", selector, "-o", "json")
			g.Expect(json.Unmarshal([]byte(out), &list)).To(Succeed())
			return list.Items
		}
		statefulSet := func(g Gomega, node string) appsv1.StatefulSet {
			var sts appsv1.StatefulSet
			out := kubectl(g, "", "get", "statefulset", "valkey-"+node, "-o", "json")
			g.Expect(json.Unmarshal([]byte(out), &sts)).To(Succeed())
			return sts
		}
		pod := func(g Gomega, node string) corev1.Pod {
			var p corev1.Pod
			g.Expect(json.Unmarshal([]byte(kubectl(g, "", "get", "pod", "valkey-"+node+"-0", "-o", "json")), &p)).To(Succeed())
			return p
		}
		readsLegacyNames := func(g Gomega, spec any) bool {
			out, err := json.Marshal(spec)
			g.Expect(err).NotTo(HaveOccurred())
			return strings.Contains(string(out), `"internal-`+name+`-`)
		}
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", name, "--ignore-not-found=true", "--wait=false"))
			for _, legacy := range legacyNames {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", legacy, "--ignore-not-found=true"))
			}
		})

		By("creating a persistent cluster")
		kubectl(Default, `apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: `+name+`
spec:
  shards: 1
  replicasPerShard: 1
  persistence:
    size: 1Gi
    reclaimPolicy: Delete
  acl:
    users:
      - name: default
        password:
          type: Any
        permissions: "~* &* +@all"
`, "apply", "-f", "-")
		Eventually(func(g Gomega) {
			cluster, err := utils.GetValkeyClusterStatus(name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(nodes(g)).To(HaveLen(2))
		}, 10*time.Minute, 2*time.Second).Should(Succeed())
		operatorPassword := func(g Gomega) string {
			return kubectl(g, "", "get", "secret", "valkey-"+name+"-system-passwords", "-o", "jsonpath={.data._operator}")
		}
		before := operatorPassword(Default)
		Expect(before).NotTo(BeEmpty())

		By("pausing the cluster")
		kubectl(Default, "", "annotate", "valkeycluster", name, pause+"=true")
		Eventually(func(g Gomega) {
			cluster, err := utils.GetValkeyClusterStatus(name)
			g.Expect(err).NotTo(HaveOccurred())
			condition := utils.FindCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused)
			g.Expect(condition).NotTo(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			for _, node := range nodes(g) {
				g.Expect(node.Annotations).To(HaveKeyWithValue(pause, "true"))
			}
		}).Should(Succeed())

		By("copying each Secret to its legacy name, labelled and owned as the older release created it")
		for current, legacy := range legacyNames {
			var secret corev1.Secret
			Expect(json.Unmarshal([]byte(kubectl(Default, "", "get", "secret", current, "-o", "json")), &secret)).To(Succeed())
			secret.ObjectMeta = metav1.ObjectMeta{
				Name:            legacy,
				Namespace:       secret.Namespace,
				Labels:          secret.Labels,
				Annotations:     secret.Annotations,
				OwnerReferences: secret.OwnerReferences,
			}
			manifest, err := json.Marshal(secret)
			Expect(err).NotTo(HaveOccurred())
			kubectl(Default, string(manifest), "create", "-f", "-")
		}

		By("moving each node onto the legacy names, one at a time")
		for _, node := range nodes(Default) {
			kubectl(Default, "", "patch", "valkeynode", node.Name, "--type=merge",
				"-p", `{"spec":{"usersACLSecretName":"internal-`+name+`-acl"}}`)
			Eventually(func(g Gomega) {
				manifest := kubectl(g, "", "get", "statefulset", "valkey-"+node.Name, "-o", "json")
				for current, legacy := range legacyNames {
					manifest = strings.ReplaceAll(manifest, `"`+current+`"`, `"`+legacy+`"`)
				}
				kubectl(g, manifest, "replace", "-f", "-")
			}).Should(Succeed())
			kubectl(Default, "", "rollout", "status", "statefulset/valkey-"+node.Name, "--timeout=5m")
			Eventually(func(g Gomega) {
				p := pod(g, node.Name)
				g.Expect(readsLegacyNames(g, p.Spec)).To(BeTrue())
				g.Expect(p.Status.Conditions).To(ContainElement(SatisfyAll(
					HaveField("Type", corev1.PodReady), HaveField("Status", corev1.ConditionTrue))))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())
		}

		By("deleting the new Secrets, which nothing reads now")
		for current := range legacyNames {
			kubectl(Default, "", "delete", "secret", current)
		}
		generations := map[string]int64{}
		podUIDs := map[string]string{}
		for _, node := range nodes(Default) {
			generations[node.Name] = statefulSet(Default, node.Name).Generation
			podUIDs[node.Name] = string(pod(Default, node.Name).UID)
		}

		By("resuming reconciliation")
		kubectl(Default, "", "annotate", "valkeycluster", name, pause+"-")
		lostAccess := false
		Eventually(func(g Gomega) {
			cluster, err := utils.GetValkeyClusterStatus(name)
			g.Expect(err).NotTo(HaveOccurred())
			for _, condition := range cluster.Status.Conditions {
				if condition.Reason == valkeyiov1alpha1.ReasonOperatorAuthenticationFailed {
					lostAccess = true
				}
			}
			for current, legacy := range legacyNames {
				g.Expect(secretExists(g, current)).To(BeTrue(), current)
				g.Expect(secretExists(g, legacy)).To(BeFalse(), "%s is deleted once no node reads it", legacy)
			}
			g.Expect(operatorPassword(g)).To(Equal(before), "the passwords are copied, not generated")
			for _, node := range nodes(g) {
				sts := statefulSet(g, node.Name)
				g.Expect(sts.Generation).To(Equal(generations[node.Name]+1), "%s's template is updated once", node.Name)
				g.Expect(readsLegacyNames(g, sts.Spec.Template.Spec)).To(BeFalse())
				g.Expect(string(pod(g, node.Name).UID)).NotTo(Equal(podUIDs[node.Name]), "%s rolls", node.Name)
				applied := utils.FindCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionACLApplied)
				g.Expect(applied).NotTo(BeNil())
				g.Expect(applied.Status).To(Equal(metav1.ConditionTrue))
			}
			g.Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
		Expect(lostAccess).To(BeFalse(), "no node rejected the operator's password during the roll")

		By("checking the copy and the deletion were reported (best-effort)")
		Eventually(func(g Gomega) {
			normal, warnings, err := utils.GetEvents(name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(normal).To(HaveKey("LegacySecretCopied"))
			g.Expect(normal).To(HaveKey("LegacySecretDeleted"))
			g.Expect(warnings).NotTo(HaveKey("OperatorAuthenticationFailed"))
		}).Should(Succeed())
	})
})
