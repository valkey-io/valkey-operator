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
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	vclient "github.com/valkey-io/valkey-go"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// orphanTestNode builds a scraped node of a 3-primary cluster. lines is its own
// CLUSTER NODES view; the first line is its myself entry.
func orphanTestNode(id string, lines ...string) *valkey.NodeState {
	n := &valkey.NodeState{Id: id, Address: "10.0.0." + id, ClusterInfo: map[string]string{"cluster_size": "3"}}
	view := ""
	for _, line := range lines {
		view += line + "\n"
	}
	n.SetClusterNodesForTesting(view)
	return n
}

// TestOrphansToTakeOver covers which dead primaries' replicas are promoted with
// TAKEOVER, with and without failover quorum.
func TestOrphansToTakeOver(t *testing.T) {
	// The flags on the orphaned replica's own CLUSTER NODES entry. Valkey adds
	// nofailover when native failover is disabled on the node.
	const failoverEnabled, failoverDisabled = "myself,slave", "myself,slave,nofailover"

	// Shards 0 and 1 lost their primaries; shard 2's primary reports both failed.
	quorumLost := func() *valkey.ClusterState {
		return &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s0", Nodes: []*valkey.NodeState{orphanTestNode("r0", "r0 10.0.0.r0:6379@16379 myself,slave p0 0 0 1 connected")}},
			{Id: "s1", Nodes: []*valkey.NodeState{orphanTestNode("r1", "r1 10.0.0.r1:6379@16379 myself,slave p1 0 0 1 connected")}},
			{Id: "s2", PrimaryId: "p2", Slots: []valkey.SlotsRange{{Start: 10923, End: 16383}}, Nodes: []*valkey.NodeState{
				orphanTestNode("p2", "p2 10.0.0.p2:6379@16379 myself,master - 0 0 1 connected 10923-16383",
					"p0 10.0.0.p0:6379@16379 master,fail? - 0 0 1 connected 0-5460",
					"p1 10.0.0.p1:6379@16379 master,fail? - 0 0 1 connected 5461-10922"),
			}},
		}}
	}

	// Only shard 0 lost its primary; shards 1 and 2 hold quorum. failFlag is how
	// the two live primaries see p0, replicaFlags how r0 sees itself.
	quorumHeld := func(failFlag, replicaFlags string) *valkey.ClusterState {
		p0 := "p0 10.0.0.p0:6379@16379 master," + failFlag + " - 0 0 1 connected 0-5460"
		return &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s0", Nodes: []*valkey.NodeState{orphanTestNode("r0", "r0 10.0.0.r0:6379@16379 "+replicaFlags+" p0 0 0 1 connected")}},
			{Id: "s1", PrimaryId: "p1", Slots: []valkey.SlotsRange{{Start: 5461, End: 10922}}, Nodes: []*valkey.NodeState{
				orphanTestNode("p1", "p1 10.0.0.p1:6379@16379 myself,master - 0 0 1 connected 5461-10922", p0),
			}},
			{Id: "s2", PrimaryId: "p2", Slots: []valkey.SlotsRange{{Start: 10923, End: 16383}}, Nodes: []*valkey.NodeState{
				orphanTestNode("p2", "p2 10.0.0.p2:6379@16379 myself,master - 0 0 1 connected 10923-16383", p0),
			}},
		}}
	}

	// pairs lists each candidate as "primary/replica", in order.
	pairs := func(orphans []orphan) []string {
		out := make([]string, 0, len(orphans))
		for _, o := range orphans {
			out = append(out, o.primaryId+"/"+o.replica.Id)
		}
		return out
	}

	t.Run("without quorum, lists every dead primary in a stable order", func(t *testing.T) {
		// Every candidate is listed, so a replica whose TAKEOVER fails does not
		// hold back the others.
		assert.Equal(t, []string{"p0/r0", "p1/r1"}, pairs(orphansToTakeOver(quorumLost())))
	})

	t.Run("with quorum, leaves the shard to native failover", func(t *testing.T) {
		assert.Empty(t, orphansToTakeOver(quorumHeld("fail", failoverEnabled)))
	})

	t.Run("with quorum, takes over when the replica has failover disabled", func(t *testing.T) {
		assert.Equal(t, []string{"p0/r0"}, pairs(orphansToTakeOver(quorumHeld("fail", failoverDisabled))))
	})

	t.Run("with quorum, reads nofailover from the replica's own entry", func(t *testing.T) {
		// p1 sees r0 as nofailover, but r0 itself does not report it, so
		// native failover can still promote r0.
		state := quorumHeld("fail", failoverEnabled)
		state.Shards[1].Nodes[0] = orphanTestNode("p1", "p1 10.0.0.p1:6379@16379 myself,master - 0 0 1 connected 5461-10922",
			"p0 10.0.0.p0:6379@16379 master,fail - 0 0 1 connected 0-5460",
			"r0 10.0.0.r0:6379@16379 slave,nofailover p0 0 0 1 connected")
		assert.Empty(t, orphansToTakeOver(state))
	})

	t.Run("with quorum, waits for a confirmed failure before taking over", func(t *testing.T) {
		// Only one of the two live primaries suspects p0, short of the
		// size/2+1 that Valkey needs to mark it failed.
		state := quorumHeld("fail", failoverDisabled)
		state.Shards[2].Nodes[0] = orphanTestNode("p2", "p2 10.0.0.p2:6379@16379 myself,master - 0 0 1 connected 10923-16383")
		state.Shards[1].Nodes[0] = orphanTestNode("p1", "p1 10.0.0.p1:6379@16379 myself,master - 0 0 1 connected 5461-10922",
			"p0 10.0.0.p0:6379@16379 master,fail? - 0 0 1 connected 0-5460")
		assert.Empty(t, orphansToTakeOver(state))
	})

	t.Run("never takes over a primary the operator reached", func(t *testing.T) {
		// p0 answered the scrape, here as a slotless pending node so the
		// served-shard rule does not apply; its replica's stale flag must not
		// trigger a takeover, even though quorum is short.
		state := quorumLost()
		state.Shards[0] = &valkey.ShardState{Id: "s0", Nodes: []*valkey.NodeState{
			orphanTestNode("r0", "r0 10.0.0.r0:6379@16379 myself,slave p0 0 0 1 connected",
				"p0 10.0.0.p0:6379@16379 master,fail? - 0 0 1 connected"),
		}}
		state.Shards = state.Shards[:1]
		state.PendingNodes = []*valkey.NodeState{
			orphanTestNode("p0", "p0 10.0.0.p0:6379@16379 myself,master - 0 0 1 connected"),
		}
		assert.Empty(t, orphansToTakeOver(state))
	})

	t.Run("skips a shard already served by a promoted replica", func(t *testing.T) {
		// r0 took over shard 0 on an earlier reconcile; its sibling r0b has not
		// learned that yet and still names the dead p0. Only shard 1 is left.
		state := quorumLost()
		state.Shards[0] = &valkey.ShardState{Id: "s0", PrimaryId: "r0", Slots: []valkey.SlotsRange{{Start: 0, End: 5460}}, Nodes: []*valkey.NodeState{
			orphanTestNode("r0", "r0 10.0.0.r0:6379@16379 myself,master - 0 0 2 connected 0-5460",
				"p0 10.0.0.p0:6379@16379 master,fail - 0 0 1 connected",
				"p1 10.0.0.p1:6379@16379 master,fail? - 0 0 1 connected 5461-10922"),
			orphanTestNode("r0b", "r0b 10.0.0.r0b:6379@16379 myself,slave p0 0 0 1 connected"),
		}}
		state.Shards = state.Shards[:2]
		assert.Equal(t, []string{"p1/r1"}, pairs(orphansToTakeOver(state)))
	})

	t.Run("does nothing when no primary is reported failed", func(t *testing.T) {
		state := quorumHeld("fail", failoverDisabled)
		state.Shards[1].Nodes[0] = orphanTestNode("p1", "p1 10.0.0.p1:6379@16379 myself,master - 0 0 1 connected 5461-10922")
		state.Shards[2].Nodes[0] = orphanTestNode("p2", "p2 10.0.0.p2:6379@16379 myself,master - 0 0 1 connected 10923-16383")
		assert.Empty(t, orphansToTakeOver(state))
	})
}

