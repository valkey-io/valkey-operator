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
	})
})
