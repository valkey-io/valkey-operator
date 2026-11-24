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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("ValkeyNode without a known parent", func() {
	ctx := context.Background()

	cleanup := func(node *valkeyiov1alpha1.ValkeyNode) {
		child := metav1.ObjectMeta{Name: valkeyNodeResourceName(node), Namespace: node.Namespace}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), node); err == nil {
			node.Finalizers = nil
			Expect(k8sClient.Update(ctx, node)).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		}
		_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: child})
		_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: child})
	}

	readyCondition := func(key types.NamespacedName) (*valkeyiov1alpha1.ValkeyNode, *metav1.Condition) {
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
		return node, meta.FindStatusCondition(node.Status.Conditions, valkeyiov1alpha1.ValkeyNodeConditionReady)
	}

	It("reports NoParent once and creates nothing", func() {
		node := &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "no-parent", Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyNodeSpec{SharedConfigMapName: testSharedConfigMapName, ConfigRevision: testConfigRevision, WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(cleanup, node.DeepCopy())
		key := client.ObjectKeyFromObject(node)

		recorder := events.NewFakeRecorder(10)
		r := &ValkeyNodeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
		var ready *metav1.Condition
		for range 2 {
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(ctrl.Result{}))
		}

		node, ready = readyCondition(key)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonNoParent))
		Expect(node.Status.Ready).To(BeFalse())
		Expect(node.Status.ObservedGeneration).To(Equal(node.Generation))

		name := types.NamespacedName{Name: valkeyNodeResourceName(node), Namespace: key.Namespace}
		for _, obj := range []client.Object{&appsv1.StatefulSet{}, &appsv1.Deployment{}, &corev1.ConfigMap{}, &corev1.Service{}} {
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, name, obj))).To(BeTrue(), "%T %s must not exist", obj, name)
		}
		Expect(recorder.Events).To(HaveLen(1))
		Expect(<-recorder.Events).To(HavePrefix("Warning NoParent "))

		By("following a spec change in observedGeneration without a second event")
		node.Spec.Image = "valkey/valkey:9.0.1"
		Expect(k8sClient.Update(ctx, node)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		node, _ = readyCondition(key)
		Expect(node.Status.ObservedGeneration).To(Equal(node.Generation))
		Expect(recorder.Events).To(BeEmpty())
	})

	It("leaves the workload of an orphaned node unchanged", func() {
		node := &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "orphaned", Namespace: "default", OwnerReferences: testParentRefs()},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{
				SharedConfigMapName: testSharedConfigMapName,
				ConfigRevision:      testConfigRevision,
				WorkloadType:        valkeyiov1alpha1.WorkloadTypeStatefulSet,
				Image:               "valkey/valkey:9.0.0",
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(cleanup, node.DeepCopy())
		key := client.ObjectKeyFromObject(node)

		r := &ValkeyNodeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: events.NewFakeRecorder(10)}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		name := types.NamespacedName{Name: valkeyNodeResourceName(node), Namespace: key.Namespace}
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, name, sts)).To(Succeed())
		template := sts.Spec.Template.DeepCopy()
		generation := sts.Generation

		By("removing the owner and authorizing a new image")
		Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
		node.OwnerReferences = nil
		node.Spec.Image = "valkey/valkey:9.0.1"
		Expect(setDesiredWorkloadRevision(node)).To(Succeed())
		Expect(k8sClient.Update(ctx, node)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, name, sts)).To(Succeed())
		Expect(sts.Generation).To(Equal(generation))
		Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("valkey/valkey:9.0.0"))
		Expect(podTemplateRollHash(sts.Spec.Template)).To(Equal(podTemplateRollHash(*template)))
		_, ready := readyCondition(key)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(valkeyiov1alpha1.ValkeyNodeReasonNoParent))
	})
})
