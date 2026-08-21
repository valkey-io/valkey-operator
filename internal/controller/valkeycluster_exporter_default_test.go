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
	"k8s.io/apimachinery/pkg/types"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// spec.exporter carries an object-level default of {enabled: true}, which only
// applies when the whole object is absent. Without a default on the field
// itself, setting any sibling field made the object present and dropped enabled
// to false, silently removing the sidecar (#394).
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

	// expectDefaultedTrue asserts the API server materialised the field rather
	// than leaving it nil. IsEnabled() alone would also pass on a nil, so it
	// cannot tell a working default from a dropped marker.
	expectDefaultedTrue := func(cluster *valkeyiov1alpha1.ValkeyCluster) {
		GinkgoHelper()
		Expect(cluster.Spec.Exporter.Enabled).NotTo(BeNil(),
			"the CRD default must materialise enabled on the stored object")
		Expect(*cluster.Spec.Exporter.Enabled).To(BeTrue())
		Expect(cluster.Spec.Exporter.IsEnabled()).To(BeTrue())
	}

	It("enables the exporter when spec.exporter is omitted", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "exp-omitted", Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, Replicas: 0},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, cluster)
		})

		stored := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "exp-omitted", Namespace: "default"}, stored)).To(Succeed())
		expectDefaultedTrue(stored)
	})

	It("keeps the exporter enabled when only image is set", func() {
		stored := storeCluster("exp-image", valkeyiov1alpha1.ExporterSpec{
			Image: "oliver006/redis_exporter:v1.80.0",
		})
		expectDefaultedTrue(stored)
	})

	It("keeps the exporter enabled when only resources are set", func() {
		stored := storeCluster("exp-resources", valkeyiov1alpha1.ExporterSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
			},
		})
		expectDefaultedTrue(stored)
	})

	It("keeps the exporter enabled when only args are set", func() {
		stored := storeCluster("exp-args", valkeyiov1alpha1.ExporterSpec{
			Args: []string{"--log-format=json"},
		})
		expectDefaultedTrue(stored)
	})

	It("keeps the exporter enabled when only securityContext is set", func() {
		stored := storeCluster("exp-secctx", valkeyiov1alpha1.ExporterSpec{
			SecurityContext: &corev1.SecurityContext{RunAsNonRoot: boolPtr(true)},
		})
		expectDefaultedTrue(stored)
	})

	It("honours an explicit enabled: false", func() {
		stored := storeCluster("exp-disabled", valkeyiov1alpha1.ExporterSpec{Enabled: boolPtr(false)})
		Expect(stored.Spec.Exporter.Enabled).NotTo(BeNil())
		Expect(*stored.Spec.Exporter.Enabled).To(BeFalse())
		Expect(stored.Spec.Exporter.IsEnabled()).To(BeFalse())
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
})
