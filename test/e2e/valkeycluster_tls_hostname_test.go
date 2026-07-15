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
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/test/utils"
)

// ValkeyCluster TLS with preferredEndpointType=Hostname exercises the
// rediscovery path that breaks with announce-by-IP + DNS-only certs:
// multi-shard CLUSTER SLOTS, then re-dial each announced endpoint under TLS.
var _ = Describe("ValkeyCluster TLS Hostname endpoints", Ordered, Label("ValkeyCluster", "TLS", "Networking"), func() {
	var valkeyClusterName string
	var tlsSecretName string
	var headlessSvc string
	var tmpDir string

	BeforeAll(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "valkey-tls-hostname-test")
		Expect(err).NotTo(HaveOccurred())

		valkeyClusterName = "cluster-tls-host"
		tlsSecretName = "valkey-tls-hostname-cert"
		headlessSvc = "valkey-" + valkeyClusterName
		// Pod FQDNs: <pod>.valkey-cluster-tls-host.default.svc.cluster.local
		wildcardSAN := fmt.Sprintf("*.%s.default.svc.cluster.local", headlessSvc)
		serviceSAN := fmt.Sprintf("%s.default.svc.cluster.local", headlessSvc)

		By("creating Cert-Manager resources with wildcard SAN for pod hostnames")
		cmManifest := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned-issuer-hostname
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
spec:
  secretName: %s
  commonName: %s
  dnsNames:
    - %s
    - %s
    - localhost
  issuerRef:
    name: selfsigned-issuer-hostname
    kind: Issuer
    group: cert-manager.io
`, tlsSecretName, tlsSecretName, serviceSAN, serviceSAN, wildcardSAN)
		cmManifestFile := filepath.Join(tmpDir, "cert-manager-resources.yaml")
		err = os.WriteFile(cmManifestFile, []byte(cmManifest), 0644)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "apply", "-f", cmManifestFile))
			return err
		}).Should(Succeed())

		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "get", "secret", tlsSecretName))
			return err
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("creating multi-shard ValkeyCluster with TLS and Hostname announcement")
		manifest := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 3
  replicas: 0
  tls:
    certificate:
      secretName: %s
  networking:
    preferredEndpointType: Hostname
    clusterDomain: cluster.local
`, valkeyClusterName, tlsSecretName)
		manifestFile := filepath.Join(tmpDir, "valkeycluster-tls-hostname.yaml")
		err = os.WriteFile(manifestFile, []byte(manifest), 0644)
		Expect(err).NotTo(HaveOccurred())

		utils.Run(exec.Command("kubectl", "delete", "valkeycluster", valkeyClusterName, "--ignore-not-found=true"))
		_, err = utils.Run(exec.Command("kubectl", "apply", "-f", manifestFile))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the cluster to be ready")
		Eventually(func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		utils.Run(exec.Command("kubectl", "delete", "valkeycluster", valkeyClusterName, "--ignore-not-found=true"))
		utils.Run(exec.Command("kubectl", "delete", "issuer", "selfsigned-issuer-hostname", "--ignore-not-found=true"))
		utils.Run(exec.Command("kubectl", "delete", "certificate", tlsSecretName, "--ignore-not-found=true"))
		utils.Run(exec.Command("kubectl", "delete", "secret", tlsSecretName, "--ignore-not-found=true"))
		os.RemoveAll(tmpDir)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo("default")
			utils.CollectDebugInfo(namespace)
		}
	})

	It("sets pod subdomain to the headless Service for DNS records", func() {
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
				"-o", "jsonpath={.items[0].spec.subdomain}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal(headlessSvc))
		}).Should(Succeed())
	})

	It("announces hostnames in CLUSTER SLOTS, not pod IPs", func() {
		// With preferredEndpointType=Hostname, rediscovery endpoints must be
		// DNS names that match the cert SANs. IP announcement is what breaks
		// strict TLS clients after CLUSTER SLOTS.
		fqdnSuffix := fmt.Sprintf(".%s.default.svc.cluster.local", headlessSvc)
		ipv4 := regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)

		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
				"-o", "jsonpath={.items[0].metadata.name}")
			podName, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podName).NotTo(BeEmpty())

			cmd = exec.Command("kubectl", "exec", podName, "-c", "server", "--",
				"valkey-cli", "--tls", "--cacert", "/tls/ca.crt", "--cert", "/tls/tls.crt", "--key", "/tls/tls.key",
				"CLUSTER", "SLOTS")
			slotsOut, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "CLUSTER SLOTS failed: %s", slotsOut)

			// Collect host fields: after each port line in CLUSTER SLOTS the host is a bulk string.
			// Simpler: require at least one pod FQDN and no primary host that is a bare IPv4.
			g.Expect(slotsOut).To(ContainSubstring(fqdnSuffix),
				"CLUSTER SLOTS should announce pod FQDNs ending in %s\noutput:\n%s", fqdnSuffix, slotsOut)

			// Every Ready pod name should appear as hostname prefix.
			cmd = exec.Command("kubectl", "get", "pods",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
				"-o", "jsonpath={range .items[*]}{.metadata.name}{'\\n'}{end}")
			podsOut, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			for _, name := range strings.Split(strings.TrimSpace(podsOut), "\n") {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				g.Expect(slotsOut).To(ContainSubstring(name+fqdnSuffix),
					"expected announced hostname for pod %s\nSLOTS:\n%s", name, slotsOut)
			}

			// Guard against still advertising pod IPs as the preferred endpoint.
			// CLUSTER SLOTS lines that look like bare IPv4 must not be present.
			for _, line := range strings.Split(slotsOut, "\n") {
				line = strings.TrimSpace(line)
				if ipv4.MatchString(line) {
					// Allow only if that IP is not a cluster endpoint host: hostnames mode
					// should not list IPs as the endpoint identity. Fail hard on any IPv4 line.
					g.Expect(line).NotTo(MatchRegexp(`^\d{1,3}(\.\d{1,3}){3}$`),
						"CLUSTER SLOTS still lists pod IP %q; Hostname mode should announce FQDNs\nfull:\n%s",
						line, slotsOut)
				}
			}
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("supports multi-shard TLS rediscovery via the Service DNS name", func() {
		// Multi-shard + cluster mode client: after topology refresh the client
		// re-dials each announced host. With Hostname mode those hosts are FQDNs
		// covered by the wildcard SAN, so strict TLS rediscovery succeeds.
		clusterFqdn := fmt.Sprintf("%s.default.svc.cluster.local", headlessSvc)
		clientPod := "client-tls-hostname"

		// Keys that hash across the slot space (3 shards).
		keys := []string{
			"tls-host-a", "tls-host-b", "tls-host-c", "tls-host-d", "tls-host-e",
			"tls-host-f", "tls-host-g", "tls-host-h", "tls-host-i", "tls-host-j",
			"alpha", "beta", "gamma", "delta", "epsilon",
		}

		By("writing keys across slots with valkey-cli -c over TLS")
		Eventually(func(g Gomega) {
			utils.Run(exec.Command("kubectl", "delete", "pod", clientPod, "--ignore-not-found=true", "--wait=true", "--timeout=30s"))

			// Build a shell one-liner that SETs each key; fail if any non-OK response.
			var setCmds []string
			for _, k := range keys {
				setCmds = append(setCmds, fmt.Sprintf(
					`out=$(valkey-cli -c -h %s --tls --cert /tls/tls.crt --key /tls/tls.key --cacert /tls/ca.crt SET %s v 2>&1) && echo "%s:$out" && echo "$out" | grep -q '^OK$'`,
					clusterFqdn, k, k,
				))
			}
			script := strings.Join(setCmds, " && ")

			cmd := exec.Command("kubectl", "run", clientPod,
				fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "client-tls-hostname",
							"image": "%s",
							"command": ["sh", "-c", %q],
							"volumeMounts": [{
								"name": "tls-vol",
								"mountPath": "/tls",
								"readOnly": true
							}]
						}],
						"volumes": [{
							"name": "tls-vol",
							"secret": {
								"secretName": "%s"
							}
						}]
					}
				}`, valkeyClientImage, script, tlsSecretName))
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "wait", "pod/"+clientPod, "--for=jsonpath={.status.phase}=Succeeded", "--timeout=120s")
			_, err = utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "logs", clientPod)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			for _, k := range keys {
				g.Expect(output).To(ContainSubstring(k+":OK"), "key %s failed:\n%s", k, output)
			}
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		utils.Run(exec.Command("kubectl", "delete", "pod", clientPod, "--ignore-not-found=true"))
	})
})
