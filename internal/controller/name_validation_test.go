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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

var _ = Describe("ValkeyCluster name length CEL validation", func() {
	It("rejects a name over 37 characters and accepts one of exactly 37", func() {
		ctx := context.Background()
		c := schedulingCluster(strings.Repeat("a", 38), "", "")
		err := k8sClient.Create(ctx, c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("at most 37 characters"))

		c = &valkeyiov1alpha1.ValkeyCluster{}
		c.Name, c.Namespace = strings.Repeat("a", 37), "default"
		c.Spec.Shards, c.Spec.Replicas = 3, 1
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
	})
})
