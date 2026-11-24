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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyNode", Label("valkeynode"), func() {
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	// A ValkeyNode created by hand has no ValkeyCluster parent, so nothing sets
	// spec.workloadRevision for it and the operator must not reconcile it.
	Context("without a parent", Label("valkeynode", "no-parent"), func() {
		const nodeName = "valkeynode-no-parent-e2e"

		It("reports NoParent and creates nothing", func() {
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyNode
metadata:
  name: %s
spec:
  workloadType: StatefulSet
  configRevision: "000000000000"
  sharedConfigMapName: valkey-unused
`, nodeName))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ValkeyNode %s", nodeName)
			defer func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeynode", nodeName, "--ignore-not-found=true", "--wait=false"))
			}()

			By("waiting for Ready=False with reason NoParent")
			Eventually(func(g Gomega) {
				node, err := utils.GetValkeyNodeStatus(nodeName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(node.Status.Ready).To(BeFalse())
				ready := utils.FindCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
				g.Expect(ready).NotTo(BeNil(), "Ready condition should be present")
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(ready.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonNoParent))
			}).Should(Succeed())

			By("verifying one NoParent Warning event is recorded")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "events",
					"--field-selector", fmt.Sprintf("reason=%s,involvedObject.name=%s", valkeyiov1alpha1.ValkeyNodeReasonNoParent, nodeName),
					"-o", "jsonpath={.items[*].type}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("Warning"))
			}).Should(Succeed())

			By("verifying the operator creates no StatefulSet or ConfigMap")
			Consistently(func(g Gomega) {
				for _, kind := range []string{"statefulset", "configmap"} {
					out, err := utils.Run(exec.Command("kubectl", "get", kind, "valkey-"+nodeName,
						"--ignore-not-found=true", "-o", "name"))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "%s valkey-%s must not exist", kind, nodeName)
				}
			}, 15*time.Second, 3*time.Second).Should(Succeed())
		})
	})
})
