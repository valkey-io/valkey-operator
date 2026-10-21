/*
Copyright 2024.

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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// shardState is a shard whose primary is at 10.0.0.1 with one replica whose
// replication link is in the given state.
func shardState(link string) *valkey.ClusterState {
	return &valkey.ClusterState{
		Shards: []*valkey.ShardState{{
			Id:        "shard-2",
			PrimaryId: "node-1",
			Nodes: []*valkey.NodeState{
				{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master"}},
				{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": link}},
			},
		}},
	}
}

func TestMaybeProactiveFailoverBeforeRollCause(t *testing.T) {
	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(10)}
	cluster := &valkeyv1.ValkeyCluster{Spec: valkeyv1.ValkeyClusterSpec{Shards: 3, Replicas: 1}}
	primary := &valkeyv1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-2-0"},
		Status:     valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.1"},
	}

	cause := r.maybeProactiveFailoverBeforeRoll(context.Background(), cluster, shardState("down"), primary, true)
	assert.Equal(t, "the shard has no synced replica to fail over to", cause, "a primary with no synced replica waits, and says why")

	assert.Empty(t, r.maybeProactiveFailoverBeforeRoll(context.Background(), cluster, shardState("down"), primary, false), "a spec update that does not roll the pod needs no failover")
	replica := &valkeyv1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-2-1"}, Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.2"}}
	assert.Empty(t, r.maybeProactiveFailoverBeforeRoll(context.Background(), cluster, shardState("down"), replica, true), "a replica rolls without failover")
	single := &valkeyv1.ValkeyCluster{Spec: valkeyv1.ValkeyClusterSpec{Shards: 3, Replicas: 0}}
	assert.Empty(t, r.maybeProactiveFailoverBeforeRoll(context.Background(), single, shardState("down"), primary, true), "without replicas there is nothing to wait for")
}

func TestMaybeProactiveFailoverBeforeRollFailoverError(t *testing.T) {
	// A synced replica exists, so the roll tries the failover, and it does not complete.
	orig := proactiveFailoverFn
	proactiveFailoverFn = func(context.Context, events.EventRecorder, *valkeyv1.ValkeyCluster, *valkey.ShardState, []*valkey.NodeState) error {
		return errors.New("CLUSTER FAILOVER timed out")
	}
	t.Cleanup(func() { proactiveFailoverFn = orig })

	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(10)}
	cluster := &valkeyv1.ValkeyCluster{Spec: valkeyv1.ValkeyClusterSpec{Shards: 3, Replicas: 1}}
	primary := &valkeyv1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-2-0"}, Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.1"}}

	cause := r.maybeProactiveFailoverBeforeRoll(context.Background(), cluster, shardState("up"), primary, true)
	assert.Equal(t, "the proactive failover to a synced replica did not complete (CLUSTER FAILOVER timed out)", cause)

	proactiveFailoverFn = func(context.Context, events.EventRecorder, *valkeyv1.ValkeyCluster, *valkey.ShardState, []*valkey.NodeState) error {
		return nil
	}
	assert.Empty(t, r.maybeProactiveFailoverBeforeRoll(context.Background(), cluster, shardState("up"), primary, true), "a completed failover lets the roll proceed")
}

func TestRollDeferredErrorText(t *testing.T) {
	deferred := &rollDeferredError{shardIndex: 2, node: "valkey-c-2-0", cause: "the shard has no synced replica to fail over to"}
	assert.Equal(t, "the roll of shard 2 primary valkey-c-2-0 is waiting, the shard has no synced replica to fail over to", deferred.Error())
}
