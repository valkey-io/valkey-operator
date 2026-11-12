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
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// failbackNodes is one shard (index 0) of two ValkeyNodes: node-index 0 at
// 10.0.0.1 and node-index 1 at 10.0.0.2.
func failbackNodes(node0Ready bool) *valkeyv1.ValkeyNodeList {
	node := func(nodeIndex int, ip string, ready bool) valkeyv1.ValkeyNode {
		return valkeyv1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "valkey-c-0-" + strconv.Itoa(nodeIndex),
				Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: strconv.Itoa(nodeIndex)},
			},
			Status: valkeyv1.ValkeyNodeStatus{PodIP: ip, Ready: ready},
		}
	}
	return &valkeyv1.ValkeyNodeList{Items: []valkeyv1.ValkeyNode{node(0, "10.0.0.1", node0Ready), node(1, "10.0.0.2", true)}}
}

// failbackState is the shard with its primary at primaryIP and the other
// member a replica of it whose replication link is link and whose server has
// been up for an hour.
func failbackState(primaryIP, link string) *valkey.ClusterState {
	return failbackStateFollowing(primaryIP, link, "primary", 3600)
}

// failbackStateFollowing is failbackState with the replica naming follows as
// its primary and reporting uptime seconds.
func failbackStateFollowing(primaryIP, link, follows string, uptime int) *valkey.ClusterState {
	other := map[string]string{"10.0.0.1": "10.0.0.2", "10.0.0.2": "10.0.0.1"}[primaryIP]
	replica := &valkey.NodeState{Address: other, Id: "replica", Flags: []string{"slave"},
		Info: map[string]string{"master_link_status": link, "uptime_in_seconds": strconv.Itoa(uptime)}}
	replica.SetClusterNodesForTesting("replica " + other + ":6379@16379 myself,slave " + follows + " 0 0 1 connected\n")
	return &valkey.ClusterState{
		Shards: []*valkey.ShardState{{
			Id:        "shard-a",
			PrimaryId: "primary",
			Slots:     []valkey.SlotsRange{{Start: 0, End: 16383}},
			Nodes: []*valkey.NodeState{
				{Address: primaryIP, Id: "primary", Flags: []string{"master"}},
				replica,
			},
		}},
	}
}

func failbackCluster(failback *valkeyv1.FailbackSpec) *valkeyv1.ValkeyCluster {
	c := &valkeyv1.ValkeyCluster{Spec: valkeyv1.ValkeyClusterSpec{Shards: 1, Replicas: 1}}
	if failback != nil {
		c.Spec.Failover = &valkeyv1.ClusterFailoverSpec{Failback: failback}
	}
	return c
}

func TestFailbackCandidate(t *testing.T) {
	for _, tc := range []struct {
		name          string
		state         *valkey.ClusterState
		node0Ready    bool
		wantDisplaced bool
		wantTarget    bool
	}{
		{"primary on node-index 0", failbackState("10.0.0.1", "up"), true, false, false},
		{"node-index 0 is a synced replica", failbackState("10.0.0.2", "up"), true, true, true},
		{"node-index 0 is not synced", failbackState("10.0.0.2", "down"), true, true, false},
		{"node-index 0 is not Ready", failbackState("10.0.0.2", "up"), false, true, false},
		{"node-index 0 follows a former primary", failbackStateFollowing("10.0.0.2", "up", "former", 3600), true, true, false},
		{"primary unknown", &valkey.ClusterState{}, true, false, false},
		{"no state", nil, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, target, displaced := failbackCandidate(tc.state, 0, failbackNodes(tc.node0Ready))
			assert.Equal(t, tc.wantDisplaced, displaced)
			if tc.wantTarget {
				require.NotNil(t, target)
				assert.Equal(t, "10.0.0.1", target.Address)
			} else {
				assert.Nil(t, target)
			}
		})
	}
}

func TestTrackFailback(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	nodes := failbackNodes(true)
	reason := func(c *valkeyv1.ValkeyCluster) string {
		cond := meta.FindStatusCondition(c.Status.Conditions, valkeyv1.ConditionPrimaryDisplaced)
		if cond == nil {
			return ""
		}
		return cond.Reason
	}

	c := failbackCluster(nil)
	trackFailback(c, failbackState("10.0.0.2", "down"), nodes, t0)
	require.Len(t, c.Status.Failback, 1)
	assert.Nil(t, c.Status.Failback[0].SyncedSince, "the clock starts only once node-index 0 is synced")
	assert.Equal(t, valkeyv1.ReasonFailbackWaitingForReplica, reason(c))

	trackFailback(c, failbackState("10.0.0.2", "up"), nodes, t0)
	require.NotNil(t, c.Status.Failback[0].SyncedSince)
	assert.Equal(t, t0, c.Status.Failback[0].SyncedSince.Time)
	assert.Equal(t, valkeyv1.ReasonFailbackWaitingForQuietPeriod, reason(c))

	trackFailback(c, failbackState("10.0.0.2", "up"), nodes, t0.Add(time.Minute))
	assert.Equal(t, t0, c.Status.Failback[0].SyncedSince.Time, "a synced member keeps its clock")

	trackFailback(c, failbackStateFollowing("10.0.0.2", "up", "primary", 20), nodes, t0.Add(90*time.Second))
	assert.Equal(t, t0.Add(90*time.Second), c.Status.Failback[0].SyncedSince.Time, "a restart between two reconciles restarts the clock")

	trackFailback(c, failbackState("10.0.0.2", "down"), nodes, t0.Add(2*time.Minute))
	assert.Nil(t, c.Status.Failback[0].SyncedSince, "losing sync resets the clock")

	trackFailback(c, failbackState("10.0.0.2", "up"), nodes, t0.Add(10*time.Minute))
	trackFailback(c, failbackState("10.0.0.2", "up"), nodes, t0.Add(15*time.Minute))
	assert.Equal(t, valkeyv1.ReasonFailbackWaitingForCluster, reason(c), "the quiet period has ended")

	trackFailback(c, &valkey.ClusterState{}, nodes, t0.Add(16*time.Minute))
	require.Len(t, c.Status.Failback, 1, "an unknown primary keeps the entry")
	assert.Nil(t, c.Status.Failback[0].SyncedSince, "and restarts its clock")

	trackFailback(c, failbackState("10.0.0.2", "up"), nodes, t0.Add(16*time.Minute+time.Second))
	assert.Equal(t, valkeyv1.ReasonFailbackWaitingForQuietPeriod, reason(c), "a recovered shard waits a full quiet period again")

	trackFailback(c, failbackState("10.0.0.1", "up"), nodes, t0.Add(17*time.Minute))
	assert.Empty(t, c.Status.Failback, "a primary back on node-index 0 clears the entry")
	assert.Empty(t, reason(c), "and the condition")

	disabled := failbackCluster(&valkeyv1.FailbackSpec{Mode: valkeyv1.FailbackDisabled})
	trackFailback(disabled, failbackState("10.0.0.2", "up"), nodes, t0)
	assert.Equal(t, valkeyv1.ReasonFailbackDisabled, reason(disabled))

	scaledIn := failbackCluster(nil)
	scaledIn.Status.Failback = []valkeyv1.ShardFailbackStatus{{Shard: 3}}
	trackFailback(scaledIn, nil, nodes, t0)
	assert.Empty(t, scaledIn.Status.Failback, "a shard removed by a scale-in is dropped")
}

