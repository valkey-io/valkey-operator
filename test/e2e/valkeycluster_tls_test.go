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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
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
  commonName: valkey-%s.default.svc.cluster.local
  dnsNames:
    - valkey-%s.default.svc.cluster.local
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
			clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", valkeyClusterName)

			utils.Run(exec.Command("kubectl", "delete", "pod", "client-tls", "--ignore-not-found=true", "--wait=true", "--timeout=30s"))
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
			}).Should(Succeed())

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
			utils.Run(exec.Command("kubectl", "delete", "pod", curlPodName, "--ignore-not-found=true", "--wait=true", "--timeout=30s"))
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
			}).Should(Succeed())
		})
	})
})

var _ = Describe("ValkeyCluster mTLS with CN-based ACL mapping", Ordered, Label("ValkeyCluster", "TLS", "mTLS"), func() {
	const (
		clusterName       = "cluster-mtls"
		customIssuer      = "valkey-mtls-custom-issuer"
		caCertName        = "valkey-mtls-ca"
		caIssuerName      = "valkey-mtls-ca-issuer"
		serverCertSecret  = "valkey-mtls-server-cert"
		clientCertSecret  = "valkey-mtls-client-alice"
		mtlsClientPodName = "client-mtls"
		mtlsNoCertPodName = "client-mtls-noauth"
	)
	var tmpDir string

	BeforeAll(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "valkey-mtls-test")
		Expect(err).NotTo(HaveOccurred())

		// Use a CA Issuer pattern so server + client certs share a trust
		// root: a self-signed custom Issuer mints a CA Certificate, and
		// the CA Issuer (referencing that CA secret) signs both leaves.
		// Without this, mTLS verification fails because each cert-manager
		// `selfSigned` Certificate becomes its own root.
		By("creating custom Issuer, root CA Certificate, CA Issuer, server Certificate, and client Certificate (CN=alice)")
		manifest := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
spec:
  secretName: %s
  isCA: true
  commonName: valkey-mtls-test-ca
  issuerRef:
    name: %s
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
spec:
  ca:
    secretName: %s
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
spec:
  secretName: %s
  commonName: valkey-%s.default.svc.cluster.local
  dnsNames:
    - valkey-%s.default.svc.cluster.local
    - localhost
  issuerRef:
    name: %s
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
spec:
  secretName: %s
  commonName: alice
  issuerRef:
    name: %s
    kind: Issuer
    group: cert-manager.io
`, customIssuer,
			caCertName, caCertName, customIssuer,
			caIssuerName, caCertName,
			serverCertSecret, serverCertSecret, clusterName, clusterName, caIssuerName,
			clientCertSecret, clientCertSecret, caIssuerName)
		manifestFile := filepath.Join(tmpDir, "mtls-cert-manager.yaml")
		Expect(os.WriteFile(manifestFile, []byte(manifest), 0644)).To(Succeed())

		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "apply", "-f", manifestFile))
			return err
		}).Should(Succeed())

		By("waiting for both leaf certificate Secrets to be created")
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "get", "secret", serverCertSecret))
			return err
		}).Should(Succeed())
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "get", "secret", clientCertSecret))
			return err
		}).Should(Succeed())

		By("creating the ValkeyCluster with mTLS + a passwordless 'alice' ACL user")
		clusterManifest := fmt.Sprintf(`
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
    authClients: "yes"
    authClientsUser: CN
  users:
    - name: alice
      enabled: true
      resetpass: true
      permissions: "+@all ~* &*"
`, clusterName, serverCertSecret)
		clusterFile := filepath.Join(tmpDir, "valkeycluster-mtls.yaml")
		Expect(os.WriteFile(clusterFile, []byte(clusterManifest), 0644)).To(Succeed())

		_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true"))
		_, err = utils.Run(exec.Command("kubectl", "apply", "-f", clusterFile))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the cluster to be ready")
		verifyReady := func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		}
		Eventually(verifyReady).Should(Succeed())
	})

	AfterAll(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "certificate", serverCertSecret, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "certificate", clientCertSecret, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "certificate", caCertName, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "issuer", caIssuerName, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "issuer", customIssuer, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", serverCertSecret, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", clientCertSecret, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", caCertName, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", mtlsClientPodName, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", mtlsNoCertPodName, "--ignore-not-found=true"))
		os.RemoveAll(tmpDir)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo("default")
			utils.CollectDebugInfo(namespace)
		}
	})

	It("renders the mTLS directives into valkey.conf", func() {
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "configmap",
				fmt.Sprintf("valkey-%s", clusterName),
				"-o", "jsonpath={.data.valkey\\.conf}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("tls-auth-clients yes"))
			g.Expect(out).To(ContainSubstring("tls-auth-clients-user CN"))
		}).Should(Succeed())
	})

	It("authenticates a client with CN=alice as the matching ACL user", func() {
		clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", clusterName)

		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", mtlsClientPodName, "--ignore-not-found=true"))
		cmd := exec.Command("kubectl", "run", mtlsClientPodName,
			fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--overrides",
			fmt.Sprintf(`{
				"spec": {
					"containers": [{
						"name": "client",
						"image": "%s",
						"command": ["valkey-cli", "-h", "%s", "--tls", "--cert", "/client-tls/tls.crt", "--key", "/client-tls/tls.key", "--cacert", "/server-tls/ca.crt", "ACL", "WHOAMI"],
						"volumeMounts": [
							{"name": "server-tls", "mountPath": "/server-tls", "readOnly": true},
							{"name": "client-tls", "mountPath": "/client-tls", "readOnly": true}
						]
					}],
					"volumes": [
						{"name": "server-tls", "secret": {"secretName": "%s"}},
						{"name": "client-tls", "secret": {"secretName": "%s"}}
					]
				}
			}`, valkeyClientImage, clusterFqdn, serverCertSecret, clientCertSecret))
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.Run(exec.Command("kubectl", "wait", fmt.Sprintf("pod/%s", mtlsClientPodName),
			"--for=jsonpath={.status.phase}=Succeeded", "--timeout=120s"))
		Expect(err).NotTo(HaveOccurred())

		out, err := utils.Run(exec.Command("kubectl", "logs", mtlsClientPodName))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("alice"),
			"ACL WHOAMI should resolve to the user mapped from the certificate's CN")

		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", mtlsClientPodName, "--ignore-not-found=true"))
	})

	// Verifies that primary <-> replica replication works under authClients=yes
	It("replicates data from primary to replica when mTLS authClients is enabled", func() {
		// get the primary pod by querying ValkeyNode status.role == "primary".
		var primaryPod string
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "valkeynode",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
				"-o", `jsonpath={range .items[?(@.status.role=="primary")]}{.status.podName}{"\n"}{end}`))
			g.Expect(err).NotTo(HaveOccurred())
			pods := strings.Fields(strings.TrimSpace(out))
			g.Expect(pods).To(HaveLen(1), "expected exactly 1 ValkeyNode with status.role=primary, got: %v", pods)
			primaryPod = pods[0]
		}).Should(Succeed())

		// Write a key directly on the primary.
		key := fmt.Sprintf("repl-mtls-%d", time.Now().UnixNano())
		_, err := utils.Run(exec.Command("kubectl", "exec", primaryPod, "-c", "server", "--",
			"valkey-cli", "--tls",
			"--cert", "/tls/tls.crt",
			"--key", "/tls/tls.key",
			"--cacert", "/tls/ca.crt",
			"SET", key, "hello"))
		Expect(err).NotTo(HaveOccurred())

		// Verify replication: WAIT 1 <timeout_ms> blocks until at least 1 replica
		// has acknowledged all pending writes, or the timeout expires.
		// A return value of "1" on the first line proves the key reached the replica.
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "exec", primaryPod, "-c", "server", "--",
				"valkey-cli", "--tls",
				"--cert", "/tls/tls.crt",
				"--key", "/tls/tls.key",
				"--cacert", "/tls/ca.crt",
				"WAIT", "1", "5000"))
			g.Expect(err).NotTo(HaveOccurred())
			firstLine := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
			g.Expect(strings.TrimSpace(firstLine)).To(Equal("1"), "expected WAIT to return 1 (replica acknowledged)")
		}).Should(Succeed())

		// Cleanup.
		_, _ = utils.Run(exec.Command("kubectl", "exec", primaryPod, "-c", "server", "--",
			"valkey-cli", "--tls",
			"--cert", "/tls/tls.crt",
			"--key", "/tls/tls.key",
			"--cacert", "/tls/ca.crt",
			"DEL", key))
	})

	It("rejects a client that does not present a certificate", func() {
		clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", clusterName)

		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", mtlsNoCertPodName, "--ignore-not-found=true"))
		cmd := exec.Command("kubectl", "run", mtlsNoCertPodName,
			fmt.Sprintf("--image=%s", valkeyClientImage), "--restart=Never", "--overrides",
			fmt.Sprintf(`{
				"spec": {
					"containers": [{
						"name": "client",
						"image": "%s",
						"command": ["valkey-cli", "-h", "%s", "--tls", "--cacert", "/tls/ca.crt", "PING"],
						"volumeMounts": [{"name": "tls-vol", "mountPath": "/tls", "readOnly": true}]
					}],
					"volumes": [{"name": "tls-vol", "secret": {"secretName": "%s"}}]
				}
			}`, valkeyClientImage, clusterFqdn, serverCertSecret))
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.Run(exec.Command("kubectl", "wait", fmt.Sprintf("pod/%s", mtlsNoCertPodName),
			"--for=jsonpath={.status.phase}=Failed", "--timeout=120s"))
		Expect(err).NotTo(HaveOccurred(),
			"client without a certificate should fail under authClients=yes")

		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", mtlsNoCertPodName, "--ignore-not-found=true"))
	})
})
