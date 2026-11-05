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

// Database-scoped ACLs (db=/alldbs/resetdbs) are a Valkey 9.1 feature, so these
// reuse the gatedImage/preGateImage constants from the TLS suite.
//
// Every cluster here sets cluster-databases: "2". The operator always runs
// cluster mode, where the database count comes from cluster-databases and
// defaults to 1 — so on a default cluster db 1 does not exist and "access to
// db 1 is refused" would pass for the wrong reason. Raising it to 2 is what
// makes the negative assertion mean what it says.
var _ = Describe("ValkeyCluster database ACLs", Ordered, Label("ValkeyCluster", "ACL"), func() {
	var tmpDir string

	// user renders one user entry with the given databases block, which may be
	// empty. Indentation is fixed by the surrounding manifest.
	user := func(name, databases string) string {
		entry := fmt.Sprintf(`    - name: %s
      enabled: true
      nopass: true
      keys:
        readWrite: ["*"]
      commands:
        allow: ["@all"]
`, name)
		if databases != "" {
			entry += "      databases:\n" + databases
		}
		return entry
	}

	// applyCluster writes and applies a single-shard cluster carrying users. It
	// applies over an existing cluster of the same name, so a spec can edit a
	// cluster in place; use deleteCluster first for a clean start.
	applyCluster := func(name, image string, users ...string) {
		manifest := fmt.Sprintf(`
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  image: %s
  shards: 1
  replicas: 0
  config:
    cluster-databases: "2"
  users:
    # The _operator system user is granted acl|load, acl|getuser and acl|users
    # but deliberately not acl|list, so reading the applied ACL back needs a
    # user of our own. The shipped users sample defines default the same way.
    - name: default
      enabled: true
      nopass: true
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

	// waitReady allows more than the suite default of 2 minutes: these are the
	// first specs to use gatedImage/preGateImage in a focused run, so the node
	// may still be pulling the image. The failure message carries the state,
	// reason and message, because "Failed" on its own says only that Ready is
	// False and the cluster is not progressing — which is also what a slow
	// image pull looks like.
	waitReady := func(name string) {
		Eventually(func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
				"cluster %s is %s (%s: %s)", name, cr.Status.State, cr.Status.Reason, cr.Status.Message)
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	}

	// serverPod returns the name of the cluster's first server pod.
	serverPod := func(name string) string {
		out, err := utils.Run(exec.Command("kubectl", "get", "pods",
			"-l", "valkey.io/cluster="+name,
			"-o", "jsonpath={.items[0].metadata.name}"))
		Expect(err).NotTo(HaveOccurred())
		pod := strings.TrimSpace(out)
		Expect(pod).NotTo(BeEmpty(), "no server pod found for cluster %s", name)
		return pod
	}

	// aclList returns ACL LIST as read back from the running server, which is
	// what makes these assertions about the server's view rather than about the
	// file the operator wrote. The fixture's default user is nopass, so no
	// credentials are needed.
	aclList := func(name string) string {
		out, err := utils.Run(exec.Command("kubectl", "exec", serverPod(name), "-c", "server", "--",
			"valkey-cli", "--no-auth-warning", "ACL", "LIST"))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("NOPERM"), "default user cannot read ACL LIST")
		return out
	}

	// renderedACL returns the aclfile the operator wrote for the cluster, from
	// the internal Secret every pod mounts (named as getInternalSecretName
	// does). Reading this rather than ACL LIST is what tests the version gate
	// on an old server, which cannot print db= back even if it was written.
	renderedACL := func(name string) string {
		out, err := utils.Run(exec.Command("kubectl", "get", "secret", "internal-"+name+"-acl",
			"-o", `jsonpath={.data.users\.acl}`))
		Expect(err).NotTo(HaveOccurred())
		acl, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
		Expect(err).NotTo(HaveOccurred())
		return string(acl)
	}

	// userLine returns the line for one user from ACL LIST output or from a
	// rendered aclfile; both use the same "user <name> ..." form.
	userLine := func(aclOut, name string) string {
		for line := range strings.SplitSeq(aclOut, "\n") {
			if strings.HasPrefix(line, "user "+name+" ") {
				return line
			}
		}
		Fail(fmt.Sprintf("no ACL line for user %q in:\n%s", name, aclOut))
		return ""
	}

	// warnsUnsupportedAcl reports whether cr carries the ConfigurationWarning
	// raised for dropped database directives.
	warnsUnsupportedAcl := func(cr *valkeyiov1alpha1.ValkeyCluster) bool {
		for _, c := range cr.Status.Conditions {
			if c.Type == valkeyiov1alpha1.ConditionConfigurationWarning &&
				c.Reason == valkeyiov1alpha1.ReasonUnsupportedAclDirective {
				return true
			}
		}
		return false
	}

	// dbDirective extracts the database-scoping directive from an ACL line, or
	// "" when it carries none.
	dbDirectiveRE := regexp.MustCompile(`\b(alldbs|resetdbs|db=[0-9,]+)\b`)
	dbDirective := func(line string) string {
		return dbDirectiveRE.FindString(line)
	}

	// asDbuser runs args as dbuser. dbuser is a nopass user, so any password
	// authenticates it.
	asDbuser := func(name string, args ...string) string {
		base := []string{"exec", serverPod(name), "-c", "server", "--",
			"valkey-cli", "--user", "dbuser", "--pass", "x", "--no-auth-warning"}
		out, _ := utils.Run(exec.Command("kubectl", append(base, args...)...))
		// Errors are the point of several of these calls, so the verdict is
		// read from the output rather than the exit status.
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
				user("dbuser", "        ids: [0]\n"),
				user("alldbsuser", "        allDbs: true\n"),
				// Control: no databases block at all. allDbs is only a no-op
				// if it renders identically to this.
				user("plainuser", ""),
			)
			waitReady(clusterName)
		})

		AfterAll(func() {
			deleteCluster(clusterName)
		})

		It("renders db=0 as the only database directive for a scoped user", func() {
			line := userLine(aclList(clusterName), "dbuser")
			// db= clears alldbs by itself, so it is the whole directive. A
			// resetdbs alongside it would mean the renderer copied the channels
			// pattern, which does need one.
			Expect(dbDirective(line)).To(Equal("db=0"), "line: %s", line)
		})

		It("makes allDbs indistinguishable from omitting databases entirely", func() {
			// The empirical check on the claim that allDbs is a no-op.
			//
			// Whether the server prints "alldbs" at all is version-dependent:
			// 9.1.0 emits it for every user (src/acl.c, "alldbs " is appended
			// when SELECTOR_FLAG_ALLDBS is set), while later 9.1.x omit it as
			// the default "for compatibility". Asserting either spelling would
			// pin this suite to one patch release.
			//
			// What holds on both is the part that actually matters: a user with
			// allDbs: true renders exactly like one that never set databases.
			// That is what makes the field unobservable, and if it ever stops
			// being true, allDbs has become a real setting and the docs need
			// revisiting.
			out := aclList(clusterName)
			withFlag := userLine(out, "alldbsuser")
			control := userLine(out, "plainuser")

			Expect(dbDirective(withFlag)).To(Equal(dbDirective(control)),
				"allDbs changed the rendered ACL\n  allDbs: %s\n  control: %s", withFlag, control)
			Expect(dbDirective(withFlag)).NotTo(HavePrefix("db="))
			Expect(dbDirective(withFlag)).NotTo(Equal("resetdbs"))
		})

		It("refuses access to a database outside the grant", func() {
			// The check that proves the directive reached the server rather
			// than merely being written to the file.
			//
			// The granted database must actually work, not merely avoid
			// NOPERM: an auth or connection failure would avoid it too, and so
			// does the CLUSTERDOWN a fresh primary can still return just after
			// the operator reports Ready, which is why this one is retried.
			Eventually(func(g Gomega) {
				g.Expect(strings.TrimSpace(asDbuser(clusterName, "-n", "0", "SET", "probe", "x"))).To(Equal("OK"))
			}).Should(Succeed())

			// Assert on NOPERM specifically: if cluster-databases were not
			// raised to 2, db 1 would not exist and this same call would fail
			// with an out-of-range error instead, which would be a false pass.
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
			applyCluster(clusterName, preGateImage, user("dbuser", "        ids: [0]\n"))
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
						// The full prerelease floor: "9.1.0" alone is a
						// substring of it and would pass either way.
						g.Expect(c.Message).To(ContainSubstring("9.1.0-rc1"))
						g.Expect(c.Message).To(ContainSubstring("9.0.0"))
						hit = true
					}
				}
				g.Expect(hit).To(BeTrue(), "expected ConfigurationWarning with reason UnsupportedAclDirective")
			}).Should(Succeed())
		})

		It("drops the directive from the rendered ACL", func() {
			// Read the file the operator wrote, not ACL LIST: a pre-9.1 server
			// cannot print db= or resetdbs back, so asking it would pass
			// whatever the gate did. dbuser asked for ids: [0], and the gate
			// must leave no database directive for it at all.
			line := userLine(renderedACL(clusterName), "dbuser")
			Expect(dbDirective(line)).To(BeEmpty(), "line: %s", line)
		})

		It("clears the warning once databases is removed", func() {
			// Edited in place, not recreated: a new cluster starts with no
			// conditions, so it would pass before any reconcile had run. With
			// the warning confirmed first, the second check can only pass once
			// a reconcile has seen the edit and cleared it.
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
