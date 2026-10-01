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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	vclient "github.com/valkey-io/valkey-go"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// takeoverClient counts CLUSTER FAILOVER TAKEOVER commands and answers OK.
type takeoverClient struct {
	vclient.Client
	takeovers int
}

// B returns a builder; the zero value builds commands without a connection.
func (c *takeoverClient) B() vclient.Builder { return vclient.Builder{} }

// Do records TAKEOVER commands and returns an empty (successful) result.

func (c *takeoverClient) Do(_ context.Context, cmd vclient.Completed) vclient.ValkeyResult {
	if strings.Join(cmd.Commands(), " ") == "CLUSTER FAILOVER TAKEOVER" {
		c.takeovers++
	}
	return vclient.ValkeyResult{}
}

// TestOrphanTakeoverGrace checks the env override and its 60s fallback.
func TestOrphanTakeoverGrace(t *testing.T) {
	t.Setenv("VALKEY_OPERATOR_ORPHAN_TAKEOVER_GRACE", "")
	assert.Equal(t, 60*time.Second, orphanTakeoverGrace())
	t.Setenv("VALKEY_OPERATOR_ORPHAN_TAKEOVER_GRACE", "5m")
	assert.Equal(t, 5*time.Minute, orphanTakeoverGrace())
	t.Setenv("VALKEY_OPERATOR_ORPHAN_TAKEOVER_GRACE", "bogus")
	assert.Equal(t, 60*time.Second, orphanTakeoverGrace())
}

// TestPromoteOrphanedReplicas uses one shard whose primary is gone: without
// quorum only the operator can promote the replica.
func TestPromoteOrphanedReplicas(t *testing.T) {
	run := func(t *testing.T, persistent bool, primaryFlags string, failedSince time.Duration, wantTakeover bool, pending ...*valkey.NodeState) (*ValkeyClusterReconciler, bool) {
		c := &takeoverClient{}
		replica := &valkey.NodeState{Id: "r1", Address: "10.0.0.2", Client: c, ClusterInfo: map[string]string{"cluster_size": "1"}}
		replica.SetClusterNodesForTesting("p1 10.0.0.1:6379@16379 " + primaryFlags + " - 0 0 1 connected 0-16383\n" +
			"r1 10.0.0.2:6379@16379 myself,slave p1 0 0 1 connected\n")
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{{PrimaryId: "p1", Nodes: []*valkey.NodeState{replica}}}, PendingNodes: pending}
		cluster := &valkeyiov1alpha1.ValkeyCluster{}
		if persistent {
			cluster.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{}
		}
		r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(8)}
		if failedSince > 0 {
			r.orphanFailedSince = map[client.ObjectKey]map[string]time.Time{{}: {"p1": time.Now().Add(-failedSince)}}
		}
		_, requeue := r.promoteOrphanedReplicas(context.Background(), cluster, state)
		if wantTakeover {
			assert.Equal(t, 1, c.takeovers)
		} else {
			assert.Zero(t, c.takeovers)
		}
		return r, requeue
	}

	t.Run("no persistence", func(t *testing.T) {
		_, requeue := run(t, false, "master,fail", 0, true)
		assert.True(t, requeue)
	})
	t.Run("persistence, first FAIL", func(t *testing.T) {
		r, requeue := run(t, true, "master,fail", 0, false)
		assert.False(t, requeue)
		assert.Contains(t, r.orphanFailedSince[client.ObjectKey{}], "p1")
	})
	t.Run("persistence, grace period over", func(t *testing.T) {
		r, requeue := run(t, true, "master,fail", 2*time.Minute, true)
		assert.True(t, requeue)
		assert.NotContains(t, r.orphanFailedSince[client.ObjectKey{}], "p1")
	})
	t.Run("persistence, node loading", func(t *testing.T) {
		loading := &valkey.NodeState{Address: "10.0.0.3", Info: map[string]string{"loading": "1"}}
		r, requeue := run(t, true, "master,fail", 2*time.Minute, false, loading)
		assert.False(t, requeue)
		assert.Contains(t, r.orphanFailedSince[client.ObjectKey{}], "p1")
	})
	t.Run("persistence, primary recovered", func(t *testing.T) {
		r, requeue := run(t, true, "master", 2*time.Minute, false)
		assert.False(t, requeue)
		assert.NotContains(t, r.orphanFailedSince[client.ObjectKey{}], "p1")
	})
}

// TestOrphanGraceResetWhenQuorumReturns checks a recovered cluster does not
// keep an old FAIL time that would skip the next grace period.
func TestOrphanGraceResetWhenQuorumReturns(t *testing.T) {
	primary := &valkey.NodeState{Id: "p1", Address: "10.0.0.1", ClusterInfo: map[string]string{"cluster_size": "1"}}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{{
		PrimaryId: "p1", Slots: []valkey.SlotsRange{{Start: 0, End: 16383}}, Nodes: []*valkey.NodeState{primary},
	}}}
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{}
	r := &ValkeyClusterReconciler{orphanFailedSince: map[client.ObjectKey]map[string]time.Time{{}: {"p1": time.Now().Add(-time.Hour)}}}

	_, requeue := r.promoteOrphanedReplicas(context.Background(), cluster, state)
	assert.False(t, requeue)
	assert.NotContains(t, r.orphanFailedSince, client.ObjectKey{})
}

// TestOrphanGraceClearedOnClusterDelete checks a deleted cluster's FAIL times
// are dropped, so a recreated cluster with the same name starts fresh.
func TestOrphanGraceClearedOnClusterDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	key := client.ObjectKey{Namespace: "ns", Name: "c"}
	other := client.ObjectKey{Namespace: "ns", Name: "other"}
	r := &ValkeyClusterReconciler{
		Client:            fake.NewClientBuilder().WithScheme(scheme).Build(),
		orphanFailedSince: map[client.ObjectKey]map[string]time.Time{key: {"p1": time.Now()}, other: {"p2": time.Now()}},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	assert.NoError(t, err)
	assert.NotContains(t, r.orphanFailedSince, key)
	assert.Contains(t, r.orphanFailedSince, other)
}
