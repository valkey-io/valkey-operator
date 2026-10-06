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
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

// The server certificate and the client certificates come from different
// CAs, as when clients hold SPIFFE X.509-SVIDs from SPIRE and the server
// certificate does not. The client CA reaches Valkey only through
// clientAuth.ca, published the way SPIRE publishes it: a SPIFFE bundle under
// bundle.spiffe in a ConfigMap.
var _ = Describe("ValkeyCluster mTLS with clientAuth.ca and URI-based ACL mapping", Ordered, Label("ValkeyCluster", "TLS", "mTLS"), func() {
	const (
		clusterName        = "cluster-clientca"
		selfSigned         = "valkey-clientca-selfsigned"
		serverCA           = "valkey-clientca-server-ca"
		clientCA           = "valkey-clientca-client-ca"
		untrustedCA        = "valkey-clientca-untrusted-ca"
		serverCertSecret   = "valkey-clientca-server-cert"
		clientCertSecret   = "valkey-clientca-client-svid"
		rogueCertSecret    = "valkey-clientca-rogue-svid"
		unmappedCertSecret = "valkey-clientca-unmapped-svid"
		unmappedID         = "spiffe://example.org/ns/default/sa/unmapped"
		unmappedPodName    = "client-clientca-unmapped"
		rotatedCA          = "valkey-clientca-rotated-ca"
		rotatedCertSecret  = "valkey-clientca-rotated-svid"
		rotatedPodName     = "client-clientca-rotated"
		spiffeID           = "spiffe://example.org/ns/default/sa/api"
		spireBundle        = "valkey-clientca-spire-bundle"
		trustedPodName     = "client-clientca-trusted"
		roguePodName       = "client-clientca-rogue"
	)
	var tmpDir string

	// caManifest is a self-signed CA Certificate plus the CA Issuer that signs with it.
	caManifest := func(name string) string {
		return fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %[1]s
spec:
  secretName: %[1]s
  isCA: true
  commonName: %[1]s
  issuerRef:
    name: %[2]s
    kind: Issuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %[1]s
spec:
  ca:
    secretName: %[1]s
`, name, selfSigned)
	}

	// svidManifest is a client certificate carrying only a SPIFFE ID URI SAN.
	svidManifest := func(name, issuer, uri string) string {
		return fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %[1]s
spec:
  secretName: %[1]s
  uris:
    - %[3]s
  usages:
    - client auth
  issuerRef:
    name: %[2]s
    kind: Issuer
    group: cert-manager.io
`, name, issuer, uri)
	}

	// whoami runs ACL WHOAMI from a pod presenting clientSecret's certificate
	// and verifying the server against the server secret's ca.crt. It returns
	// the pod's final phase and logs.
	whoami := func(podName, clientSecret string) (string, string) {
		clusterFqdn := fmt.Sprintf("valkey-%s.default.svc.cluster.local", clusterName)
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", podName, "--ignore-not-found=true"))
		_, err := utils.Run(exec.Command("kubectl", "run", podName,
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
			}`, valkeyClientImage, clusterFqdn, serverCertSecret, clientSecret)))
		Expect(err).NotTo(HaveOccurred())

		var phase string
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", podName, "-o", "jsonpath={.status.phase}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(BeElementOf("Succeeded", "Failed"))
			phase = out
		}).Should(Succeed())
		logs, _ := utils.Run(exec.Command("kubectl", "logs", podName))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", podName, "--ignore-not-found=true"))
		return phase, strings.TrimSpace(logs)
	}

	// publishBundle writes the given CAs' roots to the bundle ConfigMap as a
	// SPIFFE bundle under bundle.spiffe, as SPIRE publishes it, with a jwt-svid
	// key that the operator must skip.
	publishBundle := func(cas ...string) {
		keys := []map[string]any{{"use": "jwt-svid", "kty": "EC", "kid": "jwt"}}
		for _, ca := range cas {
			caB64, err := utils.Run(exec.Command("kubectl", "get", "secret", ca, "-o", "jsonpath={.data.ca\\.crt}"))
			Expect(err).NotTo(HaveOccurred())
			caPEM, err := base64.StdEncoding.DecodeString(strings.TrimSpace(caB64))
			Expect(err).NotTo(HaveOccurred())
			block, _ := pem.Decode(caPEM)
			Expect(block).NotTo(BeNil())
			keys = append(keys, map[string]any{"use": "x509-svid", "kty": "EC", "x5c": []string{base64.StdEncoding.EncodeToString(block.Bytes)}})
		}
		bundle, err := json.Marshal(map[string]any{"spiffe_sequence": len(cas), "keys": keys})
		Expect(err).NotTo(HaveOccurred())
		bundleFile := filepath.Join(tmpDir, "bundle.spiffe")
		Expect(os.WriteFile(bundleFile, bundle, 0644)).To(Succeed())
		manifest, err := utils.Run(exec.Command("kubectl", "create", "configmap", spireBundle,
			"--from-file=bundle.spiffe="+bundleFile, "--dry-run=client", "-o", "yaml"))
		Expect(err).NotTo(HaveOccurred())
		manifestFile := filepath.Join(tmpDir, "bundle-configmap.yaml")
		Expect(os.WriteFile(manifestFile, []byte(manifest), 0644)).To(Succeed())
		_, err = utils.Run(exec.Command("kubectl", "apply", "-f", manifestFile))
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeAll(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "valkey-clientca-test")
		Expect(err).NotTo(HaveOccurred())

		By("creating separate server, client and untrusted CAs, a server certificate and two SPIFFE client certificates")
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
  commonName: valkey-%s.default.svc.cluster.local
  dnsNames:
    - valkey-%s.default.svc.cluster.local
    - localhost
  issuerRef:
    name: %s
    kind: Issuer
    group: cert-manager.io
`, selfSigned, serverCertSecret, serverCertSecret, clusterName, clusterName, serverCA) +
			"---" + caManifest(serverCA) + "---" + caManifest(clientCA) + "---" + caManifest(untrustedCA) +
			"---" + svidManifest(clientCertSecret, clientCA, spiffeID) + "---" + svidManifest(rogueCertSecret, untrustedCA, spiffeID) +
			"---" + svidManifest(unmappedCertSecret, clientCA, unmappedID) +
			"---" + caManifest(rotatedCA) + "---" + svidManifest(rotatedCertSecret, rotatedCA, spiffeID)
		manifestFile := filepath.Join(tmpDir, "clientca-cert-manager.yaml")
		Expect(os.WriteFile(manifestFile, []byte(manifest), 0644)).To(Succeed())
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "apply", "-f", manifestFile))
			return err
		}).Should(Succeed())

		By("waiting for every certificate to be ready")
		for _, cert := range []string{serverCA, clientCA, untrustedCA, serverCertSecret, clientCertSecret, rogueCertSecret, unmappedCertSecret, rotatedCA, rotatedCertSecret} {
			Eventually(func() error {
				_, err := utils.Run(exec.Command("kubectl", "wait", "certificate/"+cert, "--for=condition=Ready", "--timeout=120s"))
				return err
			}).Should(Succeed())
		}

		By("publishing the client CA as a SPIFFE bundle in a ConfigMap, as SPIRE does")
		publishBundle(clientCA)

		By("creating the ValkeyCluster trusting the client CA through clientAuth.ca")
		clusterManifest := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  image: %s
  shards: 1
  replicas: 1
  networking:
    tls:
      certificates:
        server:
          secretName: %s
      clientAuth:
        mode: Required
        certificateUser: URI
        ca:
          - configMapName: %s
            key: bundle.spiffe
  users:
    - name: %s
      enabled: true
      resetpass: true
      permissions: "+@all ~* &*"
