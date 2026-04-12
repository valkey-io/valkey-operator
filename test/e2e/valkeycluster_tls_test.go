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

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/test/utils"
)

var _ = Describe("ValkeyCluster TLS", Ordered, Label("ValkeyCluster", "TLS"), func() {
	var valkeyClusterName string
	var tmpDir string

	BeforeAll(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "valkey-tls-test")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		os.RemoveAll(tmpDir)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo("default")
			utils.CollectDebugInfo(namespace)
		}
	})

	Context("when a ValkeyCluster CR with TLS is applied", func() {
		It("creates a Valkey Cluster with TLS enabled", func() {
			valkeyClusterName = "cluster-tls"
			tlsSecretName := "valkey-tls-cert"
			var err error

			By("creating Cert-Manager resources")
			cmManifest := `
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
  name: valkey-tls-cert
spec:
  secretName: valkey-tls-cert
  commonName: cluster-tls.default.svc.cluster.local
  dnsNames:
    - cluster-tls.default.svc.cluster.local
    - localhost
  issuerRef:
    name: selfsigned-issuer
    kind: Issuer
    group: cert-manager.io
`
			cmManifestFile := filepath.Join(tmpDir, "cert-manager-resources.yaml")
			err = os.WriteFile(cmManifestFile, []byte(cmManifest), 0644)
			Expect(err).NotTo(HaveOccurred())

			utils.Run(exec.Command("kubectl", "apply", "-f", cmManifestFile))

			By("waiting for the certificate secret to be created")
			Eventually(func() error {
				_, err := utils.Run(exec.Command("kubectl", "get", "secret", tlsSecretName))
				return err
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("creating the ValkeyCluster CR with TLS")
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

			By("validating pod mounts")
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", valkeyClusterName),
				"-o", "jsonpath={.items[0].spec.containers[?(@.name=='server')].volumeMounts[?(@.name=='tls-certs')].mountPath}",
			)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("/tls"))

			By("validating cluster access via TLS")
			clusterFqdn := fmt.Sprintf("%s.default.svc.cluster.local", valkeyClusterName)

			By("running a client pod to set a key")
			exec.Command("kubectl", "delete", "pod", "client-tls", "--ignore-not-found=true").Run()
			cmd = exec.Command("kubectl", "run", "client-tls",
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
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "wait", "pod/client-tls",
				"--for=jsonpath={.status.phase}=Succeeded", "--timeout=60s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "logs", "client-tls")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("OK"))

			// Clean up client pod
			utils.Run(exec.Command("kubectl", "delete", "pod", "client-tls", "--ignore-not-found=true"))
		})
	})
})
