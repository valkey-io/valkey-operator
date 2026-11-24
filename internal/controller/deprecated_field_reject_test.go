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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	replicasRejectMessage = "spec.replicas is deprecated and cannot be added or changed"
	usersRejectMessage    = "spec.users is deprecated and cannot be added or changed"
)

// withDeprecatedFieldRulesRemoved runs fn while the ValkeyCluster CRD does not
// reject a new spec.replicas or spec.users, so fn can store a spec that an
// older release accepted. The rules are restored before it returns.
func withDeprecatedFieldRulesRemoved(ctx context.Context, fn func()) {
	GinkgoHelper()
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "valkeyclusters.valkey.io"}, crd)).To(Succeed())
	original, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	Expect(err).NotTo(HaveOccurred())

	relaxed, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for i := range relaxed {
		version := relaxed[i].(map[string]any)
		path := []string{"schema", "openAPIV3Schema", "properties", "spec", "x-kubernetes-validations"}
		rules, _, _ := unstructured.NestedSlice(version, path...)
		kept := make([]any, 0, len(rules))
		for _, rule := range rules {
			msg, _ := rule.(map[string]any)["message"].(string)
			if strings.Contains(msg, "cannot be added or changed") {
				continue
			}
			kept = append(kept, rule)
		}
		Expect(unstructured.SetNestedSlice(version, kept, path...)).To(Succeed())
	}
	Expect(unstructured.SetNestedSlice(crd.Object, relaxed, "spec", "versions")).To(Succeed())
	Expect(k8sClient.Update(ctx, crd)).To(Succeed())

	probe := func() error {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("valkey.io/v1alpha1")
		u.SetKind("ValkeyCluster")
		u.SetName("deprecated-field-probe")
		u.SetNamespace("default")
		Expect(unstructured.SetNestedMap(u.Object, map[string]any{"shards": int64(1), "replicas": int64(1)}, "spec")).To(Succeed())
		return k8sClient.Create(ctx, u, client.DryRunAll)
	}
	defer func() {
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "valkeyclusters.valkey.io"}, crd)).To(Succeed())
		Expect(unstructured.SetNestedSlice(crd.Object, original, "spec", "versions")).To(Succeed())
		Expect(k8sClient.Update(ctx, crd)).To(Succeed())
		Eventually(probe).WithTimeout(30 * time.Second).Should(MatchError(ContainSubstring(replicasRejectMessage)))
	}()
	Eventually(probe).WithTimeout(30 * time.Second).Should(Succeed())
	fn()
}

var _ = Describe("rejected spec.replicas and spec.users", func() {
	ctx := context.Background()

	create := func(name string, spec map[string]any) error {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("valkey.io/v1alpha1")
		u.SetKind("ValkeyCluster")
		u.SetName(name)
		u.SetNamespace("default")
		Expect(unstructured.SetNestedMap(u.Object, spec, "spec")).To(Succeed())
		err := k8sClient.Create(ctx, u)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, u) })
		}
		return err
	}
	patch := func(name, body string) error {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("valkey.io/v1alpha1")
		u.SetKind("ValkeyCluster")
		u.SetName(name)
		u.SetNamespace("default")
		return k8sClient.Patch(ctx, u, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	expectInvalid := func(err error, message string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
		Expect(err.Error()).To(ContainSubstring(message))
	}

	It("rejects a create that sets spec.replicas, including 0", func() {
		expectInvalid(create("reject-replicas", map[string]any{"shards": int64(1), "replicas": int64(2)}), replicasRejectMessage)
		expectInvalid(create("reject-replicas-zero", map[string]any{"shards": int64(1), "replicas": int64(0)}), replicasRejectMessage)
	})

	It("rejects a create that sets spec.users and accepts an empty list", func() {
		alice := []any{map[string]any{"name": "alice", "nopass": true}}
		expectInvalid(create("reject-users", map[string]any{"shards": int64(1), "users": alice}), usersRejectMessage)
		Expect(create("reject-users-empty", map[string]any{"shards": int64(1), "users": []any{}})).To(Succeed())
	})

	It("keeps a stored spec.replicas, rejects a different count, and accepts the same count on replicasPerShard", func() {
		const name = "reject-replicas-stored"
		withDeprecatedFieldRulesRemoved(ctx, func() {
			Expect(create(name, map[string]any{"shards": int64(1), "replicas": int64(3)})).To(Succeed())
		})
		Expect(patch(name, `{"spec":{"shards":2}}`)).To(Succeed())
		expectInvalid(patch(name, `{"spec":{"replicas":1}}`), replicasRejectMessage)
		expectInvalid(patch(name, `{"spec":{"replicas":null,"replicasPerShard":1}}`), replicasRejectMessage)
		Expect(patch(name, `{"spec":{"replicas":null,"replicasPerShard":3}}`)).To(Succeed())
	})

	It("keeps a stored spec.users and rejects a clear that drops a user name", func() {
		const name = "reject-users-stored"
		withDeprecatedFieldRulesRemoved(ctx, func() {
			Expect(create(name, map[string]any{
				"shards": int64(1),
				"users":  []any{map[string]any{"name": "alice", "nopass": true}},
			})).To(Succeed())
		})
		Expect(patch(name, `{"spec":{"shards":2}}`)).To(Succeed())
		expectInvalid(patch(name, `{"spec":{"users":null}}`), usersRejectMessage)
		moved := `{"spec":{"users":null,"acl":{"users":[{"name":"alice","password":{"type":"Any"}}]}}}`
		Expect(patch(name, moved)).To(Succeed())
	})
})
