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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

var _ = Describe("Valkey standalone", Ordered, Label("cluster-lifecycle", "standalone"), func() {
	const testNamespace = "valkey-standalone-e2e"
	const instanceName = "standalone"
	const deployment = "valkey-operator-controller-manager"
	var originalArgs []string

	run := func(args ...string) string {
		GinkgoHelper()
		out, err := utils.Run(exec.Command("kubectl", args...))
		Expect(err).NotTo(HaveOccurred(), "%s", out)
		return out
	}
	apply := func(manifest string) {
		GinkgoHelper()
		cmd := exec.Command("kubectl", "apply", "-n", testNamespace, "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "%s", out)
	}
	setArgs := func(args []string) {
		GinkgoHelper()
		patch, err := json.Marshal([]map[string]any{
			{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": args},
		})
		Expect(err).NotTo(HaveOccurred())
		run("patch", "deployment", deployment, "-n", namespace, "--type=json", "-p", string(patch))
		run("rollout", "status", "deployment/"+deployment, "-n", namespace, "--timeout=120s")
	}
	getInstance := func(g Gomega) valkeyv1.Valkey {
		out, err := utils.Run(exec.Command("kubectl", "get", "valkey", instanceName, "-n", testNamespace, "-o", "json"))
		g.Expect(err).NotTo(HaveOccurred())
		var v valkeyv1.Valkey
		g.Expect(json.Unmarshal([]byte(out), &v)).To(Succeed())
		return v
	}
	podIdentity := func(g Gomega) (string, string) {
		out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", testNamespace, "-l",
			"valkey.io/cluster="+instanceName,
			"-o", `jsonpath={range .items[*]}{.metadata.name} {.metadata.uid}{"\n"}{end}`))
		g.Expect(err).NotTo(HaveOccurred())
		fields := strings.Fields(out)
		g.Expect(fields).To(HaveLen(2))
		return fields[0], fields[1]
	}
	cli := func(g Gomega, pod string, args ...string) string {
		cmdArgs := []string{"exec", "-n", testNamespace, pod, "-c", "server", "--",
			"env", "-u", "VALKEYCLI_AUTH", "-u", "REDISCLI_AUTH", "valkey-cli", "-e", "--raw"}
		out, err := utils.Run(exec.Command("kubectl", append(cmdArgs, args...)...))
		g.Expect(err).NotTo(HaveOccurred(), "%s", out)
		return strings.TrimSpace(out)
	}
	ready := func(g Gomega) {
		v := getInstance(g)
		g.Expect(v.Status.ObservedGeneration).To(Equal(v.Generation))
		g.Expect(v.Status.State).To(Equal(valkeyv1.ValkeyStateReady))
		g.Expect(meta.IsStatusConditionTrue(v.Status.Conditions, "Ready")).To(BeTrue())
	}

	BeforeAll(func() {
		By("starting with the default installation, without an alpha CRD")
		run("rollout", "status", "deployment/"+deployment, "-n", namespace, "--timeout=120s")
		Expect(strings.TrimSpace(run("get", "crd", "valkeys.valkey.io", "--ignore-not-found", "-o", "name"))).To(BeEmpty())
		out := run("get", "deployment", deployment, "-n", namespace, "-o",
			"jsonpath={.spec.template.spec.containers[0].args}")
		Expect(json.Unmarshal([]byte(out), &originalArgs)).To(Succeed())
		run("create", "namespace", testNamespace)
		DeferCleanup(func() {
			run("delete", "namespace", testNamespace, "--wait=true", "--timeout=120s")
			setArgs(originalArgs)
			_, _ = utils.Run(exec.Command("make", "uninstall-alpha", "ignore-not-found=true"))
		})
		run("label", "namespace", testNamespace, "pod-security.kubernetes.io/enforce=restricted")
		_, err := utils.Run(exec.Command("make", "install-alpha"))
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
			_, _ = utils.Run(exec.Command("kubectl", "get", "valkey,valkeynode,pods", "-n", testNamespace, "-o", "yaml"))
		}
	})

	It("leaves the alpha instance unreconciled until its gate is enabled", func() {
		apply(`apiVersion: v1
kind: Secret
metadata:
  name: app-password
stringData:
  password: initial-password
---
apiVersion: valkey.io/v1alpha1
kind: Valkey
metadata:
  name: standalone
spec:
  exporter:
    enabled: false
  persistence:
    size: 1Gi
  podSecurityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: server
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
        readOnlyRootFilesystem: true
  config:
    maxmemory: 100mb
    maxmemory-policy: allkeys-lru
  acl:
    users:
      - name: default
        password:
          type: Any
        permissions: "+@all ~* &*"
      - name: app
        password:
          secret:
            name: app-password
            keys: [password]
        permissions: "+@all ~* &*"
`)
		Consistently(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "valkeynode", "-n", testNamespace, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeEmpty())
		}, 5*time.Second).Should(Succeed())
		By("enabling the Valkey controller explicitly")
		setArgs(append(append([]string{}, originalArgs...), "--feature-gates=Valkey=true"))
		Eventually(ready).Should(Succeed())
	})

	It("keeps standalone data and live settings through ACL fallback and pod replacement", func() {
		var pod, uid string
		Eventually(func(g Gomega) {
			ready(g)
			pod, uid = podIdentity(g)
			g.Expect(cli(g, pod, "INFO", "server")).To(ContainSubstring("server_mode:standalone"))
			g.Expect(cli(g, pod, "CONFIG", "GET", "cluster-enabled")).To(Equal("cluster-enabled\nno"))
			g.Expect(cli(g, pod, "AUTH", "app", "initial-password")).To(Equal("OK"))
			g.Expect(cli(g, pod, "SET", "e2e:persistent", "retained")).To(Equal("OK"))
			g.Expect(cli(g, pod, "SAVE")).To(Equal("OK"))
		}).Should(Succeed())
		config := run("get", "configmap", "valkey-"+instanceName, "-n", testNamespace, "-o", `jsonpath={.data.valkey\.conf}`)
		Expect(config).NotTo(ContainSubstring("cluster-enabled"))

		By("keeping the last ACL while applying unrelated config with a missing user Secret")
		run("delete", "secret", "app-password", "-n", testNamespace)
		run("patch", "valkey", instanceName, "-n", testNamespace, "--type=merge", "-p",
			`{"spec":{"config":{"maxmemory":"120mb"}}}`)
		Eventually(func(g Gomega) {
			v := getInstance(g)
			c := meta.FindStatusCondition(v.Status.Conditions, "Degraded")
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(c.Reason).To(Equal("UsersACLUnresolved"))
			g.Expect(v.Status.ObservedGeneration).To(Equal(v.Generation))
			_, currentUID := podIdentity(g)
			g.Expect(currentUID).To(Equal(uid))
			g.Expect(cli(g, pod, "CONFIG", "GET", "maxmemory")).To(Equal("maxmemory\n125829120"))
			g.Expect(cli(g, pod, "AUTH", "app", "initial-password")).To(Equal("OK"))
		}).Should(Succeed())

		By("recovering the desired ACL without replacing the pod")
		apply(`apiVersion: v1
kind: Secret
metadata:
  name: app-password
stringData:
  password: rotated-password
`)
		Eventually(func(g Gomega) {
			ready(g)
			_, currentUID := podIdentity(g)
			g.Expect(currentUID).To(Equal(uid))
			g.Expect(cli(g, pod, "AUTH", "app", "rotated-password")).To(Equal("OK"))
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", testNamespace, pod, "-c", "server", "--",
				"env", "-u", "VALKEYCLI_AUTH", "-u", "REDISCLI_AUTH", "valkey-cli", "-e", "AUTH", "app", "initial-password"))
			g.Expect(err).To(HaveOccurred())
			g.Expect(out).To(ContainSubstring("WRONGPASS"))
			g.Expect(cli(g, pod, "GET", "e2e:persistent")).To(Equal("retained"))
		}, 3*time.Minute).Should(Succeed())

		By("replacing the pod and reading its persisted data")
		run("delete", "pod", pod, "-n", testNamespace, "--wait=false")
		Eventually(func(g Gomega) {
			replacement, replacementUID := podIdentity(g)
			g.Expect(replacementUID).NotTo(Equal(uid))
			ready(g)
			g.Expect(cli(g, replacement, "GET", "e2e:persistent")).To(Equal("retained"))
			g.Expect(cli(g, replacement, "AUTH", "app", "rotated-password")).To(Equal("OK"))
		}).Should(Succeed())
		Expect(strings.TrimSpace(run("get", "pvc", "-n", testNamespace, "-o", "name"))).NotTo(BeEmpty())
	})
})
