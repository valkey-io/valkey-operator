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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// A shard whose primary is at 10.0.0.1 and whose only replica is not synced.
func unsyncedShardState() *valkey.ClusterState {
	return &valkey.ClusterState{
		Shards: []*valkey.ShardState{{
			Id:        "shard-2",
			PrimaryId: "node-1",
			Nodes: []*valkey.NodeState{
				{Address: "10.0.0.1", Id: "node-1", Flags: []string{"master"}},
				{Address: "10.0.0.2", Id: "node-2", Flags: []string{"slave"}, Info: map[string]string{"master_link_status": "down"}},
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

	cause := r.maybeProactiveFailoverBeforeRoll(context.Background(), cluster, unsyncedShardState(), primary, true)
	assert.Equal(t, "the shard has no synced replica to fail over to", cause, "a primary with no synced replica waits, and says why")

	assert.Empty(t, r.maybeProactiveFailoverBeforeRoll(context.Background(), cluster, unsyncedShardState(), primary, false), "a spec update that does not roll the pod needs no failover")
	replica := &valkeyv1.ValkeyNode{ObjectMeta: metav1.ObjectMeta{Name: "valkey-c-2-1"}, Status: valkeyv1.ValkeyNodeStatus{PodIP: "10.0.0.2"}}
	assert.Empty(t, r.maybeProactiveFailoverBeforeRoll(context.Background(), cluster, unsyncedShardState(), replica, true), "a replica rolls without failover")
	single := &valkeyv1.ValkeyCluster{Spec: valkeyv1.ValkeyClusterSpec{Shards: 3, Replicas: 0}}
	assert.Empty(t, r.maybeProactiveFailoverBeforeRoll(context.Background(), single, unsyncedShardState(), primary, true), "without replicas there is nothing to wait for")
}

func TestMarkRollDeferred(t *testing.T) {
	recorder := events.NewFakeRecorder(10)
	r := &ValkeyClusterReconciler{Recorder: recorder}
	cluster := &valkeyv1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	deferred := &rollDeferredError{shardIndex: 2, node: "valkey-c-2-0", cause: "the shard has no synced replica to fail over to"}

	r.markRollDeferred(cluster, deferred)

	const want = "Roll of shard 2 primary valkey-c-2-0 deferred: the shard has no synced replica to fail over to"
	ready := meta.FindStatusCondition(cluster.Status.Conditions, valkeyv1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, valkeyv1.ReasonRollDeferred, ready.Reason, "a held roll is not the generic UpdatingNodes")
	assert.Equal(t, want, ready.Message)
	progressing := meta.FindStatusCondition(cluster.Status.Conditions, valkeyv1.ConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, metav1.ConditionTrue, progressing.Status)
	assert.Equal(t, valkeyv1.ReasonRollDeferred, progressing.Reason)
	assert.Equal(t, want, progressing.Message)

	select {
	case ev := <-recorder.Events:
		assert.Contains(t, ev, corev1.EventTypeNormal)
		assert.Contains(t, ev, "RollDeferred")
		assert.Contains(t, ev, want)
	default:
		t.Fatal("expected an event for the deferred roll")
	}

	assert.Equal(t, "roll of shard 2 primary valkey-c-2-0 deferred: the shard has no synced replica to fail over to", deferred.Error())
}
