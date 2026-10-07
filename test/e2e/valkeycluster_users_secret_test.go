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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

// A user whose password Secret is missing must not stop a scale-out. The
// nodes keep the last aclfile, the cluster reports Degraded with the user and
// the Secret, and the new shard still joins. Once the Secret appears the
// aclfile is rebuilt and Degraded clears.
var _ = Describe("ValkeyCluster users ACL", Ordered, Label("ValkeyCluster", "ACL"), func() {
	const (
		clusterName = "valkeycluster-users-secret"
		aliceSecret = "users-secret-alice-pw"
		bobSecret   = "users-secret-bob-pw"
	)

	manifest := func(shards int, users string) string {
		return fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: %d
  replicas: 0
  users:
%s`, clusterName, shards, users)
	}
	userBlock := func(name, secret string) string {
		return fmt.Sprintf(`    - name: %s
      enabled: true
      passwordSecret:
        name: %s
        keys: [current]
      commands:
        allow: ["@read", "@connection"]
`, name, secret)
	}
	apply := func(body string) {
		file := filepath.Join(os.TempDir(), clusterName+".yaml")
		Expect(os.WriteFile(file, []byte(body), 0644)).To(Succeed())
		defer os.Remove(file)
		_, err := utils.Run(exec.Command("kubectl", "apply", "-f", file))
		Expect(err).NotTo(HaveOccurred(), "Failed to apply ValkeyCluster CR")
	}
	clusterStatus := func(g Gomega) (*valkeyiov1alpha1.ValkeyCluster, *metav1.Condition) {
		cr, err := utils.GetValkeyClusterStatus(clusterName)
		g.Expect(err).NotTo(HaveOccurred())
		return cr, utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	}

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	AfterAll(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true", "--wait=false"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", aliceSecret, bobSecret, "--ignore-not-found=true"))
	})

	It("keeps scaling out while a user password Secret is missing", func() {
		By("creating a cluster with one user whose Secret exists")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", aliceSecret, bobSecret, "--ignore-not-found=true"))
		_, err := utils.Run(exec.Command("kubectl", "create", "secret", "generic", aliceSecret, "--from-literal=current=alice-current"))
		Expect(err).NotTo(HaveOccurred())
		apply(manifest(2, userBlock("alice", aliceSecret)))

		Eventually(func(g Gomega) {
			cr, _ := clusterStatus(g)
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(2)))
		}, 10*time.Minute).Should(Succeed())

		By("scaling to 3 shards while adding a user whose Secret does not exist")
		apply(manifest(3, userBlock("alice", aliceSecret)+userBlock("bob", bobSecret)))

		By("verifying the new shard joins and the failure is reported on Degraded")
		Eventually(func(g Gomega) {
			cr, degraded := clusterStatus(g)
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)), "the third shard must join while bob's Secret is missing")
			g.Expect(degraded).NotTo(BeNil(), "Degraded must report the unresolved user")
			g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(degraded.Reason).To(Equal(valkeyiov1alpha1.ReasonUsersACLUnresolved))
			g.Expect(degraded.Message).To(Equal(fmt.Sprintf("user bob: Secret %s not found", bobSecret)))
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateDegraded))
		}, 10*time.Minute).Should(Succeed())

		Eventually(func(g Gomega) {
			_, warnings, err := utils.GetEvents(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(warnings).To(HaveKey("UsersACLUnresolved"))
		}, 2*time.Minute).Should(Succeed())

		By("verifying alice can still authenticate on the new shard with the last aclfile")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "exec", "valkey-"+clusterName+"-2-0-0", "-c", "server", "--",
				"sh", "-c", "unset REDISCLI_AUTH; VALKEYCLI_AUTH=alice-current valkey-cli --user alice PING"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("PONG"))
		}, 2*time.Minute).Should(Succeed())

		By("creating bob's Secret and verifying Degraded clears")
		_, err = utils.Run(exec.Command("kubectl", "create", "secret", "generic", bobSecret, "--from-literal=current=bob-current"))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			cr, degraded := clusterStatus(g)
			g.Expect(degraded).To(BeNil(), "Degraded must clear once the Secret exists")
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		}, 5*time.Minute).Should(Succeed())

		Eventually(func(g Gomega) {
			normal, _, err := utils.GetEvents(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(normal).To(HaveKey("UsersACLResolved"))
		}, 2*time.Minute).Should(Succeed())

		By("verifying bob can authenticate once the aclfile is rebuilt")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "exec", "valkey-"+clusterName+"-2-0-0", "-c", "server", "--",
				"sh", "-c", "unset REDISCLI_AUTH; VALKEYCLI_AUTH=bob-current valkey-cli --user bob PING"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("PONG"))
		}, 5*time.Minute).Should(Succeed())
	})
})
