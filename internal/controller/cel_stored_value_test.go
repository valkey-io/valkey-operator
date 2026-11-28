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

package controller

import (
	"context"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

const (
	nodeConfigKeyMessage    = "spec.config keys must be lowercase"
	aclUserSecretMessage    = "password.secret.name is required"
	legacyUserSecretMessage = "passwordSecret.name is required"
	clustersCRD             = "valkeyclusters.valkey.io"
	nodesCRD                = "valkeynodes.valkey.io"
)

// celRule names an x-kubernetes-validations rule by its path under the
// spec schema and a substring of its message.
type celRule struct {
	path    []string
	message string
}

// withCELRulesRemoved runs fn while the named CRD lacks the given rules, so fn
// can store an object that an older release accepted. probe must create, as a
// dry run, an object the rules reject. The rules are restored before it
// returns.
func withCELRulesRemoved(ctx context.Context, crdName string, rules []celRule, probe func() error, fn func()) {
	GinkgoHelper()
	key := client.ObjectKey{Name: crdName}
	getCRD := func(g Gomega) *unstructured.Unstructured {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		g.Expect(k8sClient.Get(ctx, key, crd)).To(Succeed())
		return crd
	}
	setVersions := func(versions []any) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			crd := getCRD(g)
			g.Expect(unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")).To(Succeed())
			g.Expect(k8sClient.Update(ctx, crd)).To(Succeed())
		}).WithTimeout(30 * time.Second).Should(Succeed())
	}

	saved, _, err := unstructured.NestedSlice(getCRD(Default).Object, "spec", "versions")
	Expect(err).NotTo(HaveOccurred())
	relaxed, _, err := unstructured.NestedSlice(getCRD(Default).Object, "spec", "versions")
	Expect(err).NotTo(HaveOccurred())
	for _, rule := range rules {
		path := append([]string{"schema", "openAPIV3Schema", "properties", "spec"}, rule.path...)
		path = append(path, "x-kubernetes-validations")
		removed := 0
		for i := range relaxed {
			version := relaxed[i].(map[string]any)
			existing, _, err := unstructured.NestedSlice(version, path...)
			Expect(err).NotTo(HaveOccurred())
			kept := slices.DeleteFunc(existing, func(r any) bool {
				message, _ := r.(map[string]any)["message"].(string)
				return strings.Contains(message, rule.message)
			})
			removed += len(existing) - len(kept)
			Expect(unstructured.SetNestedSlice(version, kept, path...)).To(Succeed())
		}
		Expect(removed).To(BeNumerically(">", 0), "no rule %q at %v", rule.message, rule.path)
	}

	setVersions(relaxed)
	defer func() {
		setVersions(saved)
		Eventually(probe).WithTimeout(30 * time.Second).Should(MatchError(ContainSubstring(rules[0].message)))
	}()
	Eventually(probe).WithTimeout(30 * time.Second).Should(Succeed())
	fn()
}