// takeoverClient counts CLUSTER FAILOVER TAKEOVER commands and answers OK, or
// with err when it is set.
type takeoverClient struct {
	vclient.Client
	takeovers int
	err       error
}

// B returns a builder; the zero value builds commands without a connection.
func (c *takeoverClient) B() vclient.Builder { return vclient.Builder{} }

func (c *takeoverClient) Do(_ context.Context, cmd vclient.Completed) vclient.ValkeyResult {
	if strings.Join(cmd.Commands(), " ") == "CLUSTER FAILOVER TAKEOVER" {
		c.takeovers++
	}
	if c.err != nil {
		return errorResult(c.err)
	}
	return vclient.ValkeyResult{}
}

// errorResult returns a ValkeyResult holding err. valkey-go has no exported
// constructor for one; this is what valkey-go/mock.ErrorResult does.
func errorResult(err error) vclient.ValkeyResult {
	r := struct {
		err error
		val vclient.ValkeyMessage
	}{err: err}
	return *(*vclient.ValkeyResult)(unsafe.Pointer(&r))
}

// orphanSetup is one shard of cluster ns/c whose primary p1 is FAIL, with no
// quorum, so only the operator can promote the replica r1 (10.0.0.2, shard 0).
type orphanSetup struct {
	persistent  bool
	primaryUp   bool          // p1 is not flagged FAIL
	failedFor   time.Duration // since p1 was first seen FAIL; 0 if never
	timerUID    types.UID     // UID the recorded FAIL time belongs to; the cluster's if empty
	grace       time.Duration // OrphanTakeoverGrace; the default if zero
	takeoverErr error
	scraped     []*valkey.NodeState // scraped nodes outside the shard
	pods        []corev1.Pod
}

