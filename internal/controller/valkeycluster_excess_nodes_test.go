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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// Regression test for https://github.com/valkey-io/valkey-operator/issues/403:
// a ValkeyNode carrying the cluster label but with unparseable topology
// labels was silently skipped by deleteExcessValkeyNodes while the scale-in
// path still counted it as extra, wedging the cluster in Reconciling
// forever with nothing naming the offending node.
var _ = Describe("deleteExcessValkeyNodes", func() {
	const clusterName = "excess-nodes-test"

	var (
		testCtx      context.Context
		r            *ValkeyClusterReconciler
		fakeRecorder *events.FakeRecorder
		cluster      *valkeyiov1alpha1.ValkeyCluster
	)

	makeNode := func(name string, labels map[string]string) {
		GinkgoHelper()
		node := &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    labels,
			},
		}
		Expect(k8sClient.Create(testCtx, node)).To(Succeed())
	}

	BeforeEach(func() {
		testCtx = context.Background()
		fakeRecorder = events.NewFakeRecorder(100)
		r = &ValkeyClusterReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeRecorder,
		}
		cluster = &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName,
				Namespace: "default",
			},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 0,
			},
		}
		Expect(k8sClient.Create(testCtx, cluster)).To(Succeed())
	})

	AfterEach(func() {
		nodeList := &valkeyiov1alpha1.ValkeyNodeList{}
		Expect(k8sClient.List(testCtx, nodeList,
			client.InNamespace("default"),
			client.MatchingLabels{LabelCluster: clusterName})).To(Succeed())
		for i := range nodeList.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, &nodeList.Items[i]))).To(Succeed())
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, cluster))).To(Succeed())
	})

	It("deletes a ValkeyNode with unparseable topology labels and names it in an event", func() {
		By("creating a node with the cluster label but a garbage shard-index")
		badNode := clusterName + "-bad-0"
		makeNode(badNode, map[string]string{
			LabelCluster:    clusterName,
			LabelShardIndex: "not-a-number",
			LabelNodeIndex:  "0",
		})

		By("running the excess-node prune")
		deleted, err := r.deleteExcessValkeyNodes(testCtx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(deleted).To(BeTrue(), "malformed node must be reaped, not skipped")

		By("verifying the node is gone")
		got := &valkeyiov1alpha1.ValkeyNode{}
		err = k8sClient.Get(testCtx, types.NamespacedName{Name: badNode, Namespace: "default"}, got)
		Expect(errors.IsNotFound(err)).To(BeTrue())

		By("verifying an event names the offending node")
		evts := collectEvents(fakeRecorder)
		Expect(evts).To(ContainElement(ContainSubstring(badNode)))
		By("verifying the full event contract: Warning type, reason, raw label values")
		Expect(evts).To(ContainElement(ContainSubstring("Warning")))
		Expect(evts).To(ContainElement(ContainSubstring("ValkeyNodeDeleted")))
		Expect(evts).To(ContainElement(ContainSubstring("not-a-number")))
	})

	It("deletes a ValkeyNode with an unparseable node-index and valid shard-index", func() {
		By("creating a node with garbage node-index but a good shard-index")
		badIdxNode := clusterName + "-badidx-0"
		makeNode(badIdxNode, map[string]string{
			LabelCluster:    clusterName,
			LabelShardIndex: "0",
			LabelNodeIndex:  "bogus",
		})

		By("running the excess-node prune")
		deleted, err := r.deleteExcessValkeyNodes(testCtx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(deleted).To(BeTrue(), "unparseable node-index must be reaped, not skipped")

		By("verifying the node is gone and the event carries the raw values")
		got := &valkeyiov1alpha1.ValkeyNode{}
		err = k8sClient.Get(testCtx, types.NamespacedName{Name: badIdxNode, Namespace: "default"}, got)
		Expect(errors.IsNotFound(err)).To(BeTrue())
		evts := collectEvents(fakeRecorder)
		Expect(evts).To(ContainElement(ContainSubstring(badIdxNode)))
		Expect(evts).To(ContainElement(ContainSubstring("Warning")))
		Expect(evts).To(ContainElement(ContainSubstring("ValkeyNodeDeleted")))
		Expect(evts).To(ContainElement(ContainSubstring("bogus")))
	})

	It("deletes a ValkeyNode with negative topology indexes", func() {
		By("building a node with a negative shard-index via the fake client, which skips label validation")
		negNode := &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName + "-neg-0",
				Namespace: "default",
				Labels: map[string]string{
					LabelCluster:    clusterName,
					LabelShardIndex: "-1",
					LabelNodeIndex:  "0",
				},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(cluster, negNode).Build()
		negReconciler := &ValkeyClusterReconciler{
			Client:    fakeClient,
			APIReader: fakeClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  events.NewFakeRecorder(100),
		}

		By("running the excess-node prune")
		deleted, err := negReconciler.deleteExcessValkeyNodes(testCtx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(deleted).To(BeTrue(), "negative index must be reaped, not skipped")

		By("verifying the node is gone")
		got := &valkeyiov1alpha1.ValkeyNode{}
		err = fakeClient.Get(testCtx, types.NamespacedName{Name: clusterName + "-neg-0", Namespace: "default"}, got)
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})

	It("keeps a valid in-range ValkeyNode", func() {
		By("creating a healthy shard-0 node-0")
		goodNode := clusterName + "-0-0"
		makeNode(goodNode, map[string]string{
			LabelCluster:    clusterName,
			LabelShardIndex: "0",
			LabelNodeIndex:  "0",
		})

		By("running the excess-node prune")
		deleted, err := r.deleteExcessValkeyNodes(testCtx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(deleted).To(BeFalse(), "in-range node must survive the prune")

		By("verifying the node is still present")
		got := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: goodNode, Namespace: "default"}, got)).To(Succeed())
	})

	It("still deletes an out-of-range ValkeyNode", func() {
		By("creating a node past the shard count")
		farNode := clusterName + "-9-0"
		makeNode(farNode, map[string]string{
			LabelCluster:    clusterName,
			LabelShardIndex: "9",
			LabelNodeIndex:  "0",
		})

		By("running the excess-node prune")
		deleted, err := r.deleteExcessValkeyNodes(testCtx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(deleted).To(BeTrue())

		By("verifying the node is gone")
		got := &valkeyiov1alpha1.ValkeyNode{}
		err = k8sClient.Get(testCtx, types.NamespacedName{Name: farNode, Namespace: "default"}, got)
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})
})
