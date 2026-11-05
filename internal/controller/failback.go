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
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// nowFn is the clock failback reads; tests replace it.
var nowFn = time.Now

// failbackCandidate looks at shard shardIndex. displaced is true when its
// primary is known and is not the node-index 0 member. shard is the live
// shard and target the node-index 0 member when that member is a Ready
// ValkeyNode and a synced replica of the primary; target is nil otherwise.
func failbackCandidate(state *valkey.ClusterState, shardIndex int, nodes *valkeyiov1alpha1.ValkeyNodeList) (shard *valkey.ShardState, target *valkey.NodeState, displaced bool) {
	if state == nil {
		return nil, nil, false
	}
	_, primaryIP := findShardPrimary(state, shardIndex, nodes)
	if primaryIP == "" {
		return nil, nil, false
	}
	si := strconv.Itoa(shardIndex)
	idx := slices.IndexFunc(nodes.Items, func(n valkeyiov1alpha1.ValkeyNode) bool {
		return n.Labels[LabelShardIndex] == si && n.Labels[LabelNodeIndex] == "0"
	})
	if idx >= 0 && nodes.Items[idx].Status.PodIP == primaryIP {
		return nil, nil, false
	}
	shard = state.FindShardForAddress(primaryIP)
	if idx < 0 || shard == nil {
		return shard, nil, true
	}
	node0 := &nodes.Items[idx]
	if !node0.Status.Ready || node0.Status.PodIP == "" {
		return shard, nil, true
	}
	for _, replica := range shard.GetSyncedReplicas(state) {
		if replica.Address == node0.Status.PodIP {
			return shard, replica, true
		}
	}
	return shard, nil, true
}

func findFailbackStatus(cluster *valkeyiov1alpha1.ValkeyCluster, shardIndex int) *valkeyiov1alpha1.ShardFailbackStatus {
	for i := range cluster.Status.Failback {
		if int(cluster.Status.Failback[i].Shard) == shardIndex {
			return &cluster.Status.Failback[i]
		}
	}
	return nil
}

func removeFailbackStatus(cluster *valkeyiov1alpha1.ValkeyCluster, shardIndex int) {
	cluster.Status.Failback = slices.DeleteFunc(cluster.Status.Failback, func(f valkeyiov1alpha1.ShardFailbackStatus) bool {
		return int(f.Shard) == shardIndex
	})
}

// markRollHandoff records that the operator moved shardIndex's primary away to
// roll a pod, so the shard fails back without the quiet period.
func markRollHandoff(cluster *valkeyiov1alpha1.ValkeyCluster, shardIndex int) {
	if f := findFailbackStatus(cluster, shardIndex); f != nil {
		f.AfterRoll = true
		return
	}
	cluster.Status.Failback = append(cluster.Status.Failback, valkeyiov1alpha1.ShardFailbackStatus{Shard: int32(shardIndex), AfterRoll: true})
	slices.SortFunc(cluster.Status.Failback, func(a, b valkeyiov1alpha1.ShardFailbackStatus) int { return int(a.Shard - b.Shard) })
}

// refreshFailbackStatus replaces status.failback with the stored copy. The
// cluster comes from the informer cache, which can lag behind the status the
// previous reconcile wrote; status.failback is carried from one reconcile to
// the next, so a stale copy written back would drop a roll handoff or restart
// a quiet period. On a read error the cached copy is kept.
func (r *ValkeyClusterReconciler) refreshFailbackStatus(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster) {
	if r.APIReader == nil {
		return
	}
	stored := &valkeyiov1alpha1.ValkeyCluster{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(cluster), stored); err != nil {
		logf.FromContext(ctx).V(1).Info("could not read stored failback status, using the cached copy", "err", err)
		return
	}
	cluster.Status.Failback = stored.Status.Failback
}

// failbackDue reports whether f's shard may fail back now, and otherwise when
// the quiet period ends (zero while there is nothing to wait for).
func failbackDue(cluster *valkeyiov1alpha1.ValkeyCluster, f *valkeyiov1alpha1.ShardFailbackStatus, now time.Time) (bool, time.Time) {
	if f.SyncedSince == nil {
		return false, time.Time{}
	}
	if f.AfterRoll {
		return true, time.Time{}
	}
	due := f.SyncedSince.Add(time.Duration(cluster.Spec.FailbackQuietPeriodSeconds()) * time.Second)
	if now.Before(due) {
		return false, due
	}
	return true, time.Time{}
}

