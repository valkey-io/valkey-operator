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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("spec.replicasPerShard and the deprecated spec.replicas", func() {
	const bothSet = "spec.replicas is deprecated and cannot be combined with spec.replicasPerShard"

	ctx := context.Background()

	cleanup := func(name string) {
		DeferCleanup(func() {
			nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
			_ = k8sClient.List(ctx, nodeList, client.InNamespace("default"), client.MatchingLabels{LabelCluster: name})
			for i := range nodeList.Items {
				_ = k8sClient.Delete(ctx, &nodeList.Items[i])
			}
			_ = k8sClient.Delete(ctx, &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		})
	}

	// createSpec creates a ValkeyCluster from a raw spec, so a field set to 0
	// reaches the API server the way applied YAML sends it. A typed client
	// drops it through omitempty.
	createSpec := func(name string, spec map[string]any) error {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("valkey.io/v1alpha1")
		u.SetKind("ValkeyCluster")
		u.SetName(name)
		u.SetNamespace("default")
		Expect(unstructured.SetNestedMap(u.Object, spec, "spec")).To(Succeed())
		err := k8sClient.Create(ctx, u)
		if err == nil {
			cleanup(name)
		}
		return err
	}

	expectInvalid := func(err error) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
		Expect(err.Error()).To(ContainSubstring(bothSet))
	}

	It("accepts either field alone", func() {
		Expect(createSpec("rps-new-only", map[string]any{"shards": int64(1), "replicasPerShard": int64(1)})).To(Succeed())
		Expect(createSpec("rps-old-only", map[string]any{"shards": int64(1), "replicas": int64(1)})).To(Succeed())
	})

	It("rejects a spec that sets both fields, even when one is 0", func() {
		expectInvalid(createSpec("rps-both", map[string]any{"shards": int64(1), "replicas": int64(1), "replicasPerShard": int64(1)}))
		expectInvalid(createSpec("rps-both-zero", map[string]any{"shards": int64(1), "replicas": int64(0), "replicasPerShard": int64(1)}))
	})

	It("rejects an update that adds replicasPerShard without removing replicas", func() {
		const name = "rps-add-keep"
		Expect(createSpec(name, map[string]any{"shards": int64(1), "replicas": int64(1)})).To(Succeed())
		cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		expectInvalid(k8sClient.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicasPerShard":1}}`))))
	})

	It("warns about spec.replicas until the spec moves to replicasPerShard, without touching the ValkeyNodes", func() {
		const name = "rps-migrate"
		key := types.NamespacedName{Name: name, Namespace: "default"}
		Expect(createSpec(name, map[string]any{"shards": int64(1), "replicas": int64(1)})).To(Succeed())

		recorder := events.NewFakeRecorder(100)
		r := &ValkeyClusterReconciler{
			Client:        k8sClient,
			APIReader:     k8sClient,
			Scheme:        k8sClient.Scheme(),
			Recorder:      recorder,
			ValkeyClients: testValkeyClients(k8sClient),
		}
		reconcileOnce := func() *valkeyiov1alpha1.ValkeyCluster {
			GinkgoHelper()
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			cluster := &valkeyiov1alpha1.ValkeyCluster{}
			Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
			return cluster
		}
		listNodes := func() map[string]metav1.ObjectMeta {
			GinkgoHelper()
			nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
			Expect(k8sClient.List(ctx, nodeList, client.InNamespace("default"), client.MatchingLabels{LabelCluster: name})).To(Succeed())
			nodes := map[string]metav1.ObjectMeta{}
			for _, n := range nodeList.Items {
				nodes[n.Name] = n.ObjectMeta
			}
			return nodes
		}

		By("reconciling the legacy spec twice")
		reconcileOnce()
		cluster := reconcileOnce()
		cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonDeprecatedField))
		Expect(cond.Message).To(ContainSubstring("set spec.replicasPerShard: 1 and remove spec.replicas in the same update"))
		var deprecations []string
		for _, e := range collectEvents(recorder) {
			if e == "Warning "+valkeyiov1alpha1.ReasonDeprecatedField+" "+cond.Message {
				deprecations = append(deprecations, e)
			}
		}
		Expect(deprecations).To(HaveLen(1), "the event fires once, not on every reconcile")

		before := listNodes()
		Expect(before).To(HaveKey(valkeyNodeName(name, 0, 0)))
		Expect(before).To(HaveKey(valkeyNodeName(name, 0, 1)))

		By("moving the spec to replicasPerShard in one patch")
		Expect(k8sClient.Patch(ctx, cluster, client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"replicas":null,"replicasPerShard":1}}`)))).To(Succeed())
		cluster = reconcileOnce()
		Expect(meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)).To(BeNil())

		after := listNodes()
		Expect(after).To(HaveLen(len(before)))
		for nodeName, b := range before {
			Expect(after).To(HaveKey(nodeName))
			Expect(after[nodeName].UID).To(Equal(b.UID), "ValkeyNode %s was recreated", nodeName)
			Expect(after[nodeName].Generation).To(Equal(b.Generation), "ValkeyNode %s spec changed", nodeName)
		}
	})
})