func TestMaybeFailback(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	origNow, origFn := nowFn, failbackFn
	t.Cleanup(func() { nowFn, failbackFn = origNow, origFn })
	var targets []string
	var failErr error
	failbackFn = func(_ context.Context, _ events.EventRecorder, _ *valkeyv1.ValkeyCluster, _ *valkey.ShardState, target *valkey.NodeState) error {
		targets = append(targets, target.Address)
		return failErr
	}
	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(10)}
	nodes := failbackNodes(true)
	state := failbackState("10.0.0.2", "up")
	run := func(c *valkeyv1.ValkeyCluster, at time.Time, afterRollOnly bool) bool {
		targets = nil
		nowFn = func() time.Time { return at }
		return r.maybeFailback(context.Background(), c, state, nodes, 0, afterRollOnly)
	}
	synced := func(c *valkeyv1.ValkeyCluster, afterRoll bool) *valkeyv1.ValkeyCluster {
		c.Status.Failback = []valkeyv1.ShardFailbackStatus{{Shard: 0, SyncedSince: &metav1.Time{Time: t0}, AfterRoll: afterRoll}}
		return c
	}

	t.Run("waits for the quiet period", func(t *testing.T) {
		c := synced(failbackCluster(&valkeyv1.FailbackSpec{QuietPeriodSeconds: new(int32(60))}), false)
		assert.False(t, run(c, t0.Add(59*time.Second), false))
		assert.Empty(t, targets)
		assert.True(t, run(c, t0.Add(60*time.Second), false))
		assert.Equal(t, []string{"10.0.0.1"}, targets)
		assert.Empty(t, c.Status.Failback, "a completed failback clears the entry")
	})

	t.Run("after a roll it does not wait", func(t *testing.T) {
		c := synced(failbackCluster(nil), true)
		assert.True(t, run(c, t0, true))
		assert.Equal(t, []string{"10.0.0.1"}, targets)
	})

	t.Run("the roll walk leaves other failovers to the quiet period", func(t *testing.T) {
		c := synced(failbackCluster(&valkeyv1.FailbackSpec{QuietPeriodSeconds: new(int32(10))}), false)
		assert.False(t, run(c, t0.Add(10*time.Second), true))
		assert.True(t, run(c, t0.Add(10*time.Second), false))
	})

	t.Run("disabled", func(t *testing.T) {
		c := synced(failbackCluster(&valkeyv1.FailbackSpec{Mode: valkeyv1.FailbackDisabled}), true)
		assert.False(t, run(c, t0.Add(time.Hour), false))
		assert.Empty(t, targets)
	})

	t.Run("a failed attempt backs off for a quiet period", func(t *testing.T) {
		failErr = errors.New("timed out")
		t.Cleanup(func() { failErr = nil })
		c := synced(failbackCluster(&valkeyv1.FailbackSpec{QuietPeriodSeconds: new(int32(60))}), true)
		assert.True(t, run(c, t0.Add(time.Hour), false))
		require.Len(t, c.Status.Failback, 1)
		assert.False(t, c.Status.Failback[0].AfterRoll)
		assert.Equal(t, t0.Add(time.Hour), c.Status.Failback[0].SyncedSince.Time)
		assert.False(t, run(c, t0.Add(time.Hour+59*time.Second), false))
	})
}

func TestMarkRollHandoff(t *testing.T) {
	origFn := proactiveFailoverFn
	t.Cleanup(func() { proactiveFailoverFn = origFn })
	proactiveFailoverFn = func(context.Context, events.EventRecorder, *valkeyv1.ValkeyCluster, *valkey.ShardState, []*valkey.NodeState) error {
		return nil
	}
	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(10)}
	c := failbackCluster(nil)
	node0 := failbackNodes(true).Items[0]
	assert.Empty(t, r.maybeProactiveFailoverBeforeRoll(context.Background(), c, failbackState("10.0.0.1", "up"), &node0, true))
	assert.Equal(t, []valkeyv1.ShardFailbackStatus{{Shard: 0, AfterRoll: true}}, c.Status.Failback)
}
