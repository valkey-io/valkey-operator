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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// A Deployment creates the replacement of a deleted pod at once, so for the
// length of the old pod's termination two pods carry the node's labels. The
// status follows the replacement and keeps the old pod's IP in
// terminatingPodIPs while it exists, along with any replacement deleted in
// its turn.
var _ = Describe("ValkeyNode updateStatus with a terminating pod", func() {
	const holdFinalizer = "valkey.io/test-hold"
	var (
		ctx  context.Context
		node *valkeyiov1alpha1.ValkeyNode
		r    *ValkeyNodeReconciler
	)

	// createPod creates a Ready pod of the node with the given IP. held keeps
	// it around after a delete, as a terminating pod.
	createPod := func(name, ip string, held bool) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: node.Namespace, Labels: valkeyNodeLabels(node)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "valkey/valkey:9.0.0"}}},
		}
		if held {
			pod.Finalizers = []string{holdFinalizer}
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		pod.Status.PodIP = ip
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		return pod
	}

	release := func(pod *corev1.Pod) {
		current := &corev1.Pod{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
			return
		}
		current.Finalizers = nil
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
	}

	status := func() valkeyiov1alpha1.ValkeyNodeStatus {
		_, err := r.updateStatus(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		updated := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), updated)).To(Succeed())
		return updated.Status
	}

	BeforeEach(func() {
		ctx = context.Background()
		node = &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "terminating-pod-test",
				Namespace: "default",
				Labels:    map[string]string{LabelCluster: "terminating-pod-cluster", LabelShardIndex: "0", LabelNodeIndex: "0"},
			},
			Spec: valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		r = &ValkeyNodeReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: events.NewFakeRecorder(100),
			resolveRoleFunc: func(_ context.Context, _ *valkeyiov1alpha1.ValkeyNode) string {
				return RolePrimary
			},
		}
	})

	AfterEach(func() {
		pods := &corev1.PodList{}
		Expect(k8sClient.List(ctx, pods, client.InNamespace(node.Namespace), client.MatchingLabels(valkeyNodeLabels(node)))).To(Succeed())
		for i := range pods.Items {
			release(&pods.Items[i])
			_ = k8sClient.Delete(ctx, &pods.Items[i])
		}
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: valkeyNodeResourceName(node), Namespace: node.Namespace}})
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
	})

	It("follows the replacement and keeps the terminating pods' IPs until they are gone", func() {
		old := createPod(node.Name+"-old", "10.0.0.1", true)
		Expect(status().PodIP).To(Equal("10.0.0.1"))

		By("adding the replacement and deleting the old pod, which a finalizer keeps terminating")
		first := createPod(node.Name+"-first", "10.0.0.2", true)
		Expect(k8sClient.Delete(ctx, old)).To(Succeed())
		got := status()
		Expect(got.PodName).To(Equal(node.Name + "-first"))
		Expect(got.PodIP).To(Equal("10.0.0.2"))
		Expect(got.TerminatingPodIPs).To(Equal([]string{"10.0.0.1"}))

		By("deleting the replacement in its turn and adding a second one")
		createPod(node.Name+"-second", "10.0.0.3", false)
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		got = status()
		Expect(got.PodIP).To(Equal("10.0.0.3"))
		Expect(got.TerminatingPodIPs).To(ConsistOf("10.0.0.2", "10.0.0.1"), "the old pod stays visible behind the newer one")

		By("letting the old pods go")
		release(old)
		release(first)
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(old), &corev1.Pod{})).NotTo(Succeed())
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(first), &corev1.Pod{})).NotTo(Succeed())
		}).Should(Succeed())
		got = status()
		Expect(got.PodIP).To(Equal("10.0.0.3"))
		Expect(got.TerminatingPodIPs).To(BeEmpty())
	})

	It("stays on a terminating pod while it is the only one", func() {
		old := createPod(node.Name+"-only", "10.0.0.1", true)
		Expect(k8sClient.Delete(ctx, old)).To(Succeed())
		got := status()
		Expect(got.PodIP).To(Equal("10.0.0.1"))
		Expect(got.TerminatingPodIPs).To(BeEmpty())
	})
})
