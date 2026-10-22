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

// These run against the real API server because the rules on DatabasesAclSpec
// can only fail there. In CRD CEL, referencing an absent optional field is an
// error that fails validation for the whole object, so a rule touching ids
// without a has() guard rejects any databases block that sets allDbs or
// resetDbs without ids — which is every user of those two flags. A block with
// both flags false still passes, because CEL evaluates false && <error> to
// false, and a user that omits databases altogether never evaluates the rules
// at all; only a set flag exposes a missing guard. No unit test on the Go
// types can see any of this: it is the rendered schema, not the struct, that
// misbehaves.
var _ = Describe("users databases validation", Label("acl-databases", "cel"), func() {
	var (
		ctx     context.Context
		counter int
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	// applyWithUsers creates a cluster carrying users and returns the API
	// server's verdict.
	applyWithUsers := func(users ...valkeyiov1alpha1.UserAclSpec) error {
		counter++
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("acl-db-%d", counter),
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 0,
				Users:    users,
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

	// applyRawDatabases sends the databases block as unstructured content; a
	// nil block omits the key entirely. The typed client cannot express every
	// state the schema has to police. Databases is a struct, which
	// encoding/json never omits, so every typed request carries at least
	// "databases: {}". And IDs carries json:"ids,omitempty", so an empty Go
	// slice is dropped from the request body and is indistinguishable from an
	// unset field. A YAML or JSON client can send both an absent block and a
	// literal "ids: []", the case MinItems=1 exists to reject, so those cases
	// have to be driven from here.
	applyRawDatabases := func(databases map[string]any) error {
		counter++
		user := map[string]any{
			"name":    "alice",
			"enabled": true,
			"nopass":  true,
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
				"shards":   int64(1),
				"replicas": int64(0),
				"users":    []any{user},
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

	userWith := func(databases valkeyiov1alpha1.DatabasesAclSpec) valkeyiov1alpha1.UserAclSpec {
		return valkeyiov1alpha1.UserAclSpec{
			Name:       "alice",
			Enabled:    true,
			NoPassword: true,
			Databases:  databases,
		}
	}

	It("accepts a user that omits databases entirely", func() {
		// Sent raw, because the typed client cannot omit the block. This is
		// the shape of every user written before the field existed.
		Expect(applyRawDatabases(nil)).To(Succeed())
	})

	It("accepts an empty databases block", func() {
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{}))).To(Succeed())
	})

	// The has()-guard regression tests: a databases block that sets a flag
	// but carries no ids. Check these first when the rules change — an
	// unguarded rule makes allDbs and resetDbs unusable for everyone.
	It("accepts a flags-only block with ids absent", func() {
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{AllDbs: true}))).To(Succeed())
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{ResetDbs: true}))).To(Succeed())
	})

	It("accepts each directive on its own", func() {
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}}))).To(Succeed())
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0, 1, 2}}))).To(Succeed())
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{AllDbs: true}))).To(Succeed())
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{ResetDbs: true}))).To(Succeed())
	})

	It("accepts explicit false for both flags alongside ids", func() {
		// Defaulting writes allDbs=false and resetDbs=false into every present
		// databases block, so the mutual-exclusion rules must not trip on the
		// object the API server actually stores.
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{
			IDs:      []int32{0},
			AllDbs:   false,
			ResetDbs: false,
		}))).To(Succeed())
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
		// db= with no id is a syntax error that would break ACL LOAD for the
		// whole file, so the schema refuses an explicitly empty list.
		err := applyRawDatabases(map[string]any{"ids": []any{}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("should have at least 1 items"))
	})

	It("accepts an empty Go slice, which never reaches the API server", func() {
		// Not a contradiction with the case above: json:"ids,omitempty" drops
		// an empty slice from the request body, so this is an unset field by
		// the time the schema sees it. The renderer's own len() > 0 guard is
		// what keeps this state from emitting a bare "db=" — the schema cannot
		// reach it. See TestBuildUserAcl_DatabasesPrecedence.
		Expect(applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{}}))).To(Succeed())
	})

	It("rejects mutually exclusive directives sent as raw content", func() {
		// The typed path is covered above; this proves the rules also hold for
		// a kubectl-style apply, which is how users actually hit them.
		err := applyRawDatabases(map[string]any{"ids": []any{int64(0)}, "allDbs": true})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
	})

	It("rejects a negative database id", func() {
		err := applyWithUsers(userWith(valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{-1}}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("should be greater than or equal to 0"))
	})

	It("accepts a mix of users where only some set databases", func() {
		Expect(applyWithUsers(
			valkeyiov1alpha1.UserAclSpec{Name: "alice", Enabled: true, NoPassword: true},
			valkeyiov1alpha1.UserAclSpec{
				Name:       "bob",
				Enabled:    true,
				NoPassword: true,
				Databases:  valkeyiov1alpha1.DatabasesAclSpec{IDs: []int32{0}},
			},
		)).To(Succeed())
	})
})
