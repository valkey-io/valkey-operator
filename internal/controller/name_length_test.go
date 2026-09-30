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

// With workloadType StatefulSet the name valkey-<cluster>-<shard>-<node> must
// stay within 52 characters: Kubernetes puts it, plus an 11-character suffix,
// into the controller-revision-hash pod label, and label values stop at 63.
// With workloadType Deployment the limit is the headless Service
// valkey-<cluster>, a DNS label of at most 63 characters.
var _ = Describe("Resource name length validation", func() {
	const (
		statefulSetLimit = "the StatefulSet name valkey-<name>-<shard>-<node> must stay within 52 characters"
		serviceLimit     = "the headless Service name valkey-<name> must stay within 63 characters"
	)

	nameCluster := func(name string, shards, replicas int32, workload valkeyiov1alpha1.WorkloadType) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: shards, Replicas: replicas, WorkloadType: workload},
		}
	}

	It("accepts a ValkeyCluster whose longest StatefulSet name is 52 characters", func() {
		cluster := nameCluster(strings.Repeat("a", 41), 1, 0, "") // valkey-<41>-0-0 is 52
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})

	It("rejects a ValkeyCluster whose StatefulSet name would be 53 characters", func() {
		err := k8sClient.Create(ctx, nameCluster(strings.Repeat("a", 42), 1, 0, ""))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(statefulSetLimit))
	})

	It("counts the digits of the highest shard and node indexes", func() {
		By("a two-digit shard index")
		err := k8sClient.Create(ctx, nameCluster(strings.Repeat("b", 41), 11, 0, "")) // valkey-<41>-10-0 is 53
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(statefulSetLimit))

		By("a two-digit node index")
		err = k8sClient.Create(ctx, nameCluster(strings.Repeat("b", 41), 1, 10, "")) // valkey-<41>-0-10 is 53
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(statefulSetLimit))
	})

	It("rejects a scale-out that pushes the StatefulSet name past the limit", func() {
		cluster := nameCluster(strings.Repeat("c", 41), 1, 0, "")
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cluster) }()

		cluster.Spec.Shards = 11 // valkey-<41>-10-0 is 53
		err := k8sClient.Update(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(statefulSetLimit))
	})

	It("limits a Deployment cluster by its headless Service name instead", func() {
		By("a name the StatefulSet rule would reject")
		cluster := nameCluster(strings.Repeat("d", 42), 1, 0, valkeyiov1alpha1.WorkloadTypeDeployment)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())

		By("a 63-character headless Service name")
		cluster = nameCluster(strings.Repeat("d", 56), 1, 0, valkeyiov1alpha1.WorkloadTypeDeployment) // valkey-<56>
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())

		By("a 64-character headless Service name")
		err := k8sClient.Create(ctx, nameCluster(strings.Repeat("d", 57), 1, 0, valkeyiov1alpha1.WorkloadTypeDeployment))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(serviceLimit))
	})
})
