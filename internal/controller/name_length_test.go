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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// The StatefulSet name valkey-<cluster>-<shard>-<node> must stay within 52
// characters: Kubernetes puts it, plus an 11-character suffix, into the
// controller-revision-hash pod label, and label values stop at 63.
var _ = Describe("Resource name length validation", func() {
	const tooLong = "metadata.name is too long"

	nameCluster := func(name string, shards, replicas int32) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: shards, Replicas: replicas},
		}
	}

	It("accepts a ValkeyCluster whose longest StatefulSet name is 52 characters", func() {
		cluster := nameCluster(strings.Repeat("a", 41), 1, 0) // valkey-<41>-0-0 is 52
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})

	It("rejects a ValkeyCluster whose StatefulSet name would be 53 characters", func() {
		err := k8sClient.Create(ctx, nameCluster(strings.Repeat("a", 42), 1, 0))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(tooLong))
	})

	It("counts the digits of the highest shard and node indexes", func() {
		By("a two-digit shard index")
		err := k8sClient.Create(ctx, nameCluster(strings.Repeat("b", 41), 11, 0)) // valkey-<41>-10-0 is 53
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(tooLong))

		By("a two-digit node index")
		err = k8sClient.Create(ctx, nameCluster(strings.Repeat("b", 41), 1, 10)) // valkey-<41>-0-10 is 53
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(tooLong))
	})

	It("rejects a scale-out that pushes the StatefulSet name past the limit", func() {
		cluster := nameCluster(strings.Repeat("c", 41), 1, 0)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cluster) }()

		cluster.Spec.Shards = 11 // valkey-<41>-10-0 is 53
		err := k8sClient.Update(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(tooLong))
	})

	It("applies the same StatefulSet limit to a standalone ValkeyNode", func() {
		node := &valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("d", 45), Namespace: "default"}} // valkey-<45>
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		Expect(k8sClient.Delete(ctx, node)).To(Succeed())

		err := k8sClient.Create(ctx, &valkeyiov1alpha1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("d", 46), Namespace: "default"}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(tooLong))
	})
})
