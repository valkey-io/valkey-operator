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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster TLS", Ordered, Label("ValkeyCluster", "TLS"), func() {
	var valkeyClusterName string
	var tlsSecretName string
	var tmpDir string

	BeforeAll(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "valkey-tls-test")
		Expect(err).NotTo(HaveOccurred())

		valkeyClusterName = "cluster-tls"
		tlsSecretName = "valkey-tls-cert"

		By("creating Cert-Manager resources")
		cmManifest := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned-issuer
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
spec:
  secretName: %s
  commonName: %s.default.svc.cluster.local
  dnsNames:
    - %s.default.svc.cluster.local
    - localhost
  issuerRef:
    name: selfsigned-issuer
    kind: Issuer
    group: cert-manager.io
`, tlsSecretName, tlsSecretName, valkeyClusterName, valkeyClusterName)
		cmManifestFile := filepath.Join(tmpDir, "cert-manager-resources.yaml")
		err = os.WriteFile(cmManifestFile, []byte(cmManifest), 0644)
		Expect(err).NotTo(HaveOccurred())

		By("applying Cert-Manager Issuer and Certificate (retry until webhook is serving)")
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "apply", "-f", cmManifestFile))
			return err
		}).Should(Succeed())

		By("waiting for the certificate secret to be created")
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "get", "secret", tlsSecretName))
			return err
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("creating the ValkeyCluster CR with TLS and metrics exporter")
		manifest := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: 1
  replicas: 1
  tls:
    certificate:
      secretName: %s
  exporter:
    enabled: true
`, valkeyClusterName, tlsSecretName)
		manifestFile := filepath.Join(tmpDir, "valkeycluster-tls.yaml")
		err = os.WriteFile(manifestFile, []byte(manifest), 0644)
		Expect(err).NotTo(HaveOccurred())

		utils.Run(exec.Command("kubectl", "delete", "valkeycluster", valkeyClusterName, "--ignore-not-found=true"))
		_, err = utils.Run(exec.Command("kubectl", "apply", "-f", manifestFile))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the cluster to be ready")
		verifyReady := func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(valkeyClusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		}
		Eventually(verifyReady, 10*time.Minute, 5*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		utils.Run(exec.Command("kubectl", "delete", "valkeycluster", valkeyClusterName, "--ignore-not-found=true"))
		utils.Run(exec.Command("kubectl", "delete", "issuer", "selfsigned-issuer", "--ignore-not-found=true"))
		utils.Run(exec.Command("kubectl", "delete", "certificate", "valkey-tls-cert", "--ignore-not-found=true"))
		os.RemoveAll(tmpDir)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo("default")
			utils.CollectDebugInfo(namespace)
		}
	})

	Context("when a Valkeycluster CR with TLS is applied", func() {
		It("mounts TLS certs into the server container", func() {
			cmd := exec.Command("kubectl", "get", "pod",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
				"-o", "jsonpath={.items[0].spec.containers[?(@.name=='server')].volumeMounts[?(@.name=='tls-certs')].mountPath}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("/tls"))
		})

		It("allows cluster access via TLS", func() {
			clusterFqdn := fmt.Sprintf("%s.default.svc.cluster.local", valkeyClusterName)

			utils.Run(exec.Command("kubectl", "delete", "pod", "client-tls", "--ignore-not-found=true"))
			cmd := exec.Command("kubectl", "run", "client-tls",
				fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "client-tls",
							"image": "%s",
							"command": ["valkey-cli", "-c", "-h", "%s", "--tls", "--cert", "/tls/tls.crt", "--key", "/tls/tls.key", "--cacert", "/tls/ca.crt", "SET", "hello", "world"],
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
				}`, valkeyClientImage, clusterFqdn, tlsSecretName))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "wait", "pod/client-tls", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=60s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "logs", "client-tls")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("OK"))

			utils.Run(exec.Command("kubectl", "delete", "pod", "client-tls", "--ignore-not-found=true"))
		})

		It("deploys metrics exporter with TLS configuration", func() {
			By("verifying metrics-exporter container exists alongside server")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName), "-o", "jsonpath={.items[0].spec.containers[*].name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				containers := strings.Fields(out)
				g.Expect(containers).To(ContainElement("server"))
				g.Expect(containers).To(ContainElement("metrics-exporter"))
			}).Should(Succeed())

			By("verifying metrics-exporter uses rediss:// scheme in args")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName), "-o", "jsonpath={.items[0].spec.containers[?(@.name=='metrics-exporter')].args[0]}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("rediss://"), "exporter should use rediss:// when TLS is enabled")
			}).Should(Succeed())
		})

		It("exposes Valkey metrics via the exporter when TLS is enabled", func() {
			By("waiting for both containers to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName), "-o", "jsonpath={.items[0].status.containerStatuses[*].ready}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				for _, status := range strings.Fields(out) {
					g.Expect(status).To(Equal("true"))
				}
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("getting pod IP")
			var podIP string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName), "-o", "jsonpath={.items[0].status.podIP}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
				podIP = out
			}).Should(Succeed())

			By("running curl pod to verify metrics endpoint")
			curlPodName := "curl-tls-metrics"
			utils.Run(exec.Command("kubectl", "delete", "pod", curlPodName, "--ignore-not-found=true"))
			cmd := exec.Command("kubectl", "run", curlPodName, "--image=curlimages/curl:latest", "--restart=Never", "--overrides", `{
				"spec": {
					"containers": [{
						"name": "curl",
						"image": "curlimages/curl:latest",
						"command": ["/bin/sh", "-c"],
						"args": ["sleep 3600"],
						"securityContext": {
							"readOnlyRootFilesystem": true,
							"allowPrivilegeEscalation": false,
							"capabilities": {"drop": ["ALL"]},
							"runAsNonRoot": true,
							"runAsUser": 1000,
							"seccompProfile": {"type": "RuntimeDefault"}
						}
					}]
				}
			}`)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				utils.Run(exec.Command("kubectl", "delete", "pod", curlPodName, "--ignore-not-found=true", "--wait=false"))
			}()

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", curlPodName, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Running"))
			}).Should(Succeed())

			By("verifying redis_up=1 is reported via the metrics endpoint")
			Eventually(func(g Gomega) {
				url := fmt.Sprintf("http://%s:9121/metrics", podIP)
				cmd := exec.Command("kubectl", "exec", curlPodName, "--", "curl", "-s", url)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(MatchRegexp(`redis_up\s+1`), "redis_up should be 1 when exporter connects via TLS")
				g.Expect(out).To(ContainSubstring("redis_connected_clients"))
				g.Expect(out).To(ContainSubstring("redis_memory_used_bytes"))
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
})
