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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("users databases validation", Label("acl-databases", "cel"), func() {
	var (
		ctx     context.Context
		counter int
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	applyWithUsers := func(users ...valkeyiov1alpha1.ACLUserSpec) error {
		counter++
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("acl-db-%d", counter),
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:           1,
				ReplicasPerShard: 0,
				ACL:              &valkeyiov1alpha1.ACLSpec{Users: users},
			},
		}
		err := k8sClient.Create(ctx, cluster)
		if err == nil {
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
			})
		}
		return err
	}

	applyWithLegacyUsers := func(users ...valkeyiov1alpha1.UserAclSpec) error {
		counter++
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("acl-db-legacy-%d", counter),
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:           1,
				ReplicasPerShard: 0,
				Users:            users, //nolint:staticcheck // legacy spec.users
			},
		}
		err := k8sClient.Create(ctx, cluster)
		if err == nil {
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
			})
		}
		return err
	}

	// For shapes the typed client cannot send: no block (nil) or a literal "ids: []".
	applyRawDatabases := func(databases map[string]any) error {
		counter++
		user := map[string]any{
			"name":     "alice",
			"password": map[string]any{"type": "Any"},
		}
		if databases != nil {
			user["databases"] = databases
		}
		cluster := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": valkeyiov1alpha1.GroupVersion.String(),
			"kind":       "ValkeyCluster",
			"metadata": map[string]any{
				"name":      fmt.Sprintf("acl-db-raw-%d", counter),
				"namespace": "default",
			},
			"spec": map[string]any{
				"shards": int64(1),
				"acl":    map[string]any{"users": []any{user}},
			},
		}}
		err := k8sClient.Create(ctx, cluster)
		if err == nil {
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
			})
		}
		return err
	}

	anyPassword := func() *valkeyiov1alpha1.ACLUserPassword {
		return &valkeyiov1alpha1.ACLUserPassword{Type: valkeyiov1alpha1.ACLPasswordTypeAny}
	}

	userWith := func(databases valkeyiov1alpha1.DatabasesAclSpec) valkeyiov1alpha1.ACLUserSpec {
		return valkeyiov1alpha1.ACLUserSpec{
			Name:           "alice",
			Password:       anyPassword(),
			ACLPermissions: valkeyiov1alpha1.ACLPermissions{Databases: databases},
		}
	}

	It("accepts a user that omits databases entirely", func() {
		Expect(applyRawDatabases(nil)).To(Succeed())
	})

	It("accepts an empty databases block", func() {
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{}))).To(Succeed())
	})

	It("accepts each directive on its own", func() {
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}}))).To(Succeed())
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0, 1, 2}}))).To(Succeed())
		// has() guard regressions: a flag set, ids absent.
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{AllDbs: true}))).To(Succeed())
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{ResetDbs: true}))).To(Succeed())
	})

	It("rejects ids combined with allDbs", func() {
		err := applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}, AllDbs: true}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("databases.ids and databases.allDbs are mutually exclusive"))
	})

	It("rejects ids combined with resetDbs", func() {
		err := applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}, ResetDbs: true}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("databases.ids and databases.resetDbs are mutually exclusive"))
	})

	It("rejects allDbs combined with resetDbs", func() {
		err := applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{AllDbs: true, ResetDbs: true}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("databases.allDbs and databases.resetDbs are mutually exclusive"))
	})

	It("rejects a literal empty ids list", func() {
		// A bare db= would break ACL LOAD.
		err := applyRawDatabases(map[string]any{"ids": []any{}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("should have at least 1 items"))
	})

	It("rejects a negative database id", func() {
		err := applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{-1}}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("should be greater than or equal to 0"))
	})

	It("accepts a mix of users where only some set databases", func() {
		Expect(applyWithUsers(
			valkeyiov1alpha1.ACLUserSpec{Name: "alice", Password: anyPassword()},
			valkeyiov1alpha1.ACLUserSpec{
				Name:     "bob",
				Password: anyPassword(),
				ACLPermissions: valkeyiov1alpha1.ACLPermissions{
					Databases: valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}},
				},
			},
		)).To(Succeed())
	})

	It("enforces the same rules under the deprecated spec.users", func() {
		legacy := func(databases valkeyiov1alpha1.DatabasesAclSpec) valkeyiov1alpha1.UserAclSpec {
			return valkeyiov1alpha1.UserAclSpec{
				Name:           "alice",
				Enabled:        true,
				NoPassword:     true,
				ACLPermissions: valkeyiov1alpha1.ACLPermissions{Databases: databases},
			}
		}
		Expect(applyWithLegacyUsers(legacy(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}}))).To(Succeed())
		err := applyWithLegacyUsers(legacy(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}, AllDbs: true}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("databases.ids and databases.allDbs are mutually exclusive"))
	})
})
