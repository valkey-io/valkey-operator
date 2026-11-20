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
	"sigs.k8s.io/controller-runtime/pkg/client"
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
})
