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
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"maps"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	valkeyv1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

const (
	appName        = "valkey"
	resourcePrefix = "valkey-"

	// shellPath used in exec for running scripts
	shellPath = "/bin/sh"
)

// Naming and labelling scheme
//
// Every ValkeyNode (and therefore every Pod) encodes the node's position in
// the Valkey cluster in its *name*:
//
//	<cluster>-<N>-<M>    e.g. "mycluster-0-0", "mycluster-1-2"
//
// where N is the shard index and M is the node index within the shard. By
// convention, node 0 is the initial primary and nodes 1, 2, … are replicas.
// The name deliberately avoids "primary"/"replica" because failover can swap
// roles at any time.
//
// Two labels are set for selector uniqueness and kubectl convenience:
//
//	valkey.io/shard-index  – which shard ("0", "1", …)
//	valkey.io/node-index   – node within shard ("0", "1", …)
//
// The reconciler reads ValkeyNode labels (via nodeRoleAndShard) to decide whether
// to assign slots (node 0 = initial primary) or issue CLUSTER REPLICATE
// (node 1+ = initial replica), and for which shard. After a failover,
// Valkey may promote a replica to primary, making node 0 a replica. The
// reconciler detects this via shardExistsInTopology: if the shard already
// has members in the cluster topology, the replacement node-index=0 pod
// joins as a replica instead of trying to claim slots. The labels themselves are not
// updated — the live role is always read from CLUSTER NODES.
//
// Names and labels are set by valkeyNodeName when creating ValkeyNode CRs.
const (
	// LabelCluster identifies the Valkey cluster (e.g. "mycluster").
	LabelCluster = "valkey.io/cluster"
	// LabelShardIndex identifies which shard a pod belongs to (e.g. "0", "1", "2").
	LabelShardIndex = "valkey.io/shard-index"
	// LabelNodeIndex identifies the node within a shard (e.g. "0", "1", "2").
	// Node 0 is the initial primary; nodes 1+ are replicas. Together with
	// LabelShardIndex this forms a unique selector per Deployment.
	LabelNodeIndex = "valkey.io/node-index"
)

const (
	// tlsVolumeName is the name of the volume that will be mounted in the Valkey container.
	tlsVolumeName = "tls-certs"
	// tlsCertMountPath is the path where the TLS certificates are mounted in the Valkey container.
	tlsCertMountPath = "/tls"
	dataVolumeName   = "data"
	dataMountPath    = "/data"
	tlsSecretKeyCA   = "ca.crt"
	tlsSecretKeyCert = "tls.crt"
	tlsSecretKeyKey  = "tls.key"
)

// Role label values.
const (
	RolePrimary = "primary"
	RoleReplica = "replica"
	RoleMaster  = "master"
	RoleSlave   = "slave"
)

// baseLabels returns the standard Kubernetes recommended labels for a Valkey
// resource with the given instance name and component type.
func baseLabels(name, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       appName,
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/part-of":    appName,
		"app.kubernetes.io/managed-by": "valkey-operator",
	}
}

// labels returns the standard Kubernetes recommended labels merged with any
// user-defined labels on the cluster. The k8s recommended labels always take
// precedence over user-defined labels with the same key.
func labels(cluster *valkeyv1.ValkeyCluster) map[string]string {
	l := baseLabels(cluster.Name, "valkey-cluster")
	for k, v := range cluster.Labels {
		if _, exists := l[k]; !exists {
			l[k] = v
		}
	}
	return l
}

// Annotations returns a copy of user defined annotations.
func annotations(cluster *valkeyv1.ValkeyCluster) map[string]string {
	return maps.Clone(cluster.Annotations)
}

// This function takes a K8S object reference (eg: pod, secret, configmap, etc),
// and a key, and value to add to, or replace an existing, annotation within the object.
// Returns true if the annotation was added, or updated
func upsertAnnotation(o metav1.Object, key string, val string) bool {

	// Get current annotations
	annotations := o.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// If found, and equal, then no update
	if annotations[key] == val {
		return false
	}

	annotations[key] = val
	o.SetAnnotations(annotations)

	return true
}

