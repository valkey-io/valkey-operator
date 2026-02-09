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
	"maps"

	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
)

const appName = "valkey"

// Shard-label scheme
//
// Every Deployment (and therefore every Pod) is stamped at creation time with
// two labels that encode the node's intended position in the Valkey cluster:
//
//	valkey.io/shard-index  – which shard the node belongs to ("0", "1", …)
//	valkey.io/role         – "primary" or "replica"
//
// This makes the reconciler's job deterministic: when a pending node appears,
// addValkeyNode reads the pod labels to decide whether to assign slots
// (primary) or issue CLUSTER REPLICATE (replica), and for which shard. Without
// these labels the controller would have to infer the role by counting shards
// and comparing to the spec, which is fragile when cluster state is stale due
// to gossip delays.
//
// The labels are set by upsertDeployments → createClusterDeployment and
// consumed by addValkeyNode → podRoleAndShard.
const (
	// LabelShardIndex identifies which shard a pod belongs to (e.g. "0", "1", "2").
	LabelShardIndex = "valkey.io/shard-index"
	// LabelRole identifies the intended role of a pod: "primary" or "replica".
	LabelRole = "valkey.io/role"
)

// Role label values.
const (
	RolePrimary = "primary"
	RoleReplica = "replica"
)

// Labels returns a copy of user defined labels including recommended:
// https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labels(cluster *valkeyv1.ValkeyCluster) map[string]string {
	if cluster.Labels == nil {
		cluster.Labels = make(map[string]string)
	}
	l := maps.Clone(cluster.Labels)
	l["app.kubernetes.io/name"] = appName
	l["app.kubernetes.io/instance"] = cluster.Name
	l["app.kubernetes.io/component"] = "valkey-cluster"
	l["app.kubernetes.io/part-of"] = appName
	l["app.kubernetes.io/managed-by"] = "valkey-operator"
	return l
}

// Annotations returns a copy of user defined annotations.
func annotations(cluster *valkeyv1.ValkeyCluster) map[string]string {
	return maps.Clone(cluster.Annotations)
}
