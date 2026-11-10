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

	"github.com/stretchr/testify/assert"
	vclient "github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/mock"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
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
	takeovers  int
	commands   []string
	err        error
	beforeCall func()
}

// B returns a builder; the zero value builds commands without a connection.
func (c *takeoverClient) B() vclient.Builder { return vclient.Builder{} }

func (c *takeoverClient) Do(_ context.Context, cmd vclient.Completed) vclient.ValkeyResult {
	c.commands = append(c.commands, strings.Join(cmd.Commands(), " "))
	if strings.Join(cmd.Commands(), " ") == "CLUSTER FAILOVER TAKEOVER" {
		c.takeovers++
		if c.beforeCall != nil {
			c.beforeCall()
		}
	}
	if c.err != nil {
		return mock.ErrorResult(c.err)
	}
	return vclient.ValkeyResult{}
}

type orphanTakeoverFixture struct {
	reconciler *ValkeyClusterReconciler
	cluster    *valkeyiov1alpha1.ValkeyCluster
	state      *valkey.ClusterState
	nodes      *valkeyiov1alpha1.ValkeyNodeList
	candidates []orphan
	client     *takeoverClient
}

// newOrphanTakeoverFixture builds a one-shard cluster with an unreachable primary.
func newOrphanTakeoverFixture(t *testing.T, persistent bool, annotation string, takeoverErr error) *orphanTakeoverFixture {
	t.Helper()
	vc := &takeoverClient{err: takeoverErr}
	replica := &valkey.NodeState{Id: "r1", Address: "10.0.0.2", Client: vc, ClusterInfo: map[string]string{"cluster_size": "1"}}
	replica.SetClusterNodesForTesting("p1 10.0.0.1:6379@16379 master,fail - 0 0 1 connected 0-16383\n" +
		"r1 10.0.0.2:6379@16379 myself,slave p1 0 0 1 connected\n")
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{{PrimaryId: "p1", Nodes: []*valkey.NodeState{replica}}}}
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "c", Namespace: "ns", UID: "uid", Annotations: map[string]string{},
	}}
	if persistent {
		cluster.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{}
	}
	if annotation != "" {
		cluster.Annotations[forceTakeoverAnnotation] = annotation
	}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{{
		ObjectMeta: metav1.ObjectMeta{Name: "c-0-1", Namespace: "ns", Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "1"}},
		Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.0.2"},
	}}}
	scheme := runtime.NewScheme()
	assert.NoError(t, clientgoscheme.AddToScheme(scheme))
	assert.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).WithStatusSubresource(cluster).Build()
	r := &ValkeyClusterReconciler{Client: cl, Recorder: events.NewFakeRecorder(8)}
	return &orphanTakeoverFixture{
		reconciler: r,
		cluster:    cluster,
		state:      state,
		nodes:      nodes,
		candidates: orphansToTakeOver(state),
		client:     vc,
	}
}

