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
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

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
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, Replicas: 0},
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
		cluster, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: "alice"}))
		Expect(err).NotTo(HaveOccurred())
		user := readBack(cluster)
		Expect(user.State).To(Equal(valkeyiov1alpha1.ACLUserStateEnabled))
		Expect(user.Password).NotTo(BeNil())
		Expect(user.Password.Type).To(Equal(valkeyiov1alpha1.ACLPasswordTypeSecret))
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
		_, err := create(aclUsers(valkeyiov1alpha1.ACLUserSpec{Name: strings.Repeat("a", 128)}))
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
