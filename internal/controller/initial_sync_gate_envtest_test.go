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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

var _ = Describe("initial sync gate patch", func() {
	It("sets the gate without dropping the kubelet's conditions", func() {
		ctx := context.Background()
		pod := gatePod("initial-sync-gate", "", nil)
		pod.Namespace = "default"
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pod) }()
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		r := &ValkeyClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(r.setInitialSyncGate(ctx, pod, false, "FullSyncInProgress", "receiving")).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(r.setInitialSyncGate(ctx, pod, true, "ReplicaSynced", "synced")).To(Succeed())

		got := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), got)).To(Succeed())
		statuses := map[corev1.PodConditionType]corev1.ConditionStatus{}
		for _, c := range got.Status.Conditions {
			statuses[c.Type] = c.Status
		}
		Expect(statuses).To(Equal(map[corev1.PodConditionType]corev1.ConditionStatus{
			corev1.ContainersReady: corev1.ConditionTrue,
			initialSyncGate:        corev1.ConditionTrue,
		}))
	})

	It("waits for kubelet PodReady before marking a formed cluster Ready", func() {
		ctx := context.Background()
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-pod-readiness", Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, cluster)
		var shards []*valkey.ShardState
		r := &ValkeyClusterReconciler{
			Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: events.NewFakeRecorder(100), ValkeyClients: testValkeyClients(k8sClient),
			clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState {
				return &valkey.ClusterState{Shards: shards}
			},
		}
		key := client.ObjectKeyFromObject(cluster)
		reconcileOnce := func() reconcile.Result {
			GinkgoHelper()
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
			return result
		}
		reconcileOnce()
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: valkeyNodeName(key.Name, 0, 0)}, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, node)
		pod := gatePod("cluster-pod-readiness-0", "10.0.0.1", nil)
		pod.Namespace = key.Namespace
		podStatus := pod.Status
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, pod)
		pod.Status = podStatus
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		node.Status.Ready = true
		node.Status.ObservedGeneration = node.Generation
		node.Status.PodName, node.Status.PodIP = pod.Name, pod.Status.PodIP
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
		primary := gatePrimary(pod.Status.PodIP, "0-16383", nil)
		shards = []*valkey.ShardState{{
			Id: primary.Id, PrimaryId: primary.Id, Nodes: []*valkey.NodeState{primary},
			Slots: []valkey.SlotsRange{{Start: 0, End: valkey.TotalSlots - 1}},
		}}

		Expect(reconcileOnce().RequeueAfter).To(Equal(initialSyncGateRecheck))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(podCondition(pod, initialSyncGate)).To(HaveField("Status", corev1.ConditionTrue))
		Expect(meta.IsStatusConditionTrue(cluster.Status.Conditions, valkeyiov1alpha1.ConditionClusterFormed)).To(BeTrue())
		Expect(meta.IsStatusConditionFalse(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)).To(BeTrue())

		// The gate stays True across reconciles while the kubelet has not reported PodReady.
		Expect(reconcileOnce().RequeueAfter).To(Equal(initialSyncGateRecheck))
		Expect(meta.IsStatusConditionFalse(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)).To(BeTrue())

		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		reconcileOnce()
		Expect(meta.IsStatusConditionTrue(cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)).To(BeTrue())
		Expect(cluster.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
	})
})