type orphanOutcome struct {
	takeovers int
	handled   bool
	requeue   time.Duration
	timers    *orphanTimers
}

func (o orphanSetup) run(t *testing.T) orphanOutcome {
	t.Helper()
	flags := "master,fail"
	if o.primaryUp {
		flags = "master"
	}
	vc := &takeoverClient{err: o.takeoverErr}
	replica := &valkey.NodeState{Id: "r1", Address: "10.0.0.2", Client: vc, ClusterInfo: map[string]string{"cluster_size": "1"}}
	replica.SetClusterNodesForTesting("p1 10.0.0.1:6379@16379 " + flags + " - 0 0 1 connected 0-16383\n" +
		"r1 10.0.0.2:6379@16379 myself,slave p1 0 0 1 connected\n")
	state := &valkey.ClusterState{
		Shards:       []*valkey.ShardState{{PrimaryId: "p1", Nodes: []*valkey.NodeState{replica}}},
		PendingNodes: o.scraped,
	}

	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "uid"}}
	if o.persistent {
		cluster.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{}
	}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{{
		ObjectMeta: metav1.ObjectMeta{Name: "c-0-1", Namespace: "ns", Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "1"}},
		Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.0.2"},
	}}}

	scheme := runtime.NewScheme()
	assert.NoError(t, clientgoscheme.AddToScheme(scheme))
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for i := range o.pods {
		builder = builder.WithObjects(&o.pods[i])
	}
	r := &ValkeyClusterReconciler{Client: builder.Build(), Recorder: events.NewFakeRecorder(8), OrphanTakeoverGrace: o.grace}
	key := client.ObjectKeyFromObject(cluster)
	if o.failedFor > 0 {
		uid := o.timerUID
		if uid == "" {
			uid = cluster.UID
		}
		r.orphanTimers = map[client.ObjectKey]*orphanTimers{
			key: {uid: uid, failedSince: map[string]time.Time{"p1": time.Now().Add(-o.failedFor)}},
		}
	}

	result, handled := r.promoteOrphanedReplicas(context.Background(), cluster, state, nodes)
	return orphanOutcome{takeovers: vc.takeovers, handled: handled, requeue: result.RequeueAfter, timers: r.orphanTimers[key]}
}

func shardPod(name, shard string, phase corev1.PodPhase, ready bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{LabelCluster: "c", LabelShardIndex: shard}},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

