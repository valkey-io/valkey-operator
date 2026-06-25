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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

var _ = Describe("reconcileShardServices", func() {
	ctx := context.Background()

	newReconciler := func() *ValkeyClusterReconciler {
		return &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}
	}

	listShardServices := func(clusterName string) []corev1.Service {
		services := &corev1.ServiceList{}
		Expect(k8sClient.List(ctx, services, client.InNamespace("default"), client.MatchingLabels{LabelCluster: clusterName})).To(Succeed())
		shardServices := make([]corev1.Service, 0, len(services.Items))
		for _, svc := range services.Items {
			if _, ok := svc.Labels[LabelShardIndex]; ok {
				shardServices = append(shardServices, svc)
			}
		}
		return shardServices
	}

	It("creates one NodePort Service per shard pinned to each node", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "shardsvc-nodeport", Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   2,
				Replicas: 1,
				ExternalAccess: &valkeyiov1alpha1.ExternalAccessSpec{
					Enabled:     true,
					ServiceType: corev1.ServiceTypeNodePort,
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() {
			for _, svc := range listShardServices(cluster.Name) {
				_ = k8sClient.Delete(ctx, &svc)
			}
			_ = k8sClient.Delete(ctx, cluster)
		}()

		endpoints, err := newReconciler().reconcileShardServices(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())

		services := listShardServices(cluster.Name)
		Expect(services).To(HaveLen(2))
		for _, svc := range services {
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
			Expect(svc.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "valkey-operator"))
			Expect(svc.Spec.Selector).To(HaveKeyWithValue(LabelCluster, cluster.Name))
			Expect(svc.Spec.Selector).To(HaveKey(LabelShardIndex))
			// One port per node (1 primary + 1 replica), each targeting a node-unique name.
			Expect(svc.Spec.Ports).To(HaveLen(2))
			Expect(svc.Spec.Ports[0].TargetPort.StrVal).To(Equal("vk-n0"))
			Expect(svc.Spec.Ports[1].TargetPort.StrVal).To(Equal("vk-n1"))
		}

		// Status endpoints are returned for every shard, one port slot per node.
		Expect(endpoints).To(HaveLen(2))
		for _, ep := range endpoints {
			Expect(ep.NodePorts).To(HaveLen(2))
		}
	})

	It("removes shard Services on scale-in and when disabled", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "shardsvc-scalein", Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:         3,
				Replicas:       0,
				ExternalAccess: &valkeyiov1alpha1.ExternalAccessSpec{Enabled: true},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() {
			for _, svc := range listShardServices(cluster.Name) {
				_ = k8sClient.Delete(ctx, &svc)
			}
			_ = k8sClient.Delete(ctx, cluster)
		}()
		r := newReconciler()

		_, err := r.reconcileShardServices(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(listShardServices(cluster.Name)).To(HaveLen(3))

		By("scaling in to one shard")
		cluster.Spec.Shards = 1
		_, err = r.reconcileShardServices(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(listShardServices(cluster.Name)).To(HaveLen(1))

		By("disabling external access")
		cluster.Spec.ExternalAccess.Enabled = false
		_, err = r.reconcileShardServices(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(listShardServices(cluster.Name)).To(BeEmpty())
	})

	It("is a no-op on a second reconcile and preserves allocated NodePorts", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "shardsvc-stable", Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:         1,
				Replicas:       1,
				ExternalAccess: &valkeyiov1alpha1.ExternalAccessSpec{Enabled: true},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() {
			for _, svc := range listShardServices(cluster.Name) {
				_ = k8sClient.Delete(ctx, &svc)
			}
			_ = k8sClient.Delete(ctx, cluster)
		}()
		r := newReconciler()

		first, err := r.reconcileShardServices(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())
		second, err := r.reconcileShardServices(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())

		// The allocated NodePorts must be stable across reconciles.
		Expect(second).To(Equal(first))
	})
})
