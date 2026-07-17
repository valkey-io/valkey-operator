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

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
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

type FailoverType string

const (
	FailoverProactive FailoverType = "proactive"
)

var FailoverTypes = []FailoverType{
	FailoverProactive,
}

func (ft FailoverType) String() string {
	return string(ft)
}

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

	// Fail over to the most caught-up replica (highest replication offset). A
	// graceful CLUSTER FAILOVER holds writes on the primary until the target
	// replica catches up, so promoting the furthest-ahead one minimises that
	// write pause and the exposure if the primary dies mid-failover.
	target := valkey.HighestOffsetReplica(replicas)
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
				failoversTotal.WithLabelValues(cluster.Name, cluster.Namespace, FailoverProactive.String()).Inc()
				return nil
			}
		}
	}
}

// nodeRequiresRoll returns true when the node has a running pod whose spec
// differs from the desired spec, meaning a spec update will trigger a pod roll.
func nodeRequiresRoll(current *valkeyiov1alpha1.ValkeyNode, desired *valkeyiov1alpha1.ValkeyNode) bool {
	if current.Status.PodIP == "" {
		return false
	}
	// Config changes are applied live via CONFIG SET (see applyLiveConfig) and
	// must not trigger a roll; the roll-relevant config subset is already
	// captured by Spec.ServerConfigHash.
	currentSpec, desiredSpec := current.Spec, desired.Spec
	currentSpec.Config, desiredSpec.Config = nil, nil
	return !equality.Semantic.DeepEqual(currentSpec, desiredSpec)
}

// anyNodeRequiresRoll returns true if any existing ValkeyNode in the list has
// a spec diff against what the cluster would build for it. Used as a cheap
// pre-flight check to avoid opening Valkey connections on steady-state reconciles.
//
// configHash must be the current server config hash so the desired spec matches
// what reconcileValkeyNode actually applies; omitting it would make every
// settled node (which carries the hash) report a spurious diff and defeat the
// purpose of this pre-flight check.
func anyNodeRequiresRoll(cluster *valkeyiov1alpha1.ValkeyCluster, nodeList *valkeyiov1alpha1.ValkeyNodeList, configHash string) bool {
	byName := make(map[string]*valkeyiov1alpha1.ValkeyNode, len(nodeList.Items))
	for i := range nodeList.Items {
		byName[nodeList.Items[i].Name] = &nodeList.Items[i]
	}
	nodesPerShard := 1 + int(cluster.Spec.Replicas)
	for shardIndex := range int(cluster.Spec.Shards) {
		for nodeIndex := range nodesPerShard {
			desired := buildClusterValkeyNode(cluster, shardIndex, nodeIndex)
			desired.Spec.ServerConfigHash = configHash
			if current, ok := byName[desired.Name]; ok && nodeRequiresRoll(current, desired) {
				return true
			}
		}
	}
	return false
}