func TestPromoteOrphanedReplicas(t *testing.T) {
	loadingPrimary := &valkey.NodeState{Id: "p1", Address: "10.0.0.9", Info: map[string]string{"loading": "1"}}
	loadingOther := &valkey.NodeState{Id: "x1", Address: "10.0.0.3", Info: map[string]string{"loading": "1"}}

	tests := []struct {
		name      string
		setup     orphanSetup
		takeover  bool
		requeue   time.Duration // when no takeover
		keepTimer bool
	}{
		{name: "no persistence", setup: orphanSetup{}, takeover: true},
		{name: "first FAIL", setup: orphanSetup{persistent: true}, requeue: time.Minute, keepTimer: true},
		{name: "within the grace", setup: orphanSetup{persistent: true, failedFor: 45 * time.Second}, requeue: 15 * time.Second, keepTimer: true},
		{name: "longer grace set", setup: orphanSetup{persistent: true, failedFor: 2 * time.Minute, grace: 5 * time.Minute}, requeue: 3 * time.Minute, keepTimer: true},
		{name: "grace over", setup: orphanSetup{persistent: true, failedFor: 2 * time.Minute}, takeover: true},
		{name: "TAKEOVER fails", setup: orphanSetup{persistent: true, failedFor: 2 * time.Minute, takeoverErr: errors.New("boom")}, takeover: true, keepTimer: true},
		{name: "primary recovered", setup: orphanSetup{persistent: true, primaryUp: true, failedFor: 2 * time.Minute}},
		// A primary that answers the scrape, here while loading its dataset,
		// is alive however long it has been flagged.
		{name: "primary reachable", setup: orphanSetup{persistent: true, failedFor: time.Hour, scraped: []*valkey.NodeState{loadingPrimary}}},
		{name: "another node reachable", setup: orphanSetup{persistent: true, failedFor: 2 * time.Minute, scraped: []*valkey.NodeState{loadingOther}}, takeover: true},
		{name: "shard pod pending", setup: orphanSetup{persistent: true, failedFor: 2 * time.Minute, pods: []corev1.Pod{
			shardPod("c-0-0", "0", corev1.PodPending, false), shardPod("c-0-1", "0", corev1.PodRunning, true),
		}}, requeue: 3 * time.Minute, keepTimer: true},
		{name: "shard pod running but not ready", setup: orphanSetup{persistent: true, failedFor: 2 * time.Minute, pods: []corev1.Pod{
			shardPod("c-0-0", "0", corev1.PodRunning, false),
		}}, requeue: 3 * time.Minute, keepTimer: true},
		{name: "shard pod pending past the max wait", setup: orphanSetup{persistent: true, failedFor: 6 * time.Minute, pods: []corev1.Pod{
			shardPod("c-0-0", "0", corev1.PodPending, false),
		}}, takeover: true},
		{name: "other shard's pod pending", setup: orphanSetup{persistent: true, failedFor: 2 * time.Minute, pods: []corev1.Pod{
			shardPod("c-1-0", "1", corev1.PodPending, false),
		}}, takeover: true},
		// Same name, new UID: retained PVCs can bring back p1's node ID.
		{name: "cluster recreated", setup: orphanSetup{persistent: true, failedFor: time.Hour, timerUID: "old-uid"}, requeue: time.Minute, keepTimer: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.setup.run(t)
			if tt.takeover {
				assert.Equal(t, 1, got.takeovers)
				assert.True(t, got.handled)
			} else {
				assert.Zero(t, got.takeovers)
				assert.False(t, got.handled)
				assert.InDelta(t, tt.requeue, got.requeue, float64(time.Second))
			}
			if tt.keepTimer {
				assert.Contains(t, got.timers.failedSince, "p1")
				assert.Equal(t, types.UID("uid"), got.timers.uid)
			} else if !tt.takeover && got.timers != nil {
				// After a takeover the next reconcile drops it, once the
				// shard is served again.
				assert.NotContains(t, got.timers.failedSince, "p1")
			}
		})
	}
}

// A cluster that regains quorum drops its FAIL times, so the next loss of
// quorum starts a fresh grace period.
func TestOrphanTimersClearedWhenQuorumReturns(t *testing.T) {
	primary := &valkey.NodeState{Id: "p1", Address: "10.0.0.1", ClusterInfo: map[string]string{"cluster_size": "1"}}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{{
		PrimaryId: "p1", Slots: []valkey.SlotsRange{{Start: 0, End: 16383}}, Nodes: []*valkey.NodeState{primary},
	}}}
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{}
	r := &ValkeyClusterReconciler{orphanTimers: map[client.ObjectKey]*orphanTimers{
		{}: {failedSince: map[string]time.Time{"p1": time.Now().Add(-time.Hour)}},
	}}

	_, handled := r.promoteOrphanedReplicas(context.Background(), cluster, state, &valkeyiov1alpha1.ValkeyNodeList{})
	assert.False(t, handled)
	assert.NotContains(t, r.orphanTimers, client.ObjectKey{})
}

// A cluster's FAIL times are dropped once it is being deleted or gone, and
// other clusters keep theirs.
func TestOrphanTimersClearedOnClusterDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	gone := client.ObjectKey{Namespace: "ns", Name: "gone"}
	deleting := client.ObjectKey{Namespace: "ns", Name: "deleting"}
	other := client.ObjectKey{Namespace: "ns", Name: "other"}
	now := metav1.Now()
	inDeletion := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{
		Name: deleting.Name, Namespace: deleting.Namespace, DeletionTimestamp: &now, Finalizers: []string{"test"},
	}}
	timers := func() *orphanTimers { return &orphanTimers{failedSince: map[string]time.Time{"p1": time.Now()}} }
	r := &ValkeyClusterReconciler{
		Client:       fake.NewClientBuilder().WithScheme(scheme).WithObjects(inDeletion).Build(),
		orphanTimers: map[client.ObjectKey]*orphanTimers{gone: timers(), deleting: timers(), other: timers()},
	}

	for _, key := range []client.ObjectKey{gone, deleting} {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		assert.NoError(t, err)
		assert.NotContains(t, r.orphanTimers, key)
	}
	assert.Contains(t, r.orphanTimers, other)
}
