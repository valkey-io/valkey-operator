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
	"testing"

	"github.com/stretchr/testify/require"
	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The cluster controller hashes the node it builds before the owner reference
// is set; the node controller hashes the stored node. Both must agree.
func TestWorkloadRevisionParityWithEntrypoint(t *testing.T) {
	cluster := &valkeyv1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}, Spec: valkeyv1.ValkeyClusterSpec{Shards: 1}}
	desired := buildClusterValkeyNode(cluster, 0, 0)
	before, err := computeWorkloadRevision(desired)
	require.NoError(t, err)
	stored := desired.DeepCopy()
	controller := true
	stored.OwnerReferences = []metav1.OwnerReference{{Kind: "ValkeyCluster", Name: "c", Controller: &controller}}
	after, err := computeWorkloadRevision(stored)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.True(t, usesEntrypoint(desired), "a cluster node without persistence runs the entrypoint")
}
