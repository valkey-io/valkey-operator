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

var _ = Describe("ValkeyCluster name validation", func() {
	ctx := context.Background()

	nameCluster := func(name string) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1},
		}
	}

	It("rejects a name with a dot", func() {
		// Kubernetes admits the dot, but the child Service valkey-cache.prod would be an invalid DNS label.
		err := k8sClient.Create(ctx, nameCluster("cache.prod"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must not contain '.'"))
	})

	It("admits a name with a hyphen", func() {
		cluster := nameCluster("cache-prod")
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
	})
})