func TestPromoteOrphanedReplicas(t *testing.T) {

	t.Run("persistent cluster waits for explicit authorization", func(t *testing.T) {
		f := newOrphanTakeoverFixture(t, true, "", nil)
		result, handled := f.reconciler.promoteOrphanedReplicas(context.Background(), f.cluster, f.state, f.nodes, f.candidates)
		assert.True(t, handled)
		assert.Zero(t, f.client.takeovers)
		assert.Equal(t, 30*time.Second, result.RequeueAfter)
		condition := meta.FindStatusCondition(f.cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
		if assert.NotNil(t, condition) {
			assert.Equal(t, valkeyiov1alpha1.ReasonAwaitingManualTakeover, condition.Reason)
			assert.Contains(t, condition.Message, "shard 0")
			assert.Contains(t, condition.Message, "primary p1")
			assert.Contains(t, condition.Message, "replica r1")
			assert.Contains(t, condition.Message, forceTakeoverAnnotation+"=p1")
		}
		ready := meta.FindStatusCondition(f.cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
		if assert.NotNil(t, ready) {
			assert.Equal(t, metav1.ConditionFalse, ready.Status)
			assert.Equal(t, valkeyiov1alpha1.ReasonAwaitingManualTakeover, ready.Reason)
		}
	})

	t.Run("one-shot annotation authorizes takeover and is cleared", func(t *testing.T) {
		f := newOrphanTakeoverFixture(t, true, " stale, p1 , later ", nil)
		f.client.beforeCall = func() {
			stored := &valkeyiov1alpha1.ValkeyCluster{}
			assert.NoError(t, f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.cluster), stored))
			assert.Equal(t, " stale, p1 , later ", stored.Annotations[forceTakeoverAnnotation], "keep approval until TAKEOVER succeeds")
			assert.Equal(t, "p1/r1", stored.Annotations[forceTakeoverAttemptAnnotation], "pin the attempt to its selected replica")
		}
		_, handled := f.reconciler.promoteOrphanedReplicas(context.Background(), f.cluster, f.state, f.nodes, f.candidates)
		assert.True(t, handled)
		assert.Equal(t, 1, f.client.takeovers)
		stored := &valkeyiov1alpha1.ValkeyCluster{}
		assert.NoError(t, f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.cluster), stored))
		assert.Equal(t, "later,stale", stored.Annotations[forceTakeoverAnnotation])
	})

	t.Run("stale annotation does not authorize a different orphan", func(t *testing.T) {
		f := newOrphanTakeoverFixture(t, true, "stale", nil)
		_, handled := f.reconciler.promoteOrphanedReplicas(context.Background(), f.cluster, f.state, f.nodes, f.candidates)
		assert.True(t, handled)
		assert.Zero(t, f.client.takeovers)
		stored := &valkeyiov1alpha1.ValkeyCluster{}
		assert.NoError(t, f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.cluster), stored))
		assert.Equal(t, "stale", stored.Annotations[forceTakeoverAnnotation])
	})

	t.Run("without persistence takeover remains automatic", func(t *testing.T) {
		f := newOrphanTakeoverFixture(t, false, "", nil)
		_, handled := f.reconciler.promoteOrphanedReplicas(context.Background(), f.cluster, f.state, f.nodes, f.candidates)
		assert.True(t, handled)
		assert.Equal(t, 1, f.client.takeovers)
	})

	t.Run("failed takeover keeps authorization", func(t *testing.T) {
		f := newOrphanTakeoverFixture(t, true, "p1", errors.New("boom"))
		f.client.beforeCall = func() {
			stored := &valkeyiov1alpha1.ValkeyCluster{}
			assert.NoError(t, f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.cluster), stored))
			assert.Equal(t, "p1", stored.Annotations[forceTakeoverAnnotation])
			assert.Equal(t, "p1/r1", stored.Annotations[forceTakeoverAttemptAnnotation])
		}
		_, handled := f.reconciler.promoteOrphanedReplicas(context.Background(), f.cluster, f.state, f.nodes, f.candidates)
		assert.True(t, handled)
		assert.Equal(t, 1, f.client.takeovers)
		stored := &valkeyiov1alpha1.ValkeyCluster{}
		assert.NoError(t, f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.cluster), stored))
		assert.Equal(t, "p1", stored.Annotations[forceTakeoverAnnotation])
		assert.Equal(t, "p1/r1", stored.Annotations[forceTakeoverAttemptAnnotation])
	})
}

func TestHasAuthorizedOrphan(t *testing.T) {
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		forceTakeoverAnnotation: "p2,p3",
	}}}
	candidates := []orphan{{primaryId: "p1"}, {primaryId: "p2"}}
	assert.True(t, hasAuthorizedOrphan(cluster, candidates))
	assert.False(t, hasAuthorizedOrphan(cluster, candidates[:1]))
}

func TestParseForceTakeoverIDs(t *testing.T) {
	assert.Equal(t, map[string]bool{"p1": true, "p2": true}, parseForceTakeoverIDs(" p1, ,p2,p1 "))
}

func TestClearRecoveredForceTakeoverIDs(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "c", Namespace: "ns", Annotations: map[string]string{forceTakeoverAnnotation: "p1,p2"},
	}}
	r := &ValkeyClusterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()}
	state := &valkey.ClusterState{PendingNodes: []*valkey.NodeState{{Id: "p1"}}}

	assert.NoError(t, r.pruneRecoveredForceTakeoverIDs(context.Background(), cluster, state))
	stored := &valkeyiov1alpha1.ValkeyCluster{}
	assert.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(cluster), stored))
	assert.Equal(t, "p2", stored.Annotations[forceTakeoverAnnotation])

	cluster = stored
	assert.NoError(t, r.pruneRecoveredForceTakeoverIDs(context.Background(), cluster, &valkey.ClusterState{}))
	assert.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(cluster), stored))
	assert.Equal(t, "p2", stored.Annotations[forceTakeoverAnnotation], "an incomplete scrape must preserve pending authorization")
}
