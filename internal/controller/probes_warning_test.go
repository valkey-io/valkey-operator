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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("spec.probes startup warning", func() {
	ctx := context.Background()

	// createCluster creates a one-shard cluster with a replica whose startup
	// probe allows 50s, and removes it and its ValkeyNodes when the spec ends.
	createCluster := func(name string, persistence *valkeyiov1alpha1.PersistenceSpec) {
		GinkgoHelper()
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:           1,
				ReplicasPerShard: 1,
				Persistence:      persistence,
				Probes: &valkeyiov1alpha1.ProbesSpec{
					Startup: &valkeyiov1alpha1.StartupProbeSpec{FailureThreshold: new(int32(10))},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
			_ = k8sClient.List(ctx, nodeList, client.InNamespace("default"), client.MatchingLabels{LabelCluster: name})
			for i := range nodeList.Items {
				_ = k8sClient.Delete(ctx, &nodeList.Items[i])
			}
			_ = k8sClient.Delete(ctx, cluster)
		})
	}
	newReconciler := func(recorder *events.FakeRecorder) *ValkeyClusterReconciler {
		return &ValkeyClusterReconciler{
			Client:        k8sClient,
			APIReader:     k8sClient,
			Scheme:        k8sClient.Scheme(),
			Recorder:      recorder,
			ValkeyClients: testValkeyClients(k8sClient),
		}
	}
	reconcileOnce := func(r *ValkeyClusterReconciler, name string) *valkeyiov1alpha1.ValkeyCluster {
		GinkgoHelper()
		key := types.NamespacedName{Name: name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		cluster := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
		return cluster
	}

	It("warns once about a startup probe shorter than the hand-off wait, applies it, and clears when it is raised", func() {
		const name = "probes-startup-warning"
		createCluster(name, nil)
		recorder := events.NewFakeRecorder(100)
		r := newReconciler(recorder)

		reconcileOnce(r, name)
		cluster := reconcileOnce(r, name)
		cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(valkeyiov1alpha1.ReasonStartupProbeTooShort))
		Expect(cond.Message).To(ContainSubstring("gives up after 50s"))
		Expect(cond.Message).To(ContainSubstring("set failureThreshold to at least 80"))
		var warnings []string
		for _, e := range collectEvents(recorder) {
			if e == "Warning "+valkeyiov1alpha1.ReasonStartupProbeTooShort+" "+cond.Message {
				warnings = append(warnings, e)
			}
		}
		Expect(warnings).To(HaveLen(1), "the event fires once, not on every reconcile")

		By("rendering the value as set")
		node := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: valkeyNodeName(name, 0, 0), Namespace: "default"}, node)).To(Succeed())
		template, err := buildNodePodTemplate(node)
		Expect(err).NotTo(HaveOccurred())
		Expect(template.Spec.Containers[0].StartupProbe.FailureThreshold).To(Equal(int32(10)))

		By("raising failureThreshold to cover the hand-off")
		Expect(k8sClient.Patch(ctx, cluster, client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"probes":{"startup":{"failureThreshold":80}}}}`)))).To(Succeed())
		cluster = reconcileOnce(r, name)
		Expect(meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)).To(BeNil())
	})

	It("does not warn about a short startup probe on a cluster with persistence", func() {
		const name = "probes-startup-persistent"
		createCluster(name, &valkeyiov1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi")})
		cluster := reconcileOnce(newReconciler(events.NewFakeRecorder(100)), name)
		Expect(meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionConfigurationWarning)).To(BeNil())
	})
})
