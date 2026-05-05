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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/tools/events"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/internal/valkey"
)

const (
	proactiveFailoverTimeout = 10 * time.Second
	proactiveFailoverPoll    = 1 * time.Second

	eventReasonFailoverInitiated = "FailoverInitiated"
	eventReasonFailoverFailed    = "FailoverFailed"
	eventReasonFailoverTimeout   = "FailoverTimeout"
	eventReasonFailoverCompleted = "FailoverCompleted"
	eventActionProactiveFailover = "ProactiveFailover"
)

// findFailoverShard returns the shard and its synced replicas if the node at
// address is a primary that should be gracefully failed over before being
// updated, or nil if no failover is needed.
func findFailoverShard(state *valkey.ClusterState, address string) (*valkey.ShardState, []*valkey.NodeState) {
	shard := state.FindShardForAddress(address)
	if shard == nil {
		return nil, nil
	}
	primary := shard.GetPrimaryNode()
	if primary == nil || primary.Address != address {
		return nil, nil
	}
	replicas := shard.GetSyncedReplicas()
	if len(replicas) == 0 {
		return nil, nil
	}
	return shard, replicas
}

// proactiveFailover issues CLUSTER FAILOVER to the best synced replica in shard,
// then polls until the replica reports role:master or the timeout is reached.
// shard must be non-nil; replicas must be non-empty.
func proactiveFailover(ctx context.Context, recorder events.EventRecorder, cluster *valkeyiov1alpha1.ValkeyCluster, shard *valkey.ShardState, replicas []*valkey.NodeState) error {
	log := logf.FromContext(ctx)
	primaryAddress := shard.GetPrimaryNode().Address

	// Pick the first synced replica as the failover target. The ordering is
	// determined by node discovery order — no priority scheme is applied yet.
	target := replicas[0]
	log.Info("initiating proactive failover", "shard", shard.Id, "target", target.Address)

	// Emit FailoverInitiated before the command so observers see the event at
	// the moment the failover begins, not after.
	recorder.Eventf(cluster, nil, corev1.EventTypeNormal, eventReasonFailoverInitiated, eventActionProactiveFailover, "Initiated failover from %s to %s in shard %s", primaryAddress, target.Address, shard.Id)

	err := target.Client.Do(ctx, target.Client.B().ClusterFailover().Build()).Error()
	if err != nil {
		recorder.Eventf(cluster, nil, corev1.EventTypeWarning, eventReasonFailoverFailed, eventActionProactiveFailover, "CLUSTER FAILOVER command failed on %s: %v", target.Address, err)
		return fmt.Errorf("CLUSTER FAILOVER failed on %s: %w", target.Address, err)
	}

	timer := time.NewTimer(proactiveFailoverTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(proactiveFailoverPoll)
	defer ticker.Stop()

	for {
		select {
		case <-timer.C:
			recorder.Eventf(cluster, nil, corev1.EventTypeWarning, eventReasonFailoverTimeout, eventActionProactiveFailover, "Failover to %s in shard %s did not complete within %s", target.Address, shard.Id, proactiveFailoverTimeout)
			return fmt.Errorf("failover to %s timed out after %s", target.Address, proactiveFailoverTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			info, err := target.Client.Do(ctx, target.Client.B().Info().Section("replication").Build()).ToString()
			if err != nil {
				log.V(1).Info("failed to query INFO replication during failover poll", "target", target.Address, "err", err)
				continue
			}
			role := parseValkeyRole(info)
			if role == RolePrimary {
				recorder.Eventf(cluster, nil, corev1.EventTypeNormal, eventReasonFailoverCompleted, eventActionProactiveFailover, "Failover completed: %s is now primary in shard %s", target.Address, shard.Id)
				log.Info("proactive failover completed", "newPrimary", target.Address, "shard", shard.Id)
				failoversTotal.WithLabelValues(cluster.Name, cluster.Namespace, "proactive").Inc()
				return nil
			}
		}
	}
}

// nodeRequiresRoll returns true when the node has a running pod whose spec
// differs from the desired spec, meaning a spec update will trigger a pod roll.
func nodeRequiresRoll(current *valkeyiov1alpha1.ValkeyNode, desired *valkeyiov1alpha1.ValkeyNode) bool {
	return current.Status.PodIP != "" && !equality.Semantic.DeepEqual(current.Spec, desired.Spec)
}

// anyNodeRequiresRoll returns true if any existing ValkeyNode in the list has
// a spec diff against what the cluster would build for it. Used as a cheap
// pre-flight check to avoid opening Valkey connections on steady-state reconciles.
func anyNodeRequiresRoll(cluster *valkeyiov1alpha1.ValkeyCluster, nodeList *valkeyiov1alpha1.ValkeyNodeList) bool {
	byName := make(map[string]*valkeyiov1alpha1.ValkeyNode, len(nodeList.Items))
	for i := range nodeList.Items {
		byName[nodeList.Items[i].Name] = &nodeList.Items[i]
	}
	nodesPerShard := 1 + int(cluster.Spec.Replicas)
	for shardIndex := range int(cluster.Spec.Shards) {
		for nodeIndex := range nodesPerShard {
			desired := buildClusterValkeyNode(cluster, shardIndex, nodeIndex)
			if current, ok := byName[desired.Name]; ok && nodeRequiresRoll(current, desired) {
				return true
			}
		}
	}
	return false
}
