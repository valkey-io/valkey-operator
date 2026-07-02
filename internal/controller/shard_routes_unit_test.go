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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
)

func TestGatewayListenerPort(t *testing.T) {
	// 2 nodes per shard (1 primary + 1 replica), basePort 30000: a flat index
	// across the cluster so every node maps to a distinct listener.
	assert.Equal(t, int32(30000), gatewayListenerPort(30000, 0, 0, 2))
	assert.Equal(t, int32(30001), gatewayListenerPort(30000, 0, 1, 2))
	assert.Equal(t, int32(30002), gatewayListenerPort(30000, 1, 0, 2))
	assert.Equal(t, int32(30003), gatewayListenerPort(30000, 1, 1, 2))
}

func TestResolveServiceType(t *testing.T) {
	t.Run("defaults to NodePort", func(t *testing.T) {
		assert.Equal(t, corev1.ServiceTypeNodePort, resolveServiceType(&valkeyiov1alpha1.ExternalAccessSpec{}))
	})
	t.Run("defaults to ClusterIP when a Gateway is set", func(t *testing.T) {
		ea := &valkeyiov1alpha1.ExternalAccessSpec{Gateway: &valkeyiov1alpha1.GatewayExposureSpec{}}
		assert.Equal(t, corev1.ServiceTypeClusterIP, resolveServiceType(ea))
	})
	t.Run("explicit serviceType always wins", func(t *testing.T) {
		ea := &valkeyiov1alpha1.ExternalAccessSpec{
			ServiceType: corev1.ServiceTypeLoadBalancer,
			Gateway:     &valkeyiov1alpha1.GatewayExposureSpec{},
		}
		assert.Equal(t, corev1.ServiceTypeLoadBalancer, resolveServiceType(ea))
	})
}

func TestExternalClientPort_Gateway(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Replicas = 1 // 2 nodes per shard
	cluster.Spec.ExternalAccess = &valkeyiov1alpha1.ExternalAccessSpec{
		Enabled: true,
		Gateway: &valkeyiov1alpha1.GatewayExposureSpec{BasePort: 30000},
	}

	// The Gateway port is deterministic and does not depend on status read-back.
	assert.Equal(t, int32(30002), externalClientPort(cluster, 1, 0))
	assert.Equal(t, int32(30003), externalClientPort(cluster, 1, 1))
}

func TestBuildShardRouteSpec(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Name = "mycluster"
	gw := &valkeyiov1alpha1.GatewayExposureSpec{
		GatewayRef: valkeyiov1alpha1.GatewayReference{Name: "public-gw"},
		BasePort:   30000,
	}

	spec := buildShardRouteSpec(cluster, gw, 1, 1, 2)

	require.Len(t, spec.ParentRefs, 1)
	assert.Equal(t, "public-gw", string(spec.ParentRefs[0].Name))
	require.NotNil(t, spec.ParentRefs[0].Port)
	assert.Equal(t, int32(30003), *spec.ParentRefs[0].Port, "parent listener port is basePort + flat index")

	require.Len(t, spec.Rules, 1)
	require.Len(t, spec.Rules[0].BackendRefs, 1)
	backend := spec.Rules[0].BackendRefs[0]
	assert.Equal(t, "valkey-mycluster-shard-1", string(backend.Name), "backend is the shard Service")
	require.NotNil(t, backend.Port)
	assert.Equal(t, int32(DefaultPort+1), *backend.Port, "backend port is the node's port on the shard Service")
}

func TestBuildShardRouteSpec_SectionName(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Name = "mycluster"
	section := "valkey"
	gw := &valkeyiov1alpha1.GatewayExposureSpec{
		GatewayRef: valkeyiov1alpha1.GatewayReference{Name: "public-gw", SectionName: &section},
		BasePort:   30000,
	}

	spec := buildShardRouteSpec(cluster, gw, 0, 0, 2)
	require.NotNil(t, spec.ParentRefs[0].SectionName)
	assert.Equal(t, "valkey", string(*spec.ParentRefs[0].SectionName))
}

func TestShardRouteName(t *testing.T) {
	assert.Equal(t, "valkey-mycluster-1-2", shardRouteName("mycluster", 1, 2))
}
