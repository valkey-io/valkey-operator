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
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("spec.acl validation", func() {
	var (
		ctx     context.Context
		counter int
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	// create submits a 1-shard cluster with the given spec mutation and
	// returns the created object and the API server's verdict.
	create := func(mutate func(*valkeyiov1alpha1.ValkeyClusterSpec)) (*valkeyiov1alpha1.ValkeyCluster, error) {
		counter++
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("acl-spec-%d", counter),
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
		}
		mutate(&cluster.Spec)
		err := k8sClient.Create(ctx, cluster)
		if err == nil {
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
			})
		}
		return cluster, err
	}

	aclUsers := func(users ...valkeyiov1alpha1.ACLUserSpec) func(*valkeyiov1alpha1.ValkeyClusterSpec) {
		return func(s *valkeyiov1alpha1.ValkeyClusterSpec) {
			s.ACL = &valkeyiov1alpha1.ACLSpec{Users: users}
		}
	}

	legacyUser := valkeyiov1alpha1.UserAclSpec{Name: "bob", Enabled: true, NoPassword: true}

	legacyUsers := func(users ...valkeyiov1alpha1.UserAclSpec) func(*valkeyiov1alpha1.ValkeyClusterSpec) {
		return func(s *valkeyiov1alpha1.ValkeyClusterSpec) {
			s.Users = users //nolint:staticcheck // legacy spec.users
		}
	}

	readBack := func(cluster *valkeyiov1alpha1.ValkeyCluster) valkeyiov1alpha1.ACLUserSpec {
		got := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, got)).To(Succeed())
		Expect(got.Spec.ACL).NotTo(BeNil())
		Expect(got.Spec.ACL.Users).To(HaveLen(1))
		return got.Spec.ACL.Users[0]
	}

	It("accepts spec.acl.users alone", func() {
		cluster, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{
			Name:     "alice",
			Password: &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny},
		}))
		Expect(err).NotTo(HaveOccurred())
		user := readBack(cluster)
		Expect(user.State).To(Equal(valkeyiov1alpha1.ACLUserStateEnabled))
		Expect(user.Password).NotTo(BeNil())
		Expect(user.Password.Type).To(Equal(valkeyiov1alpha1.ACLPasswordTypeAny))
	})

	It("defaults state and password.type", func() {
		cluster, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{
			Name:     "alice",
			Password: &valkeyiov1alpha1.ACLUserPassword{Secret: &valkeyiov1alpha1.PasswordSecretSpec{Name: "users"}},
		}))
		Expect(err).NotTo(HaveOccurred())
		user := readBack(cluster)
		Expect(user.State).To(Equal(valkeyiov1alpha1.ACLUserStateEnabled))
		Expect(user.Password).NotTo(BeNil())
		Expect(user.Password.Type).To(Equal(valkeyiov1alpha1.ACLPasswordTypeSecret))
	})

	It("defaults an omitted password to type Secret", func() {
		cluster, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: "alice", State: valkeyiov1alpha1.ACLUserStateDisabled}))
		Expect(err).NotTo(HaveOccurred())
		user := readBack(cluster)
		Expect(user.Password).NotTo(BeNil())
		Expect(user.Password.Type).To(Equal(valkeyiov1alpha1.ACLPasswordTypeSecret))
	})

	It("requires password.secret.name for an enabled user with Secret passwords", func() {
		for desc, password := range map[string]*valkeyiov1alpha1.ACLUserPassword{
			"no password": nil,
			"no secret":   {Type: valkeyiov1alpha1.ACLPasswordTypeSecret},
			"only keys":   {Secret: &valkeyiov1alpha1.PasswordSecretSpec{Keys: []string{"alicepw"}}},
		} {
			_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: "alice", Password: password}))
			Expect(err).To(HaveOccurred(), desc)
			Expect(err.Error()).To(ContainSubstring("spec.acl.users[0].password.secret.name: Required value"), desc)
			Expect(err.Error()).To(ContainSubstring("password.secret.name is required for an enabled user whose password.type is Secret"), desc)
		}
	})

	// A typed client drops name: "" through omitempty; kubectl sends it.
	It("rejects an empty password.secret.name", func() {
		counter++
		cluster := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": valkeyiov1alpha1.GroupVersion.String(),
			"kind":       "ValkeyCluster",
			"metadata":   map[string]any{"name": fmt.Sprintf("acl-spec-%d", counter), "namespace": "default"},
			"spec": map[string]any{
				"shards": int64(1),
				"acl": map[string]any{"users": []any{map[string]any{
					"name":     "alice",
					"password": map[string]any{"secret": map[string]any{"name": ""}},
				}}},
			},
		}}
		err := k8sClient.Create(ctx, cluster)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		}
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.acl.users[0].password.secret.name: Required value"))
	})

	It("does not require a Secret for a disabled user or one without Secret passwords", func() {
		for desc, user := range map[string]valkeyiov1alpha1.ACLUserSpec{
			"disabled":      {Name: "alice", State: valkeyiov1alpha1.ACLUserStateDisabled},
			"type Any":      {Name: "alice", Password: &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny}},
			"type Disabled": {Name: "alice", Password: &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeDisabled}},
		} {
			_, err := create(aclUsers(user))
			Expect(err).NotTo(HaveOccurred(), desc)
		}
	})

	It("requires passwordSecret.name for an enabled legacy user with Secret passwords", func() {
		for desc, legacy := range map[string]valkeyiov1alpha1.UserAclSpec{
			"no passwordSecret": {Name: "alice", Enabled: true},
			"only keys":         {Name: "alice", Enabled: true, PasswordSecret: valkeyiov1alpha1.PasswordSecretSpec{Keys: []string{"alicepw"}}},
		} {
			_, err := create(legacyUsers(legacy))
			Expect(err).To(HaveOccurred(), desc)
			Expect(err.Error()).To(ContainSubstring("spec.users[0].passwordSecret.name: Required value"), desc)
		}
		for desc, legacy := range map[string]valkeyiov1alpha1.UserAclSpec{
			"named":     {Name: "alice", Enabled: true, PasswordSecret: valkeyiov1alpha1.PasswordSecretSpec{Name: "users"}},
			"nopass":    {Name: "alice", Enabled: true, NoPassword: true},
			"resetpass": {Name: "alice", Enabled: true, ResetPass: true},
		} {
			_, err := create(legacyUsers(legacy))
			Expect(err).NotTo(HaveOccurred(), desc)
		}
	})

	It("accepts password.secret when password.type is Secret", func() {
		_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{
			Name: "alice",
			Password: &valkeyiov1alpha1.ACLUserPassword{
				Secret: &valkeyiov1alpha1.PasswordSecretSpec{Name: "users", Keys: []string{"alicepw"}},
			},
		}))
		Expect(err).NotTo(HaveOccurred())
	})

	It("accepts legacy spec.users alone", func() {
		_, err := create(func(s *valkeyiov1alpha1.ValkeyClusterSpec) {
			s.Users = []valkeyiov1alpha1.UserAclSpec{legacyUser} //nolint:staticcheck // legacy spec.users
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// A typed client drops users: [] through omitempty, so send it unstructured
	// the way kubectl and Helm do.
	It("accepts an empty spec.users next to spec.acl.users", func() {
		counter++
		cluster := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": valkeyiov1alpha1.GroupVersion.String(),
			"kind":       "ValkeyCluster",
			"metadata": map[string]any{
				"name":      fmt.Sprintf("acl-spec-%d", counter),
				"namespace": "default",
			},
			"spec": map[string]any{
				"shards": int64(1),
				"users":  []any{},
				"acl": map[string]any{
					"users": []any{map[string]any{"name": "alice", "password": map[string]any{"type": "Any"}}},
				},
			},
		}}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
	})

	It("rejects spec.users combined with spec.acl.users", func() {
		_, err := create(func(s *valkeyiov1alpha1.ValkeyClusterSpec) {
			s.Users = []valkeyiov1alpha1.UserAclSpec{legacyUser} //nolint:staticcheck // legacy spec.users
			aclUsers(valkeyiov1alpha1.ACLUserSpec{
				Name:     "alice",
				Password: &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny},
			})(s)
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.acl.users: Forbidden"))
		Expect(err.Error()).To(ContainSubstring("cannot be combined with spec.acl.users"))
	})

	It("rejects password.secret when password.type is not Secret", func() {
		for _, typ := range []valkeyiov1alpha1.ACLPasswordType{
			valkeyiov1alpha1.ACLPasswordTypeAny,
			valkeyiov1alpha1.ACLPasswordTypeDisabled,
		} {
			_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{
				Name: "alice",
				Password: &valkeyiov1alpha1.ACLUserPassword{
					Type:   typ,
					Secret: &valkeyiov1alpha1.PasswordSecretSpec{Name: "users"},
				},
			}))
			Expect(err).To(HaveOccurred(), "password.type %s", typ)
			Expect(err.Error()).To(ContainSubstring("spec.acl.users[0].password.secret: Forbidden"))
			Expect(err.Error()).To(ContainSubstring("secret is only used when type is Secret"))
		}
	})

	It("rejects an unknown password.type", func() {
		_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{
			Name:     "alice",
			Password: &valkeyiov1alpha1.ACLUserPassword{Type: "None"},
		}))
		Expect(err).To(HaveOccurred())
	})

	It("rejects an unknown state", func() {
		_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: "alice", State: "Off"}))
		Expect(err).To(HaveOccurred())
	})

	It("rejects an empty username", func() {
		_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: ""}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.acl.users[0].name"))
	})

	It("rejects whitespace in a username", func() {
		for _, name := range []string{"bad name", "eve\nuser _injected on nopass +@all ~*", "tab\tname"} {
			_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: name}))
			Expect(err).To(HaveOccurred(), "name %q", name)
			Expect(err.Error()).To(ContainSubstring("spec.acl.users[0].name"))
		}
	})

	It("limits a username to 128 characters", func() {
		_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{
			Name:     strings.Repeat("a", 128),
			Password: &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny},
		}))
		Expect(err).NotTo(HaveOccurred())
		_, err = create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: strings.Repeat("a", 129)}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.acl.users[0].name"))
	})

	It("rejects a newline in permissions", func() {
		_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{
			Name:           "alice",
			ACLPermissions: valkeyiov1alpha1.ACLPermissions{RawAcl: "+ping\nuser _injected on nopass +@all"},
		}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.acl.users[0].permissions"))
	})

	It("accepts a trailing newline in permissions", func() {
		_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{
			Name:           "alice",
			Password:       &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny},
			ACLPermissions: valkeyiov1alpha1.ACLPermissions{RawAcl: "+ping\n"},
		}))
		Expect(err).NotTo(HaveOccurred())
		legacy := legacyUser
		legacy.RawAcl = "+ping\n"
		_, err = create(legacyUsers(legacy))
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects invalid legacy usernames", func() {
		for _, name := range []string{"", "bad name", "eve\nuser _injected on nopass +@all ~*", "tab\tname", strings.Repeat("a", 129)} {
			legacy := legacyUser
			legacy.Name = name
			_, err := create(legacyUsers(legacy))
			Expect(err).To(HaveOccurred(), "name %q", name)
			Expect(err.Error()).To(ContainSubstring("spec.users[0].name"), "name %q", name)
		}
		legacy := legacyUser
		legacy.Name = strings.Repeat("a", 128)
		_, err := create(legacyUsers(legacy))
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects whitespace in key and channel patterns", func() {
		bad := []string{"app:* +@all"}
		for field, perms := range map[string]valkeyiov1alpha1.ACLPermissions{
			"keys.readWrite":    {Keys: valkeyiov1alpha1.KeysAclSpec{ReadWrite: bad}},
			"keys.readOnly":     {Keys: valkeyiov1alpha1.KeysAclSpec{ReadOnly: bad}},
			"keys.writeOnly":    {Keys: valkeyiov1alpha1.KeysAclSpec{WriteOnly: bad}},
			"channels.patterns": {Channels: valkeyiov1alpha1.ChannelsAclSpec{Patterns: bad}},
		} {
			_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: "alice", ACLPermissions: perms}))
			Expect(err).To(HaveOccurred(), field)
			Expect(err.Error()).To(ContainSubstring("spec.acl.users[0]."+field+"[0]"), field)
		}
	})

	It("rejects a username starting with _", func() {
		_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: "_alice"}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Username cannot start with '_'"))
	})
})

