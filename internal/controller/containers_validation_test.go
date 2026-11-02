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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

func containersCluster(name string, containers []corev1.Container) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, Containers: containers},
	}
}

func containersNode(name string, containers []corev1.Container) *valkeyiov1alpha1.ValkeyNode {
	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       valkeyiov1alpha1.ValkeyNodeSpec{Containers: containers},
	}
}

var _ = Describe("containers CEL validation", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("accepts a server patch and a sidecar on a ValkeyCluster and a ValkeyNode", func() {
		containers := []corev1.Container{
			{Name: "server"},
			{Name: "my-sidecar", Image: "busybox:latest"},
		}
		c := containersCluster("containers-ok-c", containers)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		n := containersNode("containers-ok-n", containers)
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		Expect(k8sClient.Delete(ctx, n)).To(Succeed())
	})

	It("rejects a container named server-init on a ValkeyCluster and a ValkeyNode", func() {
		containers := []corev1.Container{{Name: "server-init", Image: "busybox:latest"}}
		err := k8sClient.Create(ctx, containersCluster("containers-bad-c", containers))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("container name 'server-init' is reserved"))
		err = k8sClient.Create(ctx, containersNode("containers-bad-n", containers))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("container name 'server-init' is reserved"))
	})
})
