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

// sentinelFor builds a minimal ValkeySentinel for exercising the CEL validation.
func sentinelFor(name string) *valkeyiov1alpha1.ValkeySentinel {
	return &valkeyiov1alpha1.ValkeySentinel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
}

var _ = Describe("ValkeySentinel CEL validation", func() {
	ctx := context.Background()

	It("defaults replicas to 3", func() {
		s := sentinelFor("vks-default")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		Expect(s.Spec.Replicas).To(Equal(int32(3)))
		Expect(s.Status.State).To(Equal(valkeyiov1alpha1.ValkeySentinelStateInitializing))
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
	})

	It("rejects fewer than 3 replicas", func() {
		s := sentinelFor("vks-two")
		s.Spec.Replicas = 2
		err := k8sClient.Create(ctx, s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.replicas"))
	})

	It("admits an even replica count", func() {
		s := sentinelFor("vks-four")
		s.Spec.Replicas = 4
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
	})

	It("admits a 36 character name and rejects 37", func() {
		ok := sentinelFor(strings.Repeat("a", 36))
		Expect(k8sClient.Create(ctx, ok)).To(Succeed())
		Expect(k8sClient.Delete(ctx, ok)).To(Succeed())

		err := k8sClient.Create(ctx, sentinelFor(strings.Repeat("a", 37)))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("at most 36 characters"))
	})

	It("rejects a dotted name", func() {
		err := k8sClient.Create(ctx, sentinelFor("vks.prod"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must not contain '.'"))
	})

	DescribeTable("spec.config",
		func(key string, wantErr bool) {
			s := sentinelFor("vks-config")
			s.Spec.Config = map[string]string{key: "x"}
			err := k8sClient.Create(ctx, s)
			if wantErr {
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("the operator registers monitors"))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, s)).To(Succeed())
		},
		Entry("admits a process-global directive", "sentinel deny-scripts-reconfig", false),
		Entry("rejects sentinel monitor", "sentinel monitor", true),
		Entry("rejects sentinel monitor in any case", "Sentinel Monitor", true),
	)
})
