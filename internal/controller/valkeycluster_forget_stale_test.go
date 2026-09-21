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
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func nodeWithPodIP(name, podIP string) valkeyiov1alpha1.ValkeyNode {
	return valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: podIP},
	}
}

// TestHasNodeWithPodIP covers the address match forgetStaleNodes uses to decide
// whether a failing CLUSTER NODES entry still belongs to a known ValkeyNode.
// A `noaddr` entry has no host, and a ValkeyNode whose pod has no IP yet has
// none either; matching those two against each other would skip CLUSTER FORGET
// for an unrelated node.
func TestHasNodeWithPodIP(t *testing.T) {
	tests := []struct {
		name  string
		nodes []valkeyiov1alpha1.ValkeyNode
		podIP string
		want  bool
	}{
		{
			name:  "empty host does not match a node without a pod IP",
			nodes: []valkeyiov1alpha1.ValkeyNode{nodeWithPodIP("node-0-0", "")},
			podIP: "",
			want:  false,
		},
		{
			name:  "matching IP",
			nodes: []valkeyiov1alpha1.ValkeyNode{nodeWithPodIP("node-0-0", "10.0.0.1")},
			podIP: "10.0.0.1",
			want:  true,
		},
		{
			name:  "non-matching IP",
			nodes: []valkeyiov1alpha1.ValkeyNode{nodeWithPodIP("node-0-0", "10.0.0.1")},
			podIP: "10.0.0.2",
			want:  false,
		},
		{
			name:  "empty host does not match a node that has a pod IP",
			nodes: []valkeyiov1alpha1.ValkeyNode{nodeWithPodIP("node-0-0", "10.0.0.1")},
			podIP: "",
			want:  false,
		},
		{
			name: "matches one node among several",
			nodes: []valkeyiov1alpha1.ValkeyNode{
				nodeWithPodIP("node-0-0", ""),
				nodeWithPodIP("node-0-1", "10.0.0.1"),
				nodeWithPodIP("node-1-0", "10.0.0.2"),
			},
			podIP: "10.0.0.2",
			want:  true,
		},
		{
			name:  "no nodes",
			nodes: nil,
			podIP: "10.0.0.1",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasNodeWithPodIP(tt.nodes, tt.podIP))
		})
	}
}
