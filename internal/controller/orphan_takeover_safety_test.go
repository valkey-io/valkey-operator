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
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	valkeyv1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestManualTakeoverRefreshesQuorumAfterLostReply(t *testing.T) {
	f := newOrphanTakeoverFixture(t, true, "p1,p2", io.EOF)
	second := &takeoverClient{}
	r2 := orphanTestNode("r2", "r2 10.0.0.22:6379@16379 myself,slave p2 0 0 1 connected",
		"p2 10.0.0.12:6379@16379 master,fail? - 0 0 1 disconnected 5461-10922")
	r2.Client = second
	p3 := orphanTestNode("p3", "p3 10.0.0.13:6379@16379 myself,master - 0 0 1 connected 10923-16383")
	f.state.Shards = append(f.state.Shards, &valkey.ShardState{Id: "s2", Nodes: []*valkey.NodeState{r2}}, &valkey.ShardState{Id: "s3", PrimaryId: "p3", Slots: []valkey.SlotsRange{{Start: 10923, End: 16383}}, Nodes: []*valkey.NodeState{p3}})
	candidates := orphansToTakeOver(f.state)
	require.Len(t, candidates, 2)
	require.False(t, f.state.HasFailoverQuorum())
	live := &valkey.ClusterState{Shards: append([]*valkey.ShardState(nil), f.state.Shards...)}
	f.client.beforeCall = func() {
		r1 := orphanTestNode("r1", "r1 10.0.0.2:6379@16379 myself,master - 0 0 1 connected 0-5460")
		live.Shards[0] = &valkey.ShardState{Id: "s1", PrimaryId: "r1", Slots: []valkey.SlotsRange{{Start: 0, End: 5460}}, Nodes: []*valkey.NodeState{r1}}
	}
	f.reconciler.promoteOrphanedReplicas(context.Background(), f.cluster, f.state, f.nodes, candidates)
	require.True(t, live.HasFailoverQuorum())
	require.Equal(t, 1, f.client.takeovers)
	require.Zero(t, second.takeovers)
	stored := &valkeyv1.ValkeyCluster{}
	require.NoError(t, f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.cluster), stored))
	require.Equal(t, "p2", stored.Annotations[forceTakeoverAnnotation])
	_, handled := f.reconciler.promoteOrphanedReplicas(context.Background(), f.cluster, live, f.nodes, orphansToTakeOver(live))
	require.False(t, handled)
	require.Zero(t, second.takeovers)
}

func TestManualTakeoverRecordsSeparateShardEvents(t *testing.T) {
	ctx := t.Context()
	f := newOrphanTakeoverFixture(t, true, "", nil)
	kube := kfake.NewClientset()
	broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: kube.EventsV1()})
	defer broadcaster.Shutdown()
	require.NoError(t, broadcaster.StartRecordingToSinkWithContext(ctx))
	f.reconciler.Recorder = broadcaster.NewRecorder(f.reconciler.Client.Scheme(), "valkeycluster-controller")
	candidates := make([]orphan, 0, 10)
	for i := range 10 {
		candidates = append(candidates, orphan{primaryId: fmt.Sprintf("%040d", i), replica: &valkey.NodeState{Id: strings.Repeat("r", 40), Address: "10.0.0.2"}})
	}
	f.reconciler.setAwaitingManualTakeover(f.cluster, f.nodes, candidates)
	require.Eventually(t, func() bool {
		list, err := kube.EventsV1().Events(f.cluster.Namespace).List(ctx, metav1.ListOptions{})
		return err == nil && len(list.Items) == len(candidates)
	}, 3*time.Second, 10*time.Millisecond)
	list, err := kube.EventsV1().Events(f.cluster.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	for _, candidate := range candidates {
		found := false
		for _, event := range list.Items {
			if strings.Contains(event.Note, candidate.primaryId) {
				found = true
			}
		}
		require.True(t, found, "missing instructions for %s", candidate.primaryId)
	}
}

