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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// valkeyFor builds a minimal Valkey for exercising the CEL validation.
func valkeyFor(name string, replicas int32) *valkeyiov1alpha1.Valkey {
	return &valkeyiov1alpha1.Valkey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: valkeyiov1alpha1.ValkeySpec{
			Replicas: replicas,
		},
	}
}

var _ = Describe("Valkey CEL validation", func() {
	ctx := context.Background()

	It("admits a standalone instance", func() {
		valkey := valkeyFor("val-standalone", 0)
		Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
		Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
	})

	It("rejects replicas above zero until replication lands", func() {
		err := k8sClient.Create(ctx, valkeyFor("val-replicated", 1))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("replication is not implemented yet"))
	})

	Describe("spec.config", func() {
		configValkey := func(name string, config map[string]string) *valkeyiov1alpha1.Valkey {
			valkey := valkeyFor(name, 0)
			valkey.Spec.Config = config
			return valkey
		}

		It("admits non-cluster keys", func() {
			valkey := configValkey("cfg-ok", map[string]string{
				"maxmemory":                 "100mb",
				"maxmemory-policy":          "allkeys-lru",
				"appendonly":                "yes",
				"timeout":                   "0",
				"maxmemory-clients":         "0",
				"latency-monitor-threshold": "100",
			})
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
		})

		It("admits an empty config", func() {
			valkey := configValkey("cfg-empty", map[string]string{})
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
		})

		DescribeTable("rejects cluster mode directives",
			func(key string) {
				err := k8sClient.Create(ctx, configValkey("cfg-reject", map[string]string{key: "yes"}))
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("must not contain cluster- keys"))
			},
			Entry("cluster-enabled", "cluster-enabled"),
			Entry("cluster-node-timeout", "cluster-node-timeout"),
			Entry("cluster-config-file", "cluster-config-file"),
			Entry("cluster-require-full-coverage", "cluster-require-full-coverage"),
			// Valkey config keys are case-insensitive, so the rule lowercases.
			// A bare startsWith would let these through.
			Entry("mixed case", "Cluster-Enabled"),
			Entry("upper case", "CLUSTER-ENABLED"),
		)

		It("rejects a cluster key mixed in with valid ones", func() {
			err := k8sClient.Create(ctx, configValkey("cfg-mixed", map[string]string{
				"maxmemory":       "100mb",
				"cluster-enabled": "yes",
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must not contain cluster- keys"))
		})

		It("rejects a cluster key added by update", func() {
			valkey := configValkey("cfg-update", map[string]string{"maxmemory": "100mb"})
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})

			valkey.Spec.Config["cluster-enabled"] = "yes"
			err := k8sClient.Update(ctx, valkey)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must not contain cluster- keys"))
		})

		It("admits a key that merely contains cluster", func() {
			// The rule is a prefix test, so it must not catch unrelated keys.
			valkey := configValkey("cfg-substring", map[string]string{
				"maxmemory-clients": "0",
			})
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
		})
	})

	DescribeTable("rejects names that collide with derived ValkeyNode names",
		func(name string) {
			err := k8sClient.Create(ctx, valkeyFor(name, 0))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("reserved for derived ValkeyNode names"))
		},
		Entry("per-node suffix", "val-0"),
		Entry("multi-digit per-node suffix", "val-12"),
	)

	DescribeTable("admits names whose suffix is not a derived one",
		func(name string) {
			valkey := valkeyFor(name, 0)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
		},
		// These were reserved while the design still had role Services.
		// Those Services were dropped, so the names are legal again.
		Entry("former primary Service suffix", "val-primary"),
		Entry("former replicas Service suffix", "val-replicas"),
	)

	It("rejects a name too long for derived child names", func() {
		// 38 characters, one over the limit.
		// The system password Secret is the longest derived name.
		// The limit keeps it inside the 63 character DNS label cap.
		err := k8sClient.Create(ctx, valkeyFor(strings.Repeat("a", 38), 0))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("at most 37 characters"))
	})

	It("admits a name at the length limit", func() {
		valkey := valkeyFor(strings.Repeat("b", 37), 0)
		Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
		Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
	})

	// The persistence rules are copied from ValkeyClusterSpec.
	// They are exercised against the Valkey spec shape rather than trusted.
	Describe("spec.persistence", func() {
		persistentValkey := func(name, size string, class *string) *valkeyiov1alpha1.Valkey {
			valkey := valkeyFor(name, 0)
			valkey.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{
				Size:             resource.MustParse(size),
				StorageClassName: class,
			}
			return valkey
		}

		It("admits an instance created with persistence", func() {
			valkey := persistentValkey("pv-ok", "1Gi", nil)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
		})

		It("rejects persistence with workloadType Deployment", func() {
			valkey := persistentValkey("pv-deploy", "1Gi", nil)
			valkey.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeDeployment
			err := k8sClient.Create(ctx, valkey)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("persistence requires workloadType StatefulSet"))
		})

		It("rejects persistence added after creation", func() {
			valkey := valkeyFor("pv-added", 0)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})

			valkey.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{
				Size: resource.MustParse("1Gi"),
			}
			err := k8sClient.Update(ctx, valkey)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("persistence cannot be added after creation"))
		})

		It("rejects persistence removed once set", func() {
			valkey := persistentValkey("pv-removed", "1Gi", nil)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})

			valkey.Spec.Persistence = nil
			err := k8sClient.Update(ctx, valkey)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("persistence cannot be removed once set"))
		})

		It("admits an expanded size", func() {
			valkey := persistentValkey("pv-expand", "1Gi", nil)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})

			valkey.Spec.Persistence.Size = resource.MustParse("2Gi")
			Expect(k8sClient.Update(ctx, valkey)).To(Succeed())
		})

		It("rejects a shrunk size", func() {
			valkey := persistentValkey("pv-shrink", "2Gi", nil)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})

			valkey.Spec.Persistence.Size = resource.MustParse("1Gi")
			err := k8sClient.Update(ctx, valkey)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("persistence.size may only be expanded"))
		})

		It("admits an equal size, so a no-op update is not a shrink", func() {
			valkey := persistentValkey("pv-same", "1Gi", nil)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})

			valkey.Spec.Persistence.Size = resource.MustParse("1Gi")
			Expect(k8sClient.Update(ctx, valkey)).To(Succeed())
		})

		It("treats equivalent quantities as equal, not as a shrink", func() {
			// 1Gi and 1024Mi are the same size written differently.
			// The rule compares quantities, so neither ordering is a shrink.
			valkey := persistentValkey("pv-equiv", "1Gi", nil)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})

			valkey.Spec.Persistence.Size = resource.MustParse("1024Mi")
			Expect(k8sClient.Update(ctx, valkey)).To(Succeed())
		})

		It("holds storageClassName immutable", func() {
			fast, slow := "fast", "slow"
			valkey := persistentValkey("pv-class", "1Gi", &fast)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})

			valkey.Spec.Persistence.StorageClassName = &slow
			err := k8sClient.Update(ctx, valkey)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("persistence.storageClassName is immutable"))
		})

		It("rejects adding a storageClassName that was unset", func() {
			class := "fast"
			valkey := persistentValkey("pv-class-add", "1Gi", nil)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})

			valkey.Spec.Persistence.StorageClassName = &class
			err := k8sClient.Update(ctx, valkey)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("persistence.storageClassName is immutable"))
		})
	})

	Describe("spec.failover", func() {
		It("defaults mode to None when the block is omitted", func() {
			valkey := valkeyFor("fo-default", 0)
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
			})
			Expect(valkey.FailoverMode()).To(Equal(valkeyiov1alpha1.FailoverModeNone))
		})

		It("admits mode None set explicitly", func() {
			valkey := valkeyFor("fo-none", 0)
			valkey.Spec.Failover = &valkeyiov1alpha1.FailoverSpec{
				Mode: valkeyiov1alpha1.FailoverModeNone,
			}
			Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
			Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
		})

		It("rejects mode Sentinel until it is implemented", func() {
			valkey := valkeyFor("fo-sentinel", 0)
			valkey.Spec.Failover = &valkeyiov1alpha1.FailoverSpec{
				Mode: valkeyiov1alpha1.FailoverModeSentinel,
			}
			err := k8sClient.Create(ctx, valkey)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be None"))
		})

		It("rejects an unknown mode", func() {
			valkey := valkeyFor("fo-bogus", 0)
			valkey.Spec.Failover = &valkeyiov1alpha1.FailoverSpec{
				Mode: valkeyiov1alpha1.FailoverMode("Operator"),
			}
			err := k8sClient.Create(ctx, valkey)
			Expect(err).To(HaveOccurred())
		})

		// The two monitorName transition rules are not covered, and cannot be.
		// They need a sentinel block, which needs mode Sentinel.
		// The spec-level rule above rejects that mode.
		// Add those cases with the change that admits it.

		It("rejects a sentinel block under mode None", func() {
			valkey := valkeyFor("fo-orphan-block", 0)
			valkey.Spec.Failover = &valkeyiov1alpha1.FailoverSpec{
				Mode:     valkeyiov1alpha1.FailoverModeNone,
				Sentinel: &valkeyiov1alpha1.SentinelFailoverSpec{},
			}
			err := k8sClient.Create(ctx, valkey)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("only valid when failover.mode is Sentinel"))
		})
	})

	It("holds workloadType immutable", func() {
		valkey := valkeyFor("val-immutable", 0)
		valkey.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeStatefulSet
		Expect(k8sClient.Create(ctx, valkey)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, valkey)).To(Succeed())
		})

		valkey.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeDeployment
		err := k8sClient.Update(ctx, valkey)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("workloadType is immutable"))
	})
})
