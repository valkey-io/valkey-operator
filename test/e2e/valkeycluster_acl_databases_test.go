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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

// cluster-databases is 2 so that db 1 exists for the NOPERM checks.
var _ = Describe("ValkeyCluster database ACLs", Ordered, Label("ValkeyCluster", "ACL"), func() {
	var tmpDir string

	user := func(name, databases string) string {
		entry := fmt.Sprintf(`      - name: %s
        password:
          type: Any
        keys:
          readWrite: ["*"]
        commands:
          allow: ["@all"]
`, name)
		if databases != "" {
			entry += "        databases:\n" + databases
		}
		return entry
	}

	applyCluster := func(name, image string, users ...string) {
		manifest := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  image: %s
  shards: 1
  replicasPerShard: 0
  config:
    cluster-databases: "2"
  acl:
    users:
      # The _operator system user is granted acl|load, acl|getuser and acl|users
      # but deliberately not acl|list, so reading the applied ACL back needs a
      # user of our own. The operator disables an undeclared default, so it is
      # declared open here, as the shipped samples do.
      - name: default
        password:
          type: Any
        permissions: "+@all ~* &*"
%s
`, name, image, strings.Join(users, ""))

		manifestFile := filepath.Join(tmpDir, name+".yaml")
		Expect(os.WriteFile(manifestFile, []byte(manifest), 0644)).To(Succeed())

		_, err := utils.Run(exec.Command("kubectl", "apply", "-f", manifestFile))
		Expect(err).NotTo(HaveOccurred())
	}

	deleteCluster := func(name string) {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "valkeycluster", name, "--ignore-not-found=true"))
	}

	// waitReady allows extra time for the first image pull.
	waitReady := func(name string) {
		Eventually(func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
				"cluster %s is %s (%s: %s)", name, cr.Status.State, cr.Status.Reason, cr.Status.Message)
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	}

	serverPod := func(name string) string {
		out, err := utils.Run(exec.Command("kubectl", "get", "pods",
			"-l", "valkey.io/cluster="+name,
			"-o", "jsonpath={.items[0].metadata.name}"))
		Expect(err).NotTo(HaveOccurred())
		pod := strings.TrimSpace(out)
		Expect(pod).NotTo(BeEmpty(), "no server pod found for cluster %s", name)
		return pod
	}

	aclList := func(name string) string {
		out, err := utils.Run(exec.Command("kubectl", "exec", serverPod(name), "-c", "server", "--",
			"valkey-cli", "--no-auth-warning", "ACL", "LIST"))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("NOPERM"), "default user cannot read ACL LIST")
		return out
	}

	renderedACL := func(name string) string {
		out, err := utils.Run(exec.Command("kubectl", "get", "secret", "internal-"+name+"-acl",
			"-o", `jsonpath={.data.users\.acl}`))
		Expect(err).NotTo(HaveOccurred())
		acl, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
		Expect(err).NotTo(HaveOccurred())
		return string(acl)
	}

	userLine := func(aclOut, name string) string {
		for line := range strings.SplitSeq(aclOut, "\n") {
			if strings.HasPrefix(line, "user "+name+" ") {
				return line
			}
		}
		Fail(fmt.Sprintf("no ACL line for user %q in:\n%s", name, aclOut))
		return ""
	}

	warnsUnsupportedAcl := func(cr *valkeyiov1alpha1.ValkeyCluster) bool {
		for _, c := range cr.Status.Conditions {
			if c.Type == valkeyiov1alpha1.ConditionConfigurationWarning &&
				c.Reason == valkeyiov1alpha1.ReasonUnsupportedAclDirective {
				return true
			}
		}
		return false
	}

	dbDirectiveRE := regexp.MustCompile(`\b(alldbs|resetdbs|db=[0-9,]+)\b`)
	dbDirective := func(line string) string {
		return dbDirectiveRE.FindString(line)
	}

	asDbuser := func(name string, args ...string) string {
		base := []string{"exec", serverPod(name), "-c", "server", "--",
			"valkey-cli", "--user", "dbuser", "--pass", "x", "--no-auth-warning"}
		out, _ := utils.Run(exec.Command("kubectl", append(base, args...)...))
		// Callers check the output, since some calls are meant to fail.
		return out
	}

	BeforeAll(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "valkey-acl-databases")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_ = os.RemoveAll(tmpDir)
	})

	Context("on a Valkey 9.1 image", func() {
		const clusterName = "cluster-acl-db-gated"

		BeforeAll(func() {
			deleteCluster(clusterName)
			applyCluster(clusterName, gatedImage,
				user("dbuser", "          ids: [0]\n"),
				user("alldbsuser", "          allDbs: true\n"),
				// Control user without databases.
				user("plainuser", ""),
			)
			waitReady(clusterName)
		})

		AfterAll(func() {
			deleteCluster(clusterName)
		})

		It("renders db=0 as the only database directive for a scoped user", func() {
			line := userLine(aclList(clusterName), "dbuser")
			Expect(dbDirective(line)).To(Equal("db=0"), "line: %s", line)
		})

		It("makes allDbs indistinguishable from omitting databases entirely", func() {
			// Whether ACL LIST prints alldbs varies across 9.1.x, so compare with the
			// control user instead.
			out := aclList(clusterName)
			withFlag := userLine(out, "alldbsuser")
			control := userLine(out, "plainuser")

			Expect(dbDirective(withFlag)).To(Equal(dbDirective(control)),
				"allDbs changed the rendered ACL\n  allDbs: %s\n  control: %s", withFlag, control)
			Expect(dbDirective(withFlag)).NotTo(HavePrefix("db="))
			Expect(dbDirective(withFlag)).NotTo(Equal("resetdbs"))
		})

		It("refuses access to a database outside the grant", func() {
			// db 0 must work, not just avoid NOPERM. Retried for early CLUSTERDOWN.
			Eventually(func(g Gomega) {
				g.Expect(strings.TrimSpace(asDbuser(clusterName, "-n", "0", "SET", "probe", "x"))).To(Equal("OK"))
			}).Should(Succeed())

			// NOPERM, not an out-of-range error, proves the restriction.
			Expect(asDbuser(clusterName, "-n", "1", "GET", "probe")).To(ContainSubstring("NOPERM"))
		})

		It("sets no configuration warning", func() {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			Expect(err).NotTo(HaveOccurred())
			for _, c := range cr.Status.Conditions {
				Expect(c.Reason).NotTo(Equal(valkeyiov1alpha1.ReasonUnsupportedAclDirective))
			}
		})
	})

	Context("on a pre-9.1 image", func() {
		const clusterName = "cluster-acl-db-pregate"

		BeforeAll(func() {
			deleteCluster(clusterName)
			applyCluster(clusterName, preGateImage, user("dbuser", "          ids: [0]\n"))
			waitReady(clusterName)
		})

		AfterAll(func() {
			deleteCluster(clusterName)
		})

		It("reaches Ready and warns instead of rejecting the spec", func() {
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))

				var hit bool
				for _, c := range cr.Status.Conditions {
					if c.Type == valkeyiov1alpha1.ConditionConfigurationWarning &&
						c.Reason == valkeyiov1alpha1.ReasonUnsupportedAclDirective {
						g.Expect(string(c.Status)).To(Equal("True"))
						g.Expect(c.Message).To(ContainSubstring("dbuser"))
						// "9.1.0" alone would always match.
						g.Expect(c.Message).To(ContainSubstring("9.1.0-rc1"))
						g.Expect(c.Message).To(ContainSubstring("9.0.0"))
						hit = true
					}
				}
				g.Expect(hit).To(BeTrue(), "expected ConfigurationWarning with reason UnsupportedAclDirective")
			}).Should(Succeed())
		})

		It("drops the directive from the rendered ACL", func() {
			// A pre-9.1 server cannot print db= back, so read the rendered file.
			line := userLine(renderedACL(clusterName), "dbuser")
			Expect(dbDirective(line)).To(BeEmpty(), "line: %s", line)
		})

		It("clears the warning once databases is removed", func() {
			// Edited in place, so the check needs a reconcile to pass.
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(warnsUnsupportedAcl(cr)).To(BeTrue(), "the warning must be set before the edit")
			}).Should(Succeed())

			applyCluster(clusterName, preGateImage, user("dbuser", ""))

			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(warnsUnsupportedAcl(cr)).To(BeFalse(), "the warning should clear once databases is removed")
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			}).Should(Succeed())
		})
	})
})
