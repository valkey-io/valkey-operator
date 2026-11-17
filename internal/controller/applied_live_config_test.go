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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("status.appliedLiveConfig", func() {
	var (
		ctx     context.Context
		node    *valkeyiov1alpha1.ValkeyNode
		counter *writeCountingClient
		r       *ValkeyNodeReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		node = &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "applied-live-config", Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{
				WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet,
				Config:       map[string]string{"maxmemory": "100mb", "maxclients": "500"},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		counter = newWriteCountingClient(k8sClient)
		r = &ValkeyNodeReconciler{
			Client:    counter,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, node)).To(Succeed())
	})

	read := func() *valkeyiov1alpha1.ValkeyNode {
		out := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), out)).To(Succeed())
		return out
	}

	It("records the applied values with LiveConfigApplied=True in one write", func() {
		Expect(r.setLiveConfigApplied(ctx, node, map[string]string{"maxmemory": "100mb", "maxclients": "500"})).To(Succeed())

		got := read()
		Expect(got.Status.AppliedLiveConfig).To(Equal([]valkeyiov1alpha1.ConfigEntry{
			{Name: "maxclients", Value: "500"},
			{Name: "maxmemory", Value: "100mb"},
		}))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied)).To(BeTrue())
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(1))

		By("writing nothing when neither changed")
		Expect(r.setLiveConfigApplied(ctx, node, map[string]string{"maxmemory": "100mb", "maxclients": "500"})).To(Succeed())
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(1))

		By("replacing the values when the applied set changes")
		Expect(r.setLiveConfigApplied(ctx, node, map[string]string{"maxmemory": "200mb"})).To(Succeed())
		Expect(read().Status.AppliedLiveConfig).To(Equal([]valkeyiov1alpha1.ConfigEntry{{Name: "maxmemory", Value: "200mb"}}))
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(2))
	})

	It("prunes keys no longer in spec when there is nothing to apply", func() {
		Expect(r.setLiveConfigApplied(ctx, node, map[string]string{"maxmemory": "100mb", "maxclients": "500"})).To(Succeed())
		counter.Reset()

		node.Spec.Config = map[string]string{"maxclients": "500"}
		Expect(r.clearLiveConfigApplied(ctx, node)).To(Succeed())

		got := read()
		Expect(got.Status.AppliedLiveConfig).To(Equal([]valkeyiov1alpha1.ConfigEntry{{Name: "maxclients", Value: "500"}}))
		Expect(meta.FindStatusCondition(got.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied)).To(BeNil())
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(1))

		By("clearing the field once no key is left")
		node.Spec.Config = nil
		Expect(r.clearLiveConfigApplied(ctx, node)).To(Succeed())
		Expect(read().Status.AppliedLiveConfig).To(BeEmpty())
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(2))

		By("writing nothing when there is nothing to prune")
		Expect(r.clearLiveConfigApplied(ctx, node)).To(Succeed())
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(2))
	})
})

var _ = Describe("writeCountingClient", func() {
	It("counts writes by kind and verb", func() {
		ctx := context.Background()
		counter := newWriteCountingClient(k8sClient)
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "write-counter", Namespace: "default"}}
		Expect(counter.Create(ctx, cm)).To(Succeed())
		cm.Data = map[string]string{"k": "v"}
		Expect(counter.Update(ctx, cm)).To(Succeed())
		Expect(counter.Delete(ctx, cm)).To(Succeed())

		Expect(counter.Counts()).To(Equal(map[writeKey]int{
			{Kind: "ConfigMap", Verb: verbCreate}: 1,
			{Kind: "ConfigMap", Verb: verbUpdate}: 1,
			{Kind: "ConfigMap", Verb: verbDelete}: 1,
		}))
	})
})
