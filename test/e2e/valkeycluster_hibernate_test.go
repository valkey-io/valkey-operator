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

// Hibernation stops every pod of a persistent cluster and keeps its PVCs;
// waking it brings back the data every node saved on the way down.
var _ = Describe("ValkeyCluster hibernation", Ordered, Label("ValkeyCluster", "Persistence", "Hibernation"), func() {
	const clusterName = "cluster-hibernate"
	const keys = 200

	podSelector := fmt.Sprintf("valkey.io/cluster=%s", clusterName)

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	It("hibernates with every pod stopped and wakes with every key", func() {
		By("creating the persistent cluster")
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName,
			"--ignore-not-found=true", "--wait=true", "--timeout=120s")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to remove a leftover ValkeyCluster CR")
		cmd = exec.Command("kubectl", "delete", "pvc",
			"-l", podSelector, "--ignore-not-found=true", "--wait=true", "--timeout=120s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to remove leftover PVCs")

		manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  acl:
    users:
      - name: default
        password:
          type: Any
        permissions: "~* &* +@all"
  shards: 3
  replicasPerShard: 1
  persistence:
    size: 1Gi
`, clusterName)
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the ValkeyCluster CR")

		verifyReady := func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(3)))
		}
		Eventually(verifyReady, 5*time.Minute).Should(Succeed())

		valkeyCli := func(script string) (string, error) {
			cmd := exec.Command("kubectl", "exec", fmt.Sprintf("valkey-%s-0-0-0", clusterName), "-c", "server", "--",
				"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; "+script)
			return utils.Run(cmd)
		}

		By("writing keys across every shard")
		Eventually(func(g Gomega) {
			_, err := valkeyCli(fmt.Sprintf(
				`for i in $(seq 1 %d); do valkey-cli -c set hib:$i v$i | grep -q OK || exit 1; done`, keys))
			g.Expect(err).NotTo(HaveOccurred())
		}).Should(Succeed())

		By("hibernating the cluster")
		cmd = exec.Command("kubectl", "annotate", "valkeycluster", clusterName,
			valkeyiov1alpha1.AnnotationHibernate+"=true", "--overwrite")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateHibernated))
			cond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonHibernated))
		}, 5*time.Minute).Should(Succeed())

		By("verifying every pod is gone and every PVC is kept")
		cmd = exec.Command("kubectl", "get", "pod", "-l", podSelector, "-o", "name")
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.GetNonEmptyLines(output)).To(BeEmpty())
		cmd = exec.Command("kubectl", "get", "pvc", "-l", podSelector, "-o", "name")
		output, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.GetNonEmptyLines(output)).To(HaveLen(6))
		cmd = exec.Command("kubectl", "get", "statefulset", "-l", podSelector,
			"-o", "jsonpath={range .items[*]}{.spec.replicas}{\"\\n\"}{end}")
		output, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.GetNonEmptyLines(output)).To(HaveLen(6))
		for _, replicas := range utils.GetNonEmptyLines(output) {
			Expect(replicas).To(Equal("0"))
		}

		By("verifying each primary recorded the data its replica saved")
		cmd = exec.Command("kubectl", "get", "valkeynode", "-l", podSelector,
			"-o", `jsonpath={range .items[*]}{.metadata.annotations.valkey\.io/hibernate-data-point}{"\n"}{end}`)
		output, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.GetNonEmptyLines(output)).To(HaveLen(3), "one data point per primary")

		By("waking the cluster")
		cmd = exec.Command("kubectl", "annotate", "valkeycluster", clusterName, valkeyiov1alpha1.AnnotationHibernate+"-")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			verifyReady(g)
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionHibernated)).To(BeNil())
		}, 5*time.Minute).Should(Succeed())

		By("verifying every key survived")
		Eventually(func(g Gomega) {
			output, err := valkeyCli(fmt.Sprintf(
				`n=0; for i in $(seq 1 %d); do [ "$(valkey-cli -c get hib:$i)" = "v$i" ] && n=$((n+1)); done; echo $n`, keys))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal(strconv.Itoa(keys)))
		}, 2*time.Minute).Should(Succeed())

		By("verifying the ValkeyNodes are unmarked")
		cmd = exec.Command("kubectl", "get", "valkeynode", "-l", podSelector,
			"-o", `jsonpath={range .items[*]}{.metadata.annotations.valkey\.io/hibernate}{.metadata.annotations.valkey\.io/hibernate-data-point}{"\n"}{end}`)
		output, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.GetNonEmptyLines(output)).To(BeEmpty())
	})

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName,
			"--ignore-not-found=true", "--wait=true", "--timeout=120s")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete the ValkeyCluster during teardown")
		cmd = exec.Command("kubectl", "delete", "pvc",
			"-l", podSelector, "--ignore-not-found=true", "--wait=true", "--timeout=120s")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete the cluster's PVCs during teardown")
	})
})