// trackFailback refreshes status.failback from the live state: it adds the
// shards whose primary moved away from node-index 0, drops the ones that are
// back, and starts or clears each shard's quiet period clock. Shards whose
// primary is unknown keep their entry as it is. It then sets the
// PrimaryDisplaced condition.
func trackFailback(cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, nodes *valkeyiov1alpha1.ValkeyNodeList, now time.Time) {
	if state != nil {
		for shardIndex := range int(cluster.Spec.Shards) {
			if _, ip := findShardPrimary(state, shardIndex, nodes); ip == "" {
				continue
			}
			_, target, displaced := failbackCandidate(state, shardIndex, nodes)
			if !displaced {
				removeFailbackStatus(cluster, shardIndex)
				continue
			}
			f := findFailbackStatus(cluster, shardIndex)
			if f == nil {
				cluster.Status.Failback = append(cluster.Status.Failback, valkeyiov1alpha1.ShardFailbackStatus{Shard: int32(shardIndex)})
				f = &cluster.Status.Failback[len(cluster.Status.Failback)-1]
			}
			switch {
			case target == nil:
				f.SyncedSince = nil
			case f.SyncedSince == nil:
				f.SyncedSince = &metav1.Time{Time: now}
			}
		}
	}
	// Shards removed by a scale-in have nothing left to fail back.
	cluster.Status.Failback = slices.DeleteFunc(cluster.Status.Failback, func(f valkeyiov1alpha1.ShardFailbackStatus) bool {
		return f.Shard >= cluster.Spec.Shards
	})
	slices.SortFunc(cluster.Status.Failback, func(a, b valkeyiov1alpha1.ShardFailbackStatus) int { return int(a.Shard - b.Shard) })

	if len(cluster.Status.Failback) == 0 {
		meta.RemoveStatusCondition(&cluster.Status.Conditions, valkeyiov1alpha1.ConditionPrimaryDisplaced)
		return
	}
	f := &cluster.Status.Failback[0]
	what := fmt.Sprintf("The primary of shard %d is not its node-index 0 member", f.Shard)
	if n := len(cluster.Status.Failback); n > 1 {
		what = fmt.Sprintf("The primaries of %d shards, starting with shard %d, are not their node-index 0 member", n, f.Shard)
	}
	reason, message := valkeyiov1alpha1.ReasonFailbackWaitingForCluster, what+": failback waits for the cluster to be healthy"
	switch due, at := failbackDue(cluster, f, now); {
	case !cluster.Spec.FailbackEnabled():
		reason, message = valkeyiov1alpha1.ReasonFailbackDisabled, what+" and failback is disabled"
	case f.SyncedSince == nil:
		reason, message = valkeyiov1alpha1.ReasonFailbackWaitingForReplica, what+": failback waits for that member to be a Ready replica in sync with the primary"
	case !due:
		reason, message = valkeyiov1alpha1.ReasonFailbackWaitingForQuietPeriod, what+": failback waits for the quiet period to end at "+at.UTC().Format(time.RFC3339)
	}
	setCondition(cluster, valkeyiov1alpha1.ConditionPrimaryDisplaced, reason, message, metav1.ConditionTrue)
}

// maybeFailback fails shard shardIndex back to its node-index 0 member when
// its status.failback entry is due. With afterRollOnly it only acts on a shard
// the operator moved itself to roll a pod. It reports whether it sent a
// failover, successful or not, so the caller re-reads the topology before
// going on.
func (r *ValkeyClusterReconciler) maybeFailback(ctx context.Context, cluster *valkeyiov1alpha1.ValkeyCluster, state *valkey.ClusterState, nodes *valkeyiov1alpha1.ValkeyNodeList, shardIndex int, afterRollOnly bool) bool {
	if !cluster.Spec.FailbackEnabled() {
		return false
	}
	f := findFailbackStatus(cluster, shardIndex)
	if f == nil || (afterRollOnly && !f.AfterRoll) {
		return false
	}
	now := nowFn()
	if due, _ := failbackDue(cluster, f, now); !due {
		return false
	}
	shard, target, displaced := failbackCandidate(state, shardIndex, nodes)
	if !displaced || target == nil {
		return false
	}
	log := logf.FromContext(ctx)
	log.Info("failing shard back to its node-index 0 member", "shardIndex", shardIndex, "target", target.Address, "afterRoll", f.AfterRoll)
	if err := failbackFn(ctx, r.Recorder, cluster, shard, target); err != nil {
		// Back off: the next attempt waits for a full quiet period.
		log.Info("failback did not complete", "shardIndex", shardIndex, "err", err)
		f.AfterRoll = false
		f.SyncedSince = &metav1.Time{Time: now}
		return true
	}
	removeFailbackStatus(cluster, shardIndex)
	return true
}

// failbackRequeueAfter returns how long to wait before the earliest quiet
// period ends, capped at limit.
func failbackRequeueAfter(cluster *valkeyiov1alpha1.ValkeyCluster, limit time.Duration) time.Duration {
	if !cluster.Spec.FailbackEnabled() {
		return limit
	}
	now := nowFn()
	for i := range cluster.Status.Failback {
		if _, at := failbackDue(cluster, &cluster.Status.Failback[i], now); !at.IsZero() && at.Sub(now) < limit {
			limit = at.Sub(now) + time.Second
		}
	}
	return limit
}
