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
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"valkey.io/valkey-operator/test/utils"
)

// applyGatewayManifest writes a manifest to a temp file and applies it with kubectl.
func applyGatewayManifest(manifest, desc string) error {
	f := filepath.Join(GinkgoT().TempDir(), "manifest.yaml")
	if err := os.WriteFile(f, []byte(manifest), 0644); err != nil {
		return err
	}
	_, err := utils.Run(exec.Command("kubectl", "apply", "-f", f))
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "failed to apply %s: %v\n", desc, err)
	}
	return err
}

// gatewayAPIExperimentalCRDs is the experimental-channel CRD bundle; TCPRoute is
// an experimental (v1alpha2) kind, so the experimental bundle is required.
const gatewayAPIExperimentalCRDs = "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/experimental-install.yaml"

var _ = Describe("ValkeyCluster Gateway API exposure", Ordered, Label("ValkeyCluster", "GatewayAPI"), func() {
	const clusterName = "cluster-gateway-sample"
	const gatewayName = "valkey-gateway"

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
		}
	})

	AfterAll(func() {
		utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true"))
		utils.Run(exec.Command("kubectl", "delete", "gateway", gatewayName, "--ignore-not-found=true"))
	})

	It("installs the Gateway API CRDs", func() {
		_, err := utils.Run(exec.Command("kubectl", "apply", "-f", gatewayAPIExperimentalCRDs))
		Expect(err).NotTo(HaveOccurred(), "Failed to install Gateway API CRDs")

		By("waiting for the TCPRoute CRD to be established")
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "wait", "--for=condition=Established",
				"crd/tcproutes.gateway.networking.k8s.io", "--timeout=60s"))
			return err
		}).Should(Succeed())
	})

	It("creates one TCPRoute per node attached to the Gateway", func() {
		By("restarting the operator so it re-runs Gateway API discovery")
		_, err := utils.Run(exec.Command("kubectl", "rollout", "restart",
			"deployment/valkey-operator-controller-manager", "-n", namespace))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "rollout", "status",
			"deployment/valkey-operator-controller-manager", "-n", namespace, "--timeout=2m"))
		Expect(err).NotTo(HaveOccurred())

		By("creating a Gateway the operator's TCPRoutes can attach to")
		gatewayManifest := fmt.Sprintf(`
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: %s
spec:
  gatewayClassName: valkey-e2e
  listeners:
    - name: shard-0-0
      protocol: TCP
      port: 30000
      allowedRoutes:
        kinds:
          - kind: TCPRoute
    - name: shard-0-1
      protocol: TCP
      port: 30001
      allowedRoutes:
        kinds:
          - kind: TCPRoute
`, gatewayName)
		Expect(applyGatewayManifest(gatewayManifest, "Gateway")).To(Succeed())

		By("creating a ValkeyCluster exposed through the Gateway")
		clusterManifest := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 1
  externalAccess:
    enabled: true
    domain: valkey.example.com
    preferredEndpointType: hostname
    gateway:
      gatewayRef:
        name: %s
      basePort: 30000
`, clusterName, gatewayName)
		Expect(applyGatewayManifest(clusterManifest, "ValkeyCluster")).To(Succeed())

		By("verifying a TCPRoute exists for each node, backed by the shard Service")
		verifyRoutes := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tcproutes",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
				"-o", "jsonpath={range .items[*]}{.metadata.name}:{.spec.rules[0].backendRefs[0].name}:{.spec.rules[0].backendRefs[0].port}{\"\\n\"}{end}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			lines := utils.GetNonEmptyLines(out)
			// 1 shard x (1 primary + 1 replica) = 2 routes.
			g.Expect(lines).To(ConsistOf(
				"valkey-cluster-gateway-sample-0-0:valkey-cluster-gateway-sample-shard-0:6379",
				"valkey-cluster-gateway-sample-0-1:valkey-cluster-gateway-sample-shard-0:6380",
			))
		}
		Eventually(verifyRoutes, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying the backing Service defaulted to ClusterIP")
		verifyServiceType := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "svc",
				"valkey-cluster-gateway-sample-shard-0",
				"-o", "jsonpath={.spec.type}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("ClusterIP"))
		}
		Eventually(verifyServiceType).Should(Succeed())
	})

	It("deletes the TCPRoutes when Gateway exposure is removed", func() {
		patch := `[{"op":"remove","path":"/spec/externalAccess/gateway"}]`
		_, err := utils.Run(exec.Command("kubectl", "patch", "valkeycluster", clusterName,
			"--type=json", "-p", patch))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "tcproutes",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
				"-o", "jsonpath={.items}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("[]"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})
})
