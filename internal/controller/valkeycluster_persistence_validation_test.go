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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// persistenceCluster builds a minimal ValkeyCluster for exercising the
// persistence CEL validation on ValkeyClusterSpec. An empty workloadType
// leaves the field to its StatefulSet default.
func persistenceCluster(name string, workloadType valkeyiov1alpha1.WorkloadType, p *valkeyiov1alpha1.PersistenceSpec) *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Shards:       1,
			Replicas:     0,
			WorkloadType: workloadType,
			Persistence:  p,
		},
	}
}

// persistenceSpec returns a PersistenceSpec of the given size. An empty
// storageClassName leaves the field unset.
func persistenceSpec(size, storageClassName string) *valkeyiov1alpha1.PersistenceSpec {
	p := &valkeyiov1alpha1.PersistenceSpec{Size: resource.MustParse(size)}
	if storageClassName != "" {
		p.StorageClassName = &storageClassName
	}
	return p
}

var _ = Describe("ValkeyClusterSpec persistence CEL validation", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	// create returns the API server's verdict and deletes the cluster at the
	// end of the spec if it was admitted.
	create := func(cluster *valkeyiov1alpha1.ValkeyCluster) error {
		err := k8sClient.Create(ctx, cluster)
		if err == nil {
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, cluster)
			})
		}
		return err
	}

	// update reads the stored cluster, applies mutate and writes it back.
	update := func(name string, mutate func(*valkeyiov1alpha1.ValkeyCluster)) error {
		GinkgoHelper()
		cluster := &valkeyiov1alpha1.ValkeyCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, cluster)).To(Succeed())
		mutate(cluster)
		return k8sClient.Update(ctx, cluster)
	}

	// bumpReplicas makes an update that leaves persistence alone still carry
	// a real spec change.
	bumpReplicas := func(c *valkeyiov1alpha1.ValkeyCluster) { c.Spec.Replicas = 1 }

	// expectInvalid requires a validation rejection, not some other error
	// such as a conflict, carrying the given rule message.
	expectInvalid := func(err error, message string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
		Expect(err.Error()).To(ContainSubstring(message))
	}

	Context("persistence requires workloadType StatefulSet", func() {
		const message = "persistence requires workloadType StatefulSet"

		It("rejects persistence with workloadType Deployment", func() {
			expectInvalid(create(persistenceCluster("persist-wl-deployment", valkeyiov1alpha1.WorkloadTypeDeployment, persistenceSpec("1Gi", ""))), message)
		})

		It("accepts persistence with workloadType StatefulSet", func() {
			Expect(create(persistenceCluster("persist-wl-statefulset", valkeyiov1alpha1.WorkloadTypeStatefulSet, persistenceSpec("1Gi", "")))).To(Succeed())
		})

		It("accepts persistence with workloadType omitted (defaults to StatefulSet)", func() {
			Expect(create(persistenceCluster("persist-wl-default", "", persistenceSpec("1Gi", "")))).To(Succeed())
		})

		It("accepts workloadType Deployment without persistence", func() {
			Expect(create(persistenceCluster("persist-wl-deployment-none", valkeyiov1alpha1.WorkloadTypeDeployment, nil))).To(Succeed())
		})
	})

	Context("persistence cannot be removed once set", func() {
		It("rejects removing persistence", func() {
			Expect(create(persistenceCluster("persist-remove", "", persistenceSpec("1Gi", "")))).To(Succeed())
			expectInvalid(update("persist-remove", func(c *valkeyiov1alpha1.ValkeyCluster) {
				c.Spec.Persistence = nil
			}), "persistence cannot be removed once set")
		})

		It("accepts an unrelated update while persistence is kept", func() {
			Expect(create(persistenceCluster("persist-keep", "", persistenceSpec("1Gi", "")))).To(Succeed())
			Expect(update("persist-keep", bumpReplicas)).To(Succeed())
		})
	})

	Context("persistence cannot be added after creation", func() {
		It("rejects adding persistence to a cluster created without it", func() {
			Expect(create(persistenceCluster("persist-add", "", nil))).To(Succeed())
			expectInvalid(update("persist-add", func(c *valkeyiov1alpha1.ValkeyCluster) {
				c.Spec.Persistence = persistenceSpec("1Gi", "")
			}), "persistence cannot be added after creation")
		})

		It("accepts an unrelated update to a cluster created without persistence", func() {
			Expect(create(persistenceCluster("persist-absent", "", nil))).To(Succeed())
			Expect(update("persist-absent", bumpReplicas)).To(Succeed())
		})

		// Transition rules are not evaluated on create, so admission of the
		// create alone says nothing about this rule; the update does.
		It("accepts updating a cluster created with persistence", func() {
			Expect(create(persistenceCluster("persist-present", "", persistenceSpec("1Gi", "")))).To(Succeed())
			Expect(update("persist-present", bumpReplicas)).To(Succeed())
		})
	})

	Context("persistence.size may only be expanded", func() {
		const message = "persistence.size may only be expanded"

		It("rejects shrinking 2Gi to 1Gi", func() {
			Expect(create(persistenceCluster("persist-size-shrink", "", persistenceSpec("2Gi", "")))).To(Succeed())
			expectInvalid(update("persist-size-shrink", func(c *valkeyiov1alpha1.ValkeyCluster) {
				c.Spec.Persistence.Size = resource.MustParse("1Gi")
			}), message)
		})

		It("accepts expanding 1Gi to 2Gi", func() {
			Expect(create(persistenceCluster("persist-size-expand", "", persistenceSpec("1Gi", "")))).To(Succeed())
			Expect(update("persist-size-expand", func(c *valkeyiov1alpha1.ValkeyCluster) {
				c.Spec.Persistence.Size = resource.MustParse("2Gi")
			})).To(Succeed())
		})

		It("accepts keeping 1Gi", func() {
			Expect(create(persistenceCluster("persist-size-same", "", persistenceSpec("1Gi", "")))).To(Succeed())
			Expect(update("persist-size-same", bumpReplicas)).To(Succeed())
		})

		// The typed client canonicalises 1024Mi to "1Gi" on the wire, so only
		// a raw patch sends a different notation of the same size. As strings,
		// "1024Mi" sorts before "1Gi", so this catches a rule that compares
		// strings instead of quantities.
		It("accepts 1Gi to 1024Mi (same size, different notation)", func() {
			cluster := persistenceCluster("persist-size-notation", "", persistenceSpec("1Gi", ""))
			Expect(create(cluster)).To(Succeed())
			Expect(k8sClient.Patch(ctx, cluster, client.RawPatch(types.MergePatchType,
				[]byte(`{"spec":{"persistence":{"size":"1024Mi"}}}`)))).To(Succeed())

			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(valkeyiov1alpha1.GroupVersion.WithKind("ValkeyCluster"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "persist-size-notation", Namespace: "default"}, stored)).To(Succeed())
			size, _, err := unstructured.NestedString(stored.Object, "spec", "persistence", "size")
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(Equal("1024Mi"))
		})
	})

	Context("persistence.storageClassName is immutable", func() {
		const message = "persistence.storageClassName is immutable"

		// setStorageClass returns a mutation that sets storageClassName, or
		// unsets it when name is empty.
		setStorageClass := func(name string) func(*valkeyiov1alpha1.ValkeyCluster) {
			return func(c *valkeyiov1alpha1.ValkeyCluster) {
				c.Spec.Persistence.StorageClassName = nil
				if name != "" {
					c.Spec.Persistence.StorageClassName = &name
				}
			}
		}

		It("rejects changing fast to slow", func() {
			Expect(create(persistenceCluster("persist-sc-change", "", persistenceSpec("1Gi", "fast")))).To(Succeed())
			expectInvalid(update("persist-sc-change", setStorageClass("slow")), message)
		})

		It("rejects setting it when it was unset", func() {
			Expect(create(persistenceCluster("persist-sc-set", "", persistenceSpec("1Gi", "")))).To(Succeed())
			expectInvalid(update("persist-sc-set", setStorageClass("fast")), message)
		})

		It("rejects unsetting it when it was set", func() {
			Expect(create(persistenceCluster("persist-sc-unset", "", persistenceSpec("1Gi", "fast")))).To(Succeed())
			expectInvalid(update("persist-sc-unset", setStorageClass("")), message)
		})

		It("accepts leaving it unset", func() {
			Expect(create(persistenceCluster("persist-sc-none", "", persistenceSpec("1Gi", "")))).To(Succeed())
			Expect(update("persist-sc-none", bumpReplicas)).To(Succeed())
		})

		It("accepts keeping fast", func() {
			Expect(create(persistenceCluster("persist-sc-same", "", persistenceSpec("1Gi", "fast")))).To(Succeed())
			Expect(update("persist-sc-same", bumpReplicas)).To(Succeed())
		})
	})
})
