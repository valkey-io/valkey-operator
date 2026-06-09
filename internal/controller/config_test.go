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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// getSampleCluster returns a ValkeyCluster object with config options.
func getSampleCluster() *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		Spec: valkeyiov1alpha1.ValkeyClusterSpec{
			Config: map[string]string{
				"maxmemory":        "50mb",
				"maxmemory-policy": "allkeys-lfu",
			},
		},
	}
}

var _ = Describe("When creating a cluster", Label("userconfig"), func() {
	It("should have base, and user-supplied configuration", func() {
		By("verifying the rendered config")
		cluster := getSampleCluster()

		testConfigString := buildServerConfig(cluster)

		// Check user-added parameter
		Expect(testConfigString).To(ContainSubstring("maxmemory-policy"))

		// Base, non-overridable parameter
		Expect(testConfigString).To(ContainSubstring("cluster-enabled"))
		Expect(testConfigString).To(ContainSubstring("primaryauth sup3rSecr3tP4ssw0rd"))
	})
})

var _ = Describe("Live config", Label("liveconfig"), func() {
	newCluster := func(cfg map[string]string) *valkeyiov1alpha1.ValkeyCluster {
		return &valkeyiov1alpha1.ValkeyCluster{
			Spec: valkeyiov1alpha1.ValkeyClusterSpec{Config: cfg},
		}
	}

	It("excludes allowlisted keys from the roll config but keeps others and base", func() {
		cluster := newCluster(map[string]string{
			"maxmemory-policy": "allkeys-lru", // allowlisted
			"appendonly":       "yes",         // not allowlisted
		})
		rollConfig := buildRollServerConfig(cluster)
		Expect(rollConfig).NotTo(ContainSubstring("maxmemory-policy"))
		Expect(rollConfig).To(ContainSubstring("appendonly"))
		Expect(rollConfig).To(ContainSubstring("cluster-enabled")) // base retained
	})

	It("keeps the roll hash stable when only an allowlisted key changes", func() {
		before := serverConfigRollHash(newCluster(map[string]string{
			"maxmemory-policy": "allkeys-lru",
			"appendonly":       "yes",
		}))
		after := serverConfigRollHash(newCluster(map[string]string{
			"maxmemory-policy": "volatile-lru",
			"appendonly":       "yes",
		}))
		Expect(after).To(Equal(before))
	})

	It("changes the roll hash when a non-allowlisted key changes", func() {
		before := serverConfigRollHash(newCluster(map[string]string{"appendonly": "yes"}))
		after := serverConfigRollHash(newCluster(map[string]string{"appendonly": "no"}))
		Expect(after).NotTo(Equal(before))
	})

	It("liveConfigToApply returns only allowlisted keys present in config", func() {
		out := liveConfigToApply(map[string]string{
			"maxmemory-policy": "allkeys-lru",
			"appendonly":       "yes",
		})
		Expect(out).To(Equal(map[string]string{"maxmemory-policy": "allkeys-lru"}))
	})
})
