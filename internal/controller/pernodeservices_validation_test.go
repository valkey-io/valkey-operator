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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// Deployment, because a StatefulSet name must stay within 52 characters.
// That limit is tighter than the 63-character per-node Service limit, so on
// a StatefulSet the Service rule is not the one that rejects these names.
var _ = Describe("perNodeServices CEL validation", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("accepts a cluster name when perNodeServices is omitted", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 53), Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:       1,
				WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment,
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cluster) }()
	})

	It("accepts a name of exactly 63 characters with perNodeServices set", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 52), Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:       1,
				Replicas:     0,
				WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment,
				Networking: &valkeyiov1alpha1.NetworkingSpec{
					PerNodeServices: &valkeyiov1alpha1.PerNodeServicesSpec{},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cluster) }()
	})

	It("rejects a name of 64 characters with perNodeServices set", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 53), Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:       1,
				Replicas:     0,
				WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment,
				Networking: &valkeyiov1alpha1.NetworkingSpec{
					PerNodeServices: &valkeyiov1alpha1.PerNodeServicesSpec{},
				},
			},
		}
		err := k8sClient.Create(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("per-node Service names would exceed 63 characters"))
	})

	It("accepts higher shard and replica counts when the name fits", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("b", 48), Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:       10,
				Replicas:     2,
				WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment,
				Networking: &valkeyiov1alpha1.NetworkingSpec{
					PerNodeServices: &valkeyiov1alpha1.PerNodeServicesSpec{},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cluster) }()
	})

	It("rejects when shard index digits push the name over 63", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("c", 52), Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:       11,
				Replicas:     0,
				WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment,
				Networking: &valkeyiov1alpha1.NetworkingSpec{
					PerNodeServices: &valkeyiov1alpha1.PerNodeServicesSpec{},
				},
			},
		}
		err := k8sClient.Create(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("per-node Service names would exceed 63 characters"))
	})
	It("rejects turning perNodeServices on when the name is too long", func() {
		cluster := &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("e", 53), Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, WorkloadType: valkeyiov1alpha1.WorkloadTypeDeployment},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cluster) }()

		cluster.Spec.Networking = &valkeyiov1alpha1.NetworkingSpec{
			PerNodeServices: &valkeyiov1alpha1.PerNodeServicesSpec{},
		}
		err := k8sClient.Update(ctx, cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("per-node Service names would exceed 63 characters"))
	})
})
