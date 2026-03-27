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
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

// getSampleCluster parses the v1alpha1_valkeycluster.yaml sample file
// and returns the ValkeyCluster object.
func getSampleCluster(t *testing.T) *valkeyiov1alpha1.ValkeyCluster {
	valkeyClusterCR := "../../config/samples/v1alpha1_valkeycluster-with-modules.yaml"

	data, err := os.ReadFile(valkeyClusterCR)
	if err != nil {
		t.Fatalf("failed to read sample YAML file: %v", err)
	}

	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	if err := yaml.Unmarshal(data, cluster); err != nil {
		t.Fatalf("failed to unmarshal sample YAML: %v", err)
	}

	return cluster
}

func TestGetUserConfig(t *testing.T) {

	ctx := t.Context()
	cluster := getSampleCluster(t)

	baseConfig := getBaseConfig()
	baseConfigLen := len(baseConfig)
	if baseConfigLen != 119 {
		t.Fatalf("unexpected base config length: got %d, want 118", baseConfigLen)
	}

	userConfig := getUserConfig(ctx, cluster)
	if !strings.Contains(userConfig, "maxmemory-policy") {
		t.Fatalf("unexpected user config: missing 'maxmemory-policy'")
	}

	// Fake K8S Client
	k8sClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	// Fake reconciler
	reconciler := &ValkeyClusterReconciler{
		Client: k8sClient,
	}

	modulesConfig := getModulesConfig(ctx, cluster, reconciler)
	if !strings.Contains(modulesConfig, "# Modules") {
		t.Fatalf("unexpected modules config: missing '# Modules' header")
	}
	if !strings.Contains(modulesConfig, "bloom-fp-rate") {
		t.Fatalf("unexpected modules config: missing 'bloom' module")
	}
	if !strings.Contains(modulesConfig, "max-path-limit") {
		t.Fatalf("unexpected modules config: missing 'max-path-limit' parameter")
	}
}