// nodeIndexForAddress finds the ValkeyNode whose Status.PodIP matches address
// and returns its node index from the LabelNodeIndex label.
// Returns -1 if the node is not found or the label is missing or invalid.
func nodeIndexForAddress(address string, nodes *valkeyv1.ValkeyNodeList) int {
	idx := slices.IndexFunc(nodes.Items, func(n valkeyv1.ValkeyNode) bool {
		return n.Status.PodIP == address
	})
	if idx == -1 {
		return -1
	}
	nodeIndex, err := strconv.Atoi(nodes.Items[idx].Labels[LabelNodeIndex])
	if err != nil {
		return -1
	}
	return nodeIndex
}

// nodeRoleAndShard finds the ValkeyNode whose Status.PodIP matches address
// and reads its CR labels to determine the intended role and shard index.
//
// The role is derived from valkey.io/node-index: node 0 is the initial
// primary, nodes 1+ are replicas. Returns ("", -1) if not found or labels missing.
func nodeRoleAndShard(address string, nodes *valkeyv1.ValkeyNodeList) (string, int) {
	idx := slices.IndexFunc(nodes.Items, func(n valkeyv1.ValkeyNode) bool {
		return n.Status.PodIP == address
	})
	if idx == -1 {
		return "", -1
	}
	node := &nodes.Items[idx]
	shardIndex, err := strconv.Atoi(node.Labels[LabelShardIndex])
	if err != nil {
		return "", -1
	}
	nodeIndex, err := strconv.Atoi(node.Labels[LabelNodeIndex])
	if err != nil || nodeIndex < 0 {
		return "", -1
	}
	if nodeIndex == 0 {
		return RolePrimary, shardIndex
	}
	return RoleReplica, shardIndex
}

// shardExistsInTopology reports whether another pod in the same shard (same
// shard-index label) already exists as a member of any shard in the Valkey
// cluster topology. This covers two cases:
//
//  1. Post-failover (completed): a promoted replica is the primary.
//  2. Mid-failover (in progress): the replica exists but hasn't been promoted
//     yet — the operator must wait rather than trying to assign new slots.
//
// In both cases the replacement node-index=0 pod must NOT call
// assignSlotsToNewPrimary. Instead it should fall through to
// replicateToShardPrimary, which will either succeed (case 1) or return an
// error and retry on the next reconcile (case 2).
func shardExistsInTopology(state *valkey.ClusterState, shardIndex int, nodes *valkeyv1.ValkeyNodeList) bool {
	si := strconv.Itoa(shardIndex)
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels[LabelShardIndex] != si || n.Status.PodIP == "" {
			continue
		}
		for _, shard := range state.Shards {
			for _, node := range shard.Nodes {
				if node.Address == n.Status.PodIP {
					return true
				}
			}
		}
	}
	return false
}

// findShardPrimary scans all pods with the given shard-index label and returns
// the Valkey node ID + IP of whichever pod is currently the slot-bearing
// primary, regardless of its node-index label. This handles the post-failover
// case where node-index=1 (or higher) was promoted by Valkey.
// Returns ("", "") if no primary is found.
func findShardPrimary(state *valkey.ClusterState, shardIndex int, nodes *valkeyv1.ValkeyNodeList) (nodeID, ip string) {
	si := strconv.Itoa(shardIndex)
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels[LabelShardIndex] != si || n.Status.PodIP == "" {
			continue
		}
		for _, shard := range state.Shards {
			if len(shard.Slots) == 0 {
				continue
			}
			primary := shard.GetPrimaryNode()
			if primary != nil && primary.Address == n.Status.PodIP {
				return primary.Id, n.Status.PodIP
			}
		}
	}
	return "", ""
}

