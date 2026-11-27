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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
				SharedConfigMapName: testSharedConfigMapName,
				ConfigRevision:      testConfigRevision,
				WorkloadType:        valkeyiov1alpha1.WorkloadTypeStatefulSet,
				Config:              map[string]string{"maxmemory": "100mb", "maxclients": "500"},
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
		Expect(r.setLiveConfigApplied(ctx, node, map[string]string{"maxmemory": "100mb", "maxclients": "500"})).Error().NotTo(HaveOccurred())

		got := read()
		Expect(got.Status.AppliedLiveConfig).To(Equal([]valkeyiov1alpha1.AppliedLiveConfigEntry{
			{Name: "maxclients", Value: "500"},
			{Name: "maxmemory", Value: "100mb"},
		}))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied)).To(BeTrue())
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(1))

		By("writing nothing when neither changed")
		Expect(r.setLiveConfigApplied(ctx, node, map[string]string{"maxmemory": "100mb", "maxclients": "500"})).Error().NotTo(HaveOccurred())
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(1))

		By("dropping a key removed from spec while others remain")
		Expect(r.setLiveConfigApplied(ctx, node, map[string]string{"maxmemory": "100mb"})).Error().NotTo(HaveOccurred())
		Expect(read().Status.AppliedLiveConfig).To(Equal([]valkeyiov1alpha1.AppliedLiveConfigEntry{{Name: "maxmemory", Value: "100mb"}}))
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(2))

		By("replacing a changed value")
		Expect(r.setLiveConfigApplied(ctx, node, map[string]string{"maxmemory": "200mb"})).Error().NotTo(HaveOccurred())
		Expect(read().Status.AppliedLiveConfig).To(Equal([]valkeyiov1alpha1.AppliedLiveConfigEntry{{Name: "maxmemory", Value: "200mb"}}))
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(3))
	})

	It("keeps the recorded values when CONFIG SET fails", func() {
		node.Status.AppliedLiveConfig = []valkeyiov1alpha1.AppliedLiveConfigEntry{{Name: "maxmemory", Value: "100mb"}}
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
		r.newConfigClient = func(context.Context, *ValkeyNodeReconciler, *valkeyiov1alpha1.ValkeyNode) (valkeyConfigClient, error) {
			return &fakeConfigClient{err: errors.New("ERR CONFIG SET failed")}, nil
		}

		stop, _, err := r.applyLiveSettings(ctx, read())
		Expect(err).To(HaveOccurred())
		Expect(stop).To(BeTrue())

		got := read()
		Expect(got.Status.AppliedLiveConfig).To(Equal([]valkeyiov1alpha1.AppliedLiveConfigEntry{{Name: "maxmemory", Value: "100mb"}}))
		Expect(meta.IsStatusConditionFalse(got.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied)).To(BeTrue())
	})

	It("clears the condition and the values when there is nothing to apply", func() {
		Expect(r.setLiveConfigApplied(ctx, node, map[string]string{"maxmemory": "100mb", "maxclients": "500"})).Error().NotTo(HaveOccurred())
		counter.Reset()

		Expect(r.clearLiveConfigApplied(ctx, node)).Error().NotTo(HaveOccurred())

		got := read()
		Expect(got.Status.AppliedLiveConfig).To(BeNil())
		Expect(meta.FindStatusCondition(got.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionLiveConfigApplied)).To(BeNil())
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(1))

		By("writing nothing when there is nothing to clear")
		Expect(r.clearLiveConfigApplied(ctx, node)).Error().NotTo(HaveOccurred())
		Expect(counter.Count("ValkeyNode", verbStatusPatch)).To(Equal(1))
	})
})