func TestManualTakeoverPrecedesDeferredRollRepairs(t *testing.T) {
	for _, approval := range []string{"", "p1"} {
		f := newOrphanReconcileFixture(t, approval, false, false, false)
		ctx := context.Background()
		settled := &valkeyv1.ValkeyNode{}
		require.NoError(t, f.reconciler.Get(ctx, client.ObjectKey{Name: "c-1-1", Namespace: "ns"}, settled))
		settled.Spec.ConfigRevision = pinnedConfigRevision(renderPinnedConfig(settled))
		require.NoError(t, setDesiredWorkloadRevision(settled))
		require.NoError(t, f.reconciler.Update(ctx, settled))
		cluster := &valkeyv1.ValkeyCluster{}
		require.NoError(t, f.reconciler.Get(ctx, client.ObjectKeyFromObject(f.cluster), cluster))
		cluster.Status.State = valkeyv1.ClusterStateReady
		setCondition(cluster, valkeyv1.ConditionReady, valkeyv1.ReasonClusterHealthy, "Cluster is healthy", metav1.ConditionTrue)
		setCondition(cluster, valkeyv1.ConditionProgressing, "ReconcileComplete", "Complete", metav1.ConditionFalse)
		require.NoError(t, f.reconciler.Status().Update(ctx, cluster))
		p2 := &valkeyv1.ValkeyNode{}
		require.NoError(t, f.reconciler.Get(ctx, client.ObjectKey{Name: "c-1-0", Namespace: "ns"}, p2))
		p2.Spec.WorkloadRevision = "prior-revision"
		require.NoError(t, f.reconciler.Update(ctx, p2))
		replacement := &valkey.NodeState{Id: "newp1", Address: "10.0.0.1", Port: 6379, Client: f.client, ClusterInfo: map[string]string{"cluster_known_nodes": "1", "cluster_size": "0"}}
		replacement.SetClusterNodesForTesting("newp1 10.0.0.1:6379@16379 myself,master - 0 0 0 connected")
		f.state.PendingNodes = []*valkey.NodeState{replacement}
		_, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(f.cluster)})
		require.NoError(t, err)
		stored := &valkeyv1.ValkeyCluster{}
		require.NoError(t, f.reconciler.Get(ctx, client.ObjectKeyFromObject(f.cluster), stored))
		require.NotContains(t, f.client.commands, "CLUSTER MEET 10.0.0.1 6379")
		if approval != "" {
			require.Equal(t, []string{"CLUSTER FAILOVER TAKEOVER"}, f.client.commands)
			require.NotEqual(t, valkeyv1.ClusterStateReady, stored.Status.State)
		} else {
			require.Empty(t, f.client.commands)
		}
	}
}

func TestManualTakeoverReplicaOffsetTie(t *testing.T) {
	f := newOrphanTakeoverFixture(t, true, "", nil)
	sibling := *f.candidates[0].replica
	sibling.Id = "r2"
	sibling.SetClusterNodesForTesting("p1 10.0.0.1:6379@16379 master,fail - 0 0 1 connected 0-16383\nr2 10.0.0.3:6379@16379 myself,slave p1 0 0 1 connected")
	first := f.candidates[0].replica
	for _, order := range [][]*valkey.NodeState{{first, &sibling}, {&sibling, first}} {
		f.state.Shards[0].Nodes = order
		require.Equal(t, "r1", orphansToTakeOver(f.state)[0].replica.Id)
	}
}

func TestManualTakeoverInstructionsRequireVersionCheck(t *testing.T) {
	f := newOrphanTakeoverFixture(t, true, "", nil)
	instructions := manualTakeoverMessage(f.cluster, f.nodes, f.candidates)
	require.Contains(t, instructions, "read current approvals and resourceVersion")
	require.Contains(t, instructions, "--resource-version=<resourceVersion>")
}