`, clusterName, gatedImage, serverCertSecret, spireBundle, spiffeID)
		clusterFile := filepath.Join(tmpDir, "valkeycluster-clientca.yaml")
		Expect(os.WriteFile(clusterFile, []byte(clusterManifest), 0644)).To(Succeed())
		_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true"))
		_, err = utils.Run(exec.Command("kubectl", "apply", "-f", clusterFile))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the cluster to be ready")
		Eventually(func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		}).Should(Succeed())
	})

	AfterAll(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true"))
		for _, cert := range []string{serverCertSecret, clientCertSecret, rogueCertSecret, unmappedCertSecret, rotatedCertSecret, serverCA, clientCA, untrustedCA, rotatedCA} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "certificate", cert, "--ignore-not-found=true"))
			_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", cert, "--ignore-not-found=true"))
		}
		for _, issuer := range []string{selfSigned, serverCA, clientCA, untrustedCA, rotatedCA} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "issuer", issuer, "--ignore-not-found=true"))
		}
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", trustedPodName, roguePodName, unmappedPodName, rotatedPodName, "--ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "configmap", spireBundle, "--ignore-not-found=true"))
		os.RemoveAll(tmpDir)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo("default")
			utils.CollectDebugInfo(namespace)
		}
	})

	It("writes the trust bundle and reports TLSConfigured", func() {
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "secret", clusterName+"-tls-trust",
				"-o", "jsonpath={.data.ca\\.crt}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(BeEmpty())

			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			cond := utils.FindCondition(cr.Status.Conditions, valkeyiov1alpha1.ConditionTLSConfigured)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(string(cond.Status)).To(Equal("True"))
		}).Should(Succeed())
	})

	It("points every ValkeyNode at the trust bundle", func() {
		out, err := utils.Run(exec.Command("kubectl", "get", "valkeynode",
			"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
			"-o", "jsonpath={range .items[*]}{.spec.tls.certificates.trustBundle.secretName}{\"\\n\"}{end}"))
		Expect(err).NotTo(HaveOccurred())
		refs := strings.Fields(out)
		Expect(refs).To(HaveLen(2))
		Expect(refs).To(HaveEach(clusterName + "-tls-trust"))
	})

	It("authenticates a client certificate from the client CA as its SPIFFE ID", func() {
		phase, logs := whoami(trustedPodName, clientCertSecret)
		Expect(phase).To(Equal("Succeeded"), logs)
		Expect(logs).To(Equal(spiffeID), "ACL WHOAMI should resolve to the user named by the certificate's URI SAN")
	})

	It("trusts a root added to the source live, without restarting any node", func() {
		podUIDs := func() string {
			out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
				"-o", "jsonpath={range .items[*]}{.metadata.uid}{\" \"}{.status.containerStatuses[0].restartCount}{\"\\n\"}{end}"))
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		before := podUIDs()

		phase, logs := whoami(rotatedPodName, rotatedCertSecret)
		Expect(phase).To(Equal("Failed"), "a root not yet in the bundle must not be trusted: %s", logs)

		publishBundle(clientCA, rotatedCA)
		Eventually(func(g Gomega) {
			phase, logs := whoami(rotatedPodName, rotatedCertSecret)
			g.Expect(phase).To(Equal("Succeeded"), logs)
			g.Expect(logs).To(Equal(spiffeID))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		Expect(podUIDs()).To(Equal(before), "the new root must be picked up without recreating or restarting a pod")
	})

	It("disables the default user, so a trusted certificate that names no ACL user gets NOAUTH", func() {
		_, logs := whoami(unmappedPodName, unmappedCertSecret)
		Expect(logs).To(ContainSubstring("NOAUTH"),
			"with clientAuth.ca set and no default in spec.users, an unmapped certificate must not fall back to default")
	})

	It("rejects a client certificate from a CA that is not in clientAuth.ca", func() {
		phase, logs := whoami(roguePodName, rogueCertSecret)
		Expect(phase).To(Equal("Failed"), "a certificate from an untrusted CA must not connect: %s", logs)
	})

	// Runs last: it removes clientAuth.ca. The ACL change reaches every node at
	// once, while nodes keep trusting the removed roots until they roll, so
	// default must stay disabled through and after the roll.
	It("keeps the default user disabled after clientAuth.ca is removed", func() {
		_, err := utils.Run(exec.Command("kubectl", "patch", "valkeycluster", clusterName, "--type=json",
			"-p", `[{"op":"remove","path":"/spec/networking/tls/clientAuth/ca"}]`))
		Expect(err).NotTo(HaveOccurred())

		By("connecting with an unmapped certificate from the removed CA while nodes roll")
		phase, logs := whoami(unmappedPodName, unmappedCertSecret)
		Expect(logs).NotTo(Equal("default"), "an old-CA certificate must never land on default (phase %s)", phase)
		if phase == "Succeeded" {
			Expect(logs).To(ContainSubstring("NOAUTH"))
		}

		By("waiting for every node to stop referencing the trust bundle")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "valkeynode",
				"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
				"-o", "jsonpath={range .items[*]}{.spec.tls.certificates.trustBundle.secretName}{\"|\"}{end}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.Trim(out, "|")).To(BeEmpty())
		}).Should(Succeed())

		By("checking the aclfile still disables default")
		aclB64, err := utils.Run(exec.Command("kubectl", "get", "secret", "internal-"+clusterName+"-acl",
			"-o", "jsonpath={.data.users\\.acl}"))
		Expect(err).NotTo(HaveOccurred())
		acl, err := base64.StdEncoding.DecodeString(strings.TrimSpace(aclB64))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(acl)).To(ContainSubstring("user default off resetkeys resetchannels -@all"))
	})
})
