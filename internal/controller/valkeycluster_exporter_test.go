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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// spec.exporter carries an object-level default of {enabled: true}, which only
// applies when the whole object is absent. Setting any sibling field used to
// drop enabled to false and silently remove the sidecar (#394). The cluster
// controller resolves a nil enabled to true and propagates an explicit value
// to its nodes; a ValkeyNode alone runs the sidecar only on an explicit true.
var _ = Describe("exporter enabled defaulting", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// storeCluster creates a cluster and returns it as the API server
	// defaulted it.
	storeCluster := func(name string, exporter valkeyiov1alpha1.ExporterSpec) *valkeyiov1alpha1.ValkeyCluster {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Replicas: 0,
				Exporter: exporter,
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, cluster)
		})

		stored := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, stored)).To(Succeed())
		return stored
	}

	// expectExporterOn asserts the resolved default reaches the pod: the node
	// spec built from the cluster carries an explicit true and the rendered
	// pod has the sidecar.
	expectExporterOn := func(cluster *valkeyiov1alpha1.ValkeyCluster) {
		GinkgoHelper()
		Expect(cluster.Spec.ExporterEnabled()).To(BeTrue())

		node := buildClusterValkeyNode(cluster, 0, 0)
		Expect(node.Spec.Exporter.Enabled).NotTo(BeNil(),
			"the cluster controller must propagate an explicit value")
		Expect(*node.Spec.Exporter.Enabled).To(BeTrue())

		pts, err := buildValkeyNodePodTemplateSpec(node, valkeyNodeLabels(node))
		Expect(err).NotTo(HaveOccurred())
		names := make([]string, 0, len(pts.Spec.Containers))
		for _, c := range pts.Spec.Containers {
			names = append(names, c.Name)
		}
		Expect(names).To(ContainElement("metrics-exporter"))
	}

	// A typed Go client cannot omit the block: a struct never satisfies
	// omitempty, so it always sends exporter: {}. Only an unstructured create
	// exercises the object-level default the way applied YAML does.
	It("enables the exporter when spec.exporter is omitted", func() {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("valkey.io/v1alpha1")
		u.SetKind("ValkeyCluster")
		u.SetName("exp-omitted")
		u.SetNamespace("default")
		Expect(unstructured.SetNestedMap(u.Object, map[string]any{
			"shards":   int64(1),
			"replicas": int64(0),
		}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, u)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, u)
		})

		stored := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "exp-omitted", Namespace: "default"}, stored)).To(Succeed())
		// The object-level default materialises when the block is absent.
		Expect(stored.Spec.Exporter.Enabled).NotTo(BeNil())
		Expect(*stored.Spec.Exporter.Enabled).To(BeTrue())
		expectExporterOn(stored)
	})

	It("enables the exporter when spec.exporter is empty", func() {
		stored := storeCluster("exp-empty", valkeyiov1alpha1.ExporterSpec{})
		expectExporterOn(stored)
	})

	It("keeps the exporter enabled when only image is set", func() {
		stored := storeCluster("exp-image", valkeyiov1alpha1.ExporterSpec{
			Image: "oliver006/redis_exporter:v1.80.0",
		})
		expectExporterOn(stored)
	})

	It("keeps the exporter enabled when only resources are set", func() {
		stored := storeCluster("exp-resources", valkeyiov1alpha1.ExporterSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
			},
		})
		expectExporterOn(stored)
	})

	It("keeps the exporter enabled when only args are set", func() {
		stored := storeCluster("exp-args", valkeyiov1alpha1.ExporterSpec{
			Args: []string{"--log-format=json"},
		})
		expectExporterOn(stored)
	})

	It("keeps the exporter enabled when only securityContext is set", func() {
		stored := storeCluster("exp-secctx", valkeyiov1alpha1.ExporterSpec{
			SecurityContext: &corev1.SecurityContext{RunAsNonRoot: boolPtr(true)},
		})
		expectExporterOn(stored)
	})

	It("honours an explicit enabled: false", func() {
		stored := storeCluster("exp-disabled", valkeyiov1alpha1.ExporterSpec{Enabled: boolPtr(false)})
		Expect(stored.Spec.ExporterEnabled()).To(BeFalse())

		node := buildClusterValkeyNode(stored, 0, 0)
		Expect(node.Spec.Exporter.Enabled).To(BeNil(),
			"disabled must resolve to nil on the node spec — an explicit false rolls upgraded clusters")

		pts, err := buildValkeyNodePodTemplateSpec(node, valkeyNodeLabels(node))
		Expect(err).NotTo(HaveOccurred())
		Expect(pts.Spec.Containers).To(HaveLen(1))
	})

	// Old operators serialised Enabled as a plain bool with omitempty, so a
	// disabled exporter was stored with the field absent. The desired spec
	// must reproduce that shape: nodeRequiresRoll compares specs, and a
	// nil-vs-false mismatch triggers a proactive failover on operator upgrade
	// even though the rendered pod template is identical (#401).
	It("predicts no roll for nodes stored by an older operator", func() {
		stored := storeCluster("exp-upgrade", valkeyiov1alpha1.ExporterSpec{Enabled: boolPtr(false)})

		old := buildClusterValkeyNode(stored, 0, 0)
		old.Spec.Exporter.Enabled = nil // the shape an old operator stored
		old.Status.PodIP = "10.0.0.1"

		desired := buildClusterValkeyNode(stored, 0, 0)
		Expect(nodeRequiresRoll(old, desired)).To(BeFalse(),
			"a spec-shape-only change must not fail over upgraded clusters")
	})

	// Enabled is a *bool so that an explicit false is serialised rather than
	// dropped by omitempty and defaulted straight back to true.
	It("keeps enabled: false across an update", func() {
		stored := storeCluster("exp-disabled-update", valkeyiov1alpha1.ExporterSpec{Enabled: boolPtr(false)})

		stored.Spec.Exporter.Image = "oliver006/redis_exporter:v1.80.0"
		Expect(k8sClient.Update(ctx, stored)).To(Succeed())

		reread := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "exp-disabled-update", Namespace: "default"}, reread)).To(Succeed())
		Expect(reread.Spec.Exporter.Enabled).NotTo(BeNil())
		Expect(*reread.Spec.Exporter.Enabled).To(BeFalse(),
			"an explicit false must survive a round trip through the API server")
	})

	// Guards against a field-level default: it would stamp enabled: true onto
	// bare ValkeyNodes too, giving standalone nodes a sidecar whose _exporter
	// credentials nothing provisions.
	It("does not stamp enabled onto a standalone ValkeyNode", func() {
		node := &valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Name: "exp-standalone", Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyNodeSpec{},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, node)
		})

		stored := &valkeyiov1alpha1.ValkeyNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "exp-standalone", Namespace: "default"}, stored)).To(Succeed())
		Expect(stored.Spec.Exporter.Enabled).To(BeNil())
	})
})