// Kubernetes 1.31 and 1.32 do not ratchet CEL rules on status writes, so each
// rule below must itself pass a value that an update leaves unchanged. Without
// that, a status write on an object stored before the rule existed fails.
var _ = Describe("CEL rules on a value stored before the rule existed", func() {
	ctx := context.Background()

	expectInvalid := func(err error, messages ...string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
		for _, message := range messages {
			Expect(err.Error()).To(ContainSubstring(message))
		}
	}
	latestCluster := func(name string) *valkeyiov1alpha1.ValkeyCluster {
		GinkgoHelper()
		cluster := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, cluster)).To(Succeed())
		return cluster
	}
	latestNode := func(name string) *valkeyiov1alpha1.ValkeyNode {
		GinkgoHelper()
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, node)).To(Succeed())
		return node
	}
	dryRun := func(obj client.Object) func() error {
		return func() error {
			probe := obj.DeepCopyObject().(client.Object)
			probe.SetName(probe.GetName() + "-probe")
			return k8sClient.Create(ctx, probe, client.DryRunAll)
		}
	}
	store := func(crdName string, rules []celRule, obj client.Object) {
		GinkgoHelper()
		withCELRulesRemoved(ctx, crdName, rules, dryRun(obj), func() {
			Expect(k8sClient.Create(ctx, obj.DeepCopyObject().(client.Object))).To(Succeed())
		})
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
	}
	// expectClusterWritesAccepted checks a status write and a spec update
	// that leaves the stored value alone.
	expectClusterWritesAccepted := func(name string) {
		GinkgoHelper()
		By("accepting a status write")
		cluster := latestCluster(name)
		cluster.Status.State = valkeyiov1alpha1.ClusterStateReady
		cluster.Status.Shards = 1
		Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())
		Expect(latestCluster(name).Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))

		By("accepting a spec update that leaves the stored value alone")
		cluster = latestCluster(name)
		cluster.Spec.Shards = 2
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
	}

	It("keeps a stored spec.config with an uppercase key and an operator-owned key", func() {
		const name = "stored-config"
		invalid := map[string]string{"MaxMemory": "1gb", "dir": "/data2", "maxmemory-policy": "allkeys-lru"}
		store(clustersCRD, []celRule{
			{path: []string{"properties", "config"}, message: keyFormatMessage},
			{path: []string{"properties", "config"}, message: reservedKeyMessage},
		}, configCluster(name, invalid))

		expectClusterWritesAccepted(name)

		By("rejecting a change to another key while the invalid keys stay")
		cluster := latestCluster(name)
		cluster.Spec.Config["maxmemory-policy"] = "volatile-lru"
		expectInvalid(k8sClient.Update(ctx, cluster), keyFormatMessage, reservedKeyMessage)

		By("rejecting a new invalid key")
		cluster = latestCluster(name)
		cluster.Spec.Config["MAXCLIENTS"] = "100"
		expectInvalid(k8sClient.Update(ctx, cluster), keyFormatMessage)

		By("accepting spec.config once the invalid keys are gone")
		cluster = latestCluster(name)
		cluster.Spec.Config = map[string]string{"maxmemory": "1gb", "maxmemory-policy": "allkeys-lru"}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		By("rejecting a create with the invalid keys")
		expectInvalid(k8sClient.Create(ctx, configCluster("stored-config-new", invalid)), keyFormatMessage, reservedKeyMessage)
	})

	It("keeps a stored ValkeyNode spec.config with an uppercase key", func() {
		const name = "stored-node-config"
		node := func(name string) *valkeyiov1alpha1.ValkeyNode {
			return &valkeyiov1alpha1.ValkeyNode{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: valkeyiov1alpha1.ValkeyNodeSpec{
					ConfigRevision:      testConfigRevision,
					SharedConfigMapName: testSharedConfigMapName,
					WorkloadType:        valkeyiov1alpha1.WorkloadTypeStatefulSet,
					Config:              map[string]string{"MaxMemory": "1gb"},
				},
			}
		}
		store(nodesCRD, []celRule{{path: []string{"properties", "config"}, message: nodeConfigKeyMessage}}, node(name))

		By("accepting a status write")
		stored := latestNode(name)
		stored.Status.ObservedGeneration = stored.Generation
		stored.Status.PodIP = "10.0.0.1"
		Expect(k8sClient.Status().Update(ctx, stored)).To(Succeed())
		Expect(latestNode(name).Status.PodIP).To(Equal("10.0.0.1"))

		By("accepting a spec update that leaves spec.config alone")
		stored = latestNode(name)
		stored.Spec.ConfigRevision = "111111111111"
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())

		By("rejecting a change to spec.config while the uppercase key stays")
		stored = latestNode(name)
		stored.Spec.Config["maxmemory-policy"] = "allkeys-lru"
		expectInvalid(k8sClient.Update(ctx, stored), nodeConfigKeyMessage)

		By("rejecting a create with the uppercase key")
		expectInvalid(k8sClient.Create(ctx, node("stored-node-config-new")), nodeConfigKeyMessage)
	})

	It("keeps a stored spec.acl.users entry without password.secret.name", func() {
		const name = "stored-acl-user"
		withoutSecretName := func(user string) valkeyiov1alpha1.ACLUserSpec {
			return valkeyiov1alpha1.ACLUserSpec{
				Name:     user,
				Password: &valkeyiov1alpha1.ACLUserPassword{Secret: &valkeyiov1alpha1.PasswordSecretSpec{Keys: []string{user + "pw"}}},
			}
		}
		cluster := func(name string) *valkeyiov1alpha1.ValkeyCluster {
			return &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: valkeyiov1alpha1.ValkeyClusterSpec{
					Shards: 1,
					ACL:    &valkeyiov1alpha1.ACLSpec{Users: []valkeyiov1alpha1.ACLUserSpec{withoutSecretName("alice")}},
				},
			}
		}
		store(clustersCRD, []celRule{
			{path: []string{"properties", "acl", "properties", "users", "items"}, message: aclUserSecretMessage},
		}, cluster(name))

		expectClusterWritesAccepted(name)

		By("rejecting a change to the stored user")
		stored := latestCluster(name)
		stored.Spec.ACL.Users[0].RawAcl = "+@read"
		expectInvalid(k8sClient.Update(ctx, stored), "spec.acl.users[0].password.secret.name: Required value")

		By("rejecting a new user without password.secret.name, and only for that user")
		stored = latestCluster(name)
		stored.Spec.ACL.Users = append(stored.Spec.ACL.Users, withoutSecretName("bob"))
		err := k8sClient.Update(ctx, stored)
		expectInvalid(err, "spec.acl.users[1].password.secret.name: Required value")
		Expect(err.Error()).NotTo(ContainSubstring("spec.acl.users[0]"))

		By("rejecting a create with the user")
		expectInvalid(k8sClient.Create(ctx, cluster("stored-acl-user-new")), aclUserSecretMessage)
	})

	It("keeps a stored spec.users entry without passwordSecret.name", func() {
		const name = "stored-legacy-user"
		withoutSecretName := func(user string) valkeyiov1alpha1.UserAclSpec {
			return valkeyiov1alpha1.UserAclSpec{Name: user, Enabled: true}
		}
		cluster := func(name string) *valkeyiov1alpha1.ValkeyCluster {
			c := &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
			}
			c.Spec.Users = []valkeyiov1alpha1.UserAclSpec{withoutSecretName("carol")} //nolint:staticcheck // legacy spec.users
			return c
		}
		store(clustersCRD, []celRule{
			{path: []string{"properties", "users", "items"}, message: legacyUserSecretMessage},
			{path: nil, message: usersRejectMessage},
		}, cluster(name))

		expectClusterWritesAccepted(name)

		By("rejecting a change to the stored user")
		stored := latestCluster(name)
		stored.Spec.Users[0].RawAcl = "+@read" //nolint:staticcheck // legacy spec.users
		expectInvalid(k8sClient.Update(ctx, stored), "spec.users[0].passwordSecret.name: Required value")

		By("rejecting a new user without passwordSecret.name, and only for that user")
		stored = latestCluster(name)
		stored.Spec.Users = append(stored.Spec.Users, withoutSecretName("dave")) //nolint:staticcheck // legacy spec.users
		err := k8sClient.Update(ctx, stored)
		expectInvalid(err, "spec.users[1].passwordSecret.name: Required value")
		Expect(err.Error()).NotTo(ContainSubstring("spec.users[0]"))

		By("rejecting a create with the user")
		expectInvalid(k8sClient.Create(ctx, cluster("stored-legacy-user-new")), legacyUserSecretMessage)
	})
})
