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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("failover.failback validation", func() {
	ctx := context.Background()
	cluster := func(name string, failback *valkeyiov1alpha1.FailbackSpec) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{
				Shards:   1,
				Failover: &valkeyiov1alpha1.ClusterFailoverSpec{Failback: failback},
			},
		}
	}

	It("defaults an empty failback to Enabled with a 300s quiet period", func() {
		c := cluster("failback-defaults", &valkeyiov1alpha1.FailbackSpec{})
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, c) }()
		Expect(c.Spec.Failover.Failback.Mode).To(Equal(valkeyiov1alpha1.FailbackEnabled))
		Expect(c.Spec.Failover.Failback.QuietPeriodSeconds).To(Equal(new(int32(300))))
	})

	It("accepts Disabled and a zero quiet period", func() {
		c := cluster("failback-disabled", &valkeyiov1alpha1.FailbackSpec{Mode: valkeyiov1alpha1.FailbackDisabled, QuietPeriodSeconds: new(int32(0))})
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, c) }()
	})

	It("rejects a negative quiet period", func() {
		err := k8sClient.Create(ctx, cluster("failback-negative", &valkeyiov1alpha1.FailbackSpec{QuietPeriodSeconds: new(int32(-1))}))
		Expect(err).To(MatchError(ContainSubstring("quietPeriodSeconds")))
	})

	It("rejects an unknown mode", func() {
		err := k8sClient.Create(ctx, cluster("failback-mode", &valkeyiov1alpha1.FailbackSpec{Mode: "Sometimes"}))
		Expect(err).To(MatchError(ContainSubstring("spec.failover.failback.mode")))
	})
})
