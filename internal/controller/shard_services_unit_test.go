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
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildShardServicePorts(t *testing.T) {
	ports := buildShardServicePorts(nil, 3)
	require.Len(t, ports, 3)

	for i, p := range ports {
		// Port is the in-cluster port and must be unique within the Service.
		assert.Equal(t, int32(DefaultPort+i), p.Port)
		// targetPort references the node-unique container port name so the port
		// reaches exactly one pod.
		name := "vk-n" + strconv.Itoa(i)
		assert.Equal(t, name, p.Name)
		assert.Equal(t, name, p.TargetPort.StrVal)
		// No NodePort is requested; Kubernetes allocates it.
		assert.Equal(t, int32(0), p.NodePort)
	}
}

func TestBuildShardServicePortsPreservesAllocatedNodePorts(t *testing.T) {
	existing := []corev1.ServicePort{
		{Name: "vk-n0", NodePort: 31000},
		{Name: "vk-n1", NodePort: 31001},
	}
	ports := buildShardServicePorts(existing, 2)
	require.Len(t, ports, 2)
	assert.Equal(t, int32(31000), ports[0].NodePort, "allocated NodePort must be preserved across updates")
	assert.Equal(t, int32(31001), ports[1].NodePort)
}

func TestShardEndpointFromService(t *testing.T) {
	svc := &corev1.Service{}
	svc.Spec.Ports = []corev1.ServicePort{
		{Name: "vk-n0", Port: DefaultPort, NodePort: 31000},
		{Name: "vk-n1", Port: DefaultPort + 1, NodePort: 31001},
	}

	nodePort := shardEndpointFromService(2, svc, corev1.ServiceTypeNodePort, 2)
	assert.Equal(t, int32(2), nodePort.ShardIndex)
	assert.Equal(t, []int32{31000, 31001}, nodePort.NodePorts, "NodePort type reports allocated node ports")

	loadBalancer := shardEndpointFromService(2, svc, corev1.ServiceTypeLoadBalancer, 2)
	assert.Equal(t, []int32{DefaultPort, DefaultPort + 1}, loadBalancer.NodePorts, "LoadBalancer type reports frontend ports")
}