// A cluster stored before password.secret.name was required keeps its user
// without one: CRD validation ratcheting skips a rule on a list item that an
// update leaves unchanged. To store such a cluster, the rule is taken out of
// the installed CRD and then put back.
var _ = Describe("password.secret.name on a cluster stored before it was required", func() {
	ctx := context.Background()
	crdKey := client.ObjectKey{Name: "valkeyclusters.valkey.io"}
	getCRD := func(g Gomega) *unstructured.Unstructured {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		g.Expect(k8sClient.Get(ctx, crdKey, crd)).To(Succeed())
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
	userWithoutName := func(name string) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, ACL: &valkeyiov1alpha1.ACLSpec{Users: []valkeyiov1alpha1.ACLUserSpec{{
				Name:     "alice",
				Password: &valkeyiov1alpha1.ACLUserPassword{Secret: &valkeyiov1alpha1.PasswordSecretSpec{Keys: []string{"alicepw"}}},
			}}}},
		}
	}

	It("accepts updates that leave that user unchanged, and rejects one that changes it", func() {
		saved, _, err := unstructured.NestedSlice(getCRD(Default).Object, "spec", "versions")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { setVersions(saved) })

		By("storing the cluster while the installed CRD lacks the rule")
		relaxed, _, err := unstructured.NestedSlice(getCRD(Default).Object, "spec", "versions")
		Expect(err).NotTo(HaveOccurred())
		itemPath := []string{"schema", "openAPIV3Schema", "properties", "spec", "properties", "acl", "properties", "users", "items", "x-kubernetes-validations"}
		version := relaxed[0].(map[string]any)
		rules, found, err := unstructured.NestedSlice(version, itemPath...)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		rules = slices.DeleteFunc(rules, func(rule any) bool {
			message, _ := rule.(map[string]any)["message"].(string)
			return strings.HasPrefix(message, "password.secret.name is required")
		})
		Expect(unstructured.SetNestedSlice(version, rules, itemPath...)).To(Succeed())
		setVersions(relaxed)
		stored := userWithoutName("acl-ratchet")
		Eventually(func() error { return k8sClient.Create(ctx, stored.DeepCopy()) }).WithTimeout(30 * time.Second).Should(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, stored) })

		By("putting the rule back")
		setVersions(saved)
		Eventually(func() error {
			probe := userWithoutName("acl-ratchet-probe")
			if err := k8sClient.Create(ctx, probe); err != nil {
				return nil
			}
			_ = k8sClient.Delete(ctx, probe)
			return errors.New("the API server does not enforce the rule yet")
		}).WithTimeout(30 * time.Second).Should(Succeed())

		By("accepting an update that leaves the user alone")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stored), stored)).To(Succeed())
		stored.Spec.Shards = 2
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())

		By("rejecting an update to the user that does not add the name")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stored), stored)).To(Succeed())
		stored.Spec.ACL.Users[0].RawAcl = "+@read"
		err = k8sClient.Update(ctx, stored)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.acl.users[0].password.secret.name: Required value"))

		By("accepting the user once it names its Secret")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stored), stored)).To(Succeed())
		stored.Spec.ACL.Users[0].Password.Secret.Name = "users"
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())
	})
})
