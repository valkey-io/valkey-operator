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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("Pause reconciliation", func() {
	ctx := context.Background()
	pauseOn := map[string]string{valkeyiov1alpha1.AnnotationPauseReconciliation: "true"}

	deleteNode := func(key types.NamespacedName) {
		node := &valkeyiov1alpha1.ValkeyNode{}
		if err := k8sClient.Get(ctx, key, node); err == nil {
			node.Finalizers = nil
			Expect(k8sClient.Update(ctx, node)).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		}
	}

	It("leaves a paused cluster alone, marks its nodes, and clears the marks on resume", func() {
		key := types.NamespacedName{Name: "paused-cluster", Namespace: "default"}
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Annotations: pauseOn},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 3, Replicas: 1},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		nodeKey := types.NamespacedName{Name: "paused-cluster-0-0", Namespace: "default"}
		owned := &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: nodeKey.Name, Namespace: nodeKey.Namespace,
				Labels: map[string]string{LabelCluster: key.Name}},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet},
		}
		Expect(controllerutil.SetControllerReference(cluster, owned, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, owned)).To(Succeed())
		By("adding a standalone node that carries the cluster label but no owner reference")
		strayKey := types.NamespacedName{Name: "paused-cluster-stray", Namespace: "default"}
		Expect(k8sClient.Create(ctx, &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: strayKey.Name, Namespace: strayKey.Namespace,
				Labels: map[string]string{LabelCluster: key.Name}},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet},
		})).To(Succeed())
		DeferCleanup(func() {
			deleteNode(nodeKey)
			deleteNode(strayKey)
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &valkeyiov1alpha1.ValkeyCluster{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}))).To(Succeed())
		})

		recorder := events.NewFakeRecorder(100)
		r := &ValkeyClusterReconciler{
			Client:        k8sClient,
			APIReader:     k8sClient,
			Scheme:        k8sClient.Scheme(),
			Recorder:      recorder,
			ValkeyClients: testValkeyClients(k8sClient),
		}

		By("reconciling the paused cluster twice")
		for range 2 {
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(pausedRequeue))
		}
		Expect(recorder.Events).To(HaveLen(1))
		Expect(<-recorder.Events).To(ContainSubstring("ReconciliationPaused"))

		Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
		cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonReconciliationPaused))

		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, nodeKey, node)).To(Succeed())
		Expect(node.Annotations).To(HaveKeyWithValue(valkeyiov1alpha1.AnnotationPauseReconciliation, "true"))
		stray := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, strayKey, stray)).To(Succeed())
		Expect(stray.Annotations).NotTo(HaveKey(valkeyiov1alpha1.AnnotationPauseReconciliation))

		By("checking nothing else was created")
		nodes := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(ctx, nodes, client.InNamespace("default"), client.MatchingLabels{LabelCluster: key.Name})).To(Succeed())
		Expect(nodes.Items).To(HaveLen(2))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: headlessServiceName(key.Name), Namespace: key.Namespace}, &corev1.Service{}))).To(BeTrue())

		By("resuming with a value that is not exactly \"true\"")
		cluster.Annotations[valkeyiov1alpha1.AnnotationPauseReconciliation] = "True"
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
		Expect(meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReconciliationPaused)).To(BeNil())
		Expect(k8sClient.Get(ctx, nodeKey, node)).To(Succeed())
		Expect(node.Annotations).NotTo(HaveKey(valkeyiov1alpha1.AnnotationPauseReconciliation))
		Expect(recorder.Events).To(Receive(ContainSubstring("ReconciliationResumed")))
	})

	It("skips a paused ValkeyNode without creating its workload", func() {
		key := types.NamespacedName{Name: "paused-node", Namespace: "default"}
		Expect(k8sClient.Create(ctx, &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Annotations: pauseOn},
			Spec:       valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet},
		})).To(Succeed())
		DeferCleanup(func() { deleteNode(key) })

		r := &ValkeyNodeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: events.NewFakeRecorder(100)}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(pausedRequeue))

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: "valkey-" + key.Name, Namespace: key.Namespace}, &appsv1.StatefulSet{}))).To(BeTrue())
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
		Expect(node.Finalizers).To(BeEmpty())
		Expect(node.Status).To(Equal(valkeyiov1alpha1.ValkeyNodeStatus{}))
	})
})