// primaryNodeIndexForShard returns the node index of the current slot-bearing
// primary for the given shard, using clusterState as the source of truth.
// Returns -1 if clusterState is nil, the shard has no primary in the topology,
// or the primary's node index cannot be resolved or is out of range.
func primaryNodeIndexForShard(shardIndex, nodesPerShard int, nodes *valkeyv1.ValkeyNodeList, clusterState *valkey.ClusterState) int {
	if clusterState == nil {
		return -1
	}
	_, primaryIP := findShardPrimary(clusterState, shardIndex, nodes)
	if primaryIP == "" {
		return -1
	}
	idx := nodeIndexForAddress(primaryIP, nodes)
	if idx < 0 || idx >= nodesPerShard {
		return -1
	}
	return idx
}

// replicaFirstNodeOrder returns the node indices for a shard ordered so that
// replicas are processed before the primary. The primary is identified via
// clusterState — the live cluster topology — rather than by node-index convention,
// which may be stale after a failover.
//
// When the primary cannot be identified, indices are returned in descending order
// (nodesPerShard-1 … 0), keeping node-index=0 (the conventional primary) last.
func replicaFirstNodeOrder(shardIndex, nodesPerShard int, nodes *valkeyv1.ValkeyNodeList, clusterState *valkey.ClusterState) []int {
	primaryIdx := primaryNodeIndexForShard(shardIndex, nodesPerShard, nodes, clusterState)

	order := make([]int, 0, nodesPerShard)
	if primaryIdx < 0 {
		// No live primary identified — fall back to descending order, keeping
		// node-index=0 (the conventional primary) last.
		for i := nodesPerShard - 1; i >= 0; i-- {
			order = append(order, i)
		}
		return order
	}
	for i := range nodesPerShard {
		if i != primaryIdx {
			order = append(order, i)
		}
	}
	order = append(order, primaryIdx)
	return order
}

func countSlots(ranges []valkey.SlotsRange) int {
	count := 0
	for _, slot := range ranges {
		count += slot.End - slot.Start + 1
	}
	return count
}

// valkeyNodeName returns the deterministic name for a ValkeyNode CR.
// The name encodes the shard index and node index within the shard:
//
//	<cluster>-<N>-<M>    e.g. "mycluster-0-0", "mycluster-1-2"
//
// By convention, node 0 is the initial primary and nodes 1, 2, … are
// replicas.
func valkeyNodeName(clusterName string, shardIndex int, nodeIndex int) string {
	return fmt.Sprintf("%s-%d-%d", clusterName, shardIndex, nodeIndex)
}

// shardClientPortName returns the node-unique name of the server container's
// client port. A per-shard Service targets each node individually by referencing
// this name as its targetPort, so only the matching pod is added to the endpoints
// of that Service port. The name fits the 15-character port-name limit.
func shardClientPortName(nodeIndex string) string {
	return "vk-n" + nodeIndex
}

// shardHostname returns the client-facing hostname a node announces, formed as
// "<prefix>-<shardIndex>.<domain>". All nodes in a shard share one hostname; they
// are distinguished by their announced client port.
func shardHostname(prefix, shardIndex, domain string) string {
	return fmt.Sprintf("%s-%s.%s", prefix, shardIndex, domain)
}

// getTLSConfig returns the TLS configuration for a ValkeyCluster.
func getTLSConfig(ctx context.Context, c client.Reader, secretName, serverName, namespace string) (*tls.Config, error) {
	secret := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName}, secret)
	if err != nil {
		return nil, err
	}

	caData, caOk := secret.Data[tlsSecretKeyCA]

	if !caOk {
		return nil, fmt.Errorf("TLS secret is missing required key: ca=%v", caOk)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("failed to parse CA certificates from secret key %q", "ca.crt")
	}

	tlsCfg := &tls.Config{
		RootCAs:    caCertPool,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}
	return tlsCfg, nil
}
