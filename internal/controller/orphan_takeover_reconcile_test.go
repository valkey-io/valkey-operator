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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vclient "github.com/valkey-io/valkey-go"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// newOrphanReconcileFixture adds a second shard to the orphan fixture. Its
// replica can hold the node-update phase while shard 0 waits for recovery.
func newOrphanReconcileFixture(t *testing.T, annotation string, holdRoll, unschedulable, staleAddress bool) *orphanTakeoverFixture {
	t.Helper()
	f := newOrphanTakeoverFixture(t, true, annotation, nil)
	f.cluster.Spec.Shards = 2
	f.cluster.Spec.Replicas = 1
	f.cluster.Spec.Image = "valkey/valkey:9.0.0"
	f.cluster.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeStatefulSet

	replica := f.state.Shards[0].Nodes[0]
	replica.ShardId = "s0"
	replica.Port = DefaultPort
	replica.ClusterInfo["cluster_size"] = "2"
	primary := &valkey.NodeState{
		Id: "p2", Address: "10.0.1.1", Port: DefaultPort, ShardId: "s1", Client: f.client,
		ClusterInfo: map[string]string{"cluster_size": "2"},
	}
	primary.SetClusterNodesForTesting("p2 10.0.1.1:6379@16379 myself,master - 0 0 1 connected 8192-16383\n")
	f.state.Shards = append(f.state.Shards, &valkey.ShardState{
		Id: "s1", PrimaryId: "p2", Slots: []valkey.SlotsRange{{Start: 8192, End: 16383}}, Nodes: []*valkey.NodeState{primary},
	})
	if staleAddress {
		replica.SetClusterNodesForTesting("p1 10.0.0.1:6379@16379 master,fail - 0 0 1 connected 0-8191\n" +
			"r1 10.0.0.2:6379@16379 myself,slave p1 0 0 1 connected\n" +
			"p2 10.0.1.9:6379@16379 master,fail? - 0 0 1 disconnected 8192-16383\n")
	}

	scheme := f.reconciler.Client.Scheme()
	objects := []client.Object{f.cluster}
	for shard := range 2 {
		for index := range 2 {
			node := buildClusterValkeyNode(f.cluster, shard, index)
			require.NoError(t, setDesiredWorkloadRevision(node))
			require.NoError(t, controllerutil.SetControllerReference(f.cluster, node, scheme))
			node.Status.Ready = true
			switch {
			case shard == 0 && index == 0:
				node.Status.PodIP = "10.0.0.1"
				node.Status.Ready = false
			case shard == 0 && index == 1:
				node.Status.PodIP = replica.Address
			case shard == 1 && index == 0:
				node.Status.PodIP = primary.Address
			case shard == 1 && index == 1:
				node.Status.Ready = !holdRoll
			}
			objects = append(objects, node)
		}
	}
	if unschedulable {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "c-0-0-0", Namespace: f.cluster.Namespace, Labels: map[string]string{LabelCluster: f.cluster.Name, LabelShardIndex: "0"}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "volume cannot be attached",
			}}},
		})
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}).Build()
	pool := valkey.NewPool(valkey.DefaultIdleTTL, func(vclient.ClientOption) (vclient.Client, error) {
		t.Error("unexpected live Valkey connection; the reconcile must use the supplied snapshot")
		return nil, errors.New("unexpected dial")
	})
	t.Cleanup(pool.Close)
	f.reconciler = &ValkeyClusterReconciler{
		Client: cl, APIReader: cl, Scheme: scheme, Recorder: events.NewFakeRecorder(100),
		ValkeyClients: NewClientProvider(cl, cl, pool),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState {
			return f.state
		},
	}
	return f
}

func TestReconcileOrphanTakeoverDuringNodeUpdate(t *testing.T) {
	for _, unschedulable := range []bool{false, true} {
		name := "unready replica in another shard"
		if unschedulable {
			name = "unschedulable primary and unready replica in another shard"
		}
		t.Run(name, func(t *testing.T) {
			f := newOrphanReconcileFixture(t, "p1", true, unschedulable, false)
			key := client.ObjectKeyFromObject(f.cluster)
			result, err := f.reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			require.NoError(t, err)
			assert.Equal(t, 2*time.Second, result.RequeueAfter)
			assert.Equal(t, 1, f.client.takeovers, "authorization must be processed before node-update or scheduling returns")
			stored := &valkeyiov1alpha1.ValkeyCluster{}
			require.NoError(t, f.reconciler.Get(context.Background(), key, stored))
			assert.NotContains(t, stored.Annotations, forceTakeoverAnnotation)
		})
	}
}

func TestReconcileOrphanWaitHealsStaleAddresses(t *testing.T) {
	for _, holdRoll := range []bool{false, true} {
		name := "without a node update in progress"
		if holdRoll {
			name = "during a node update"
		}
		t.Run(name, func(t *testing.T) {
			f := newOrphanReconcileFixture(t, "", holdRoll, false, true)
			key := client.ObjectKeyFromObject(f.cluster)
			result, err := f.reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			require.NoError(t, err)
			assert.Equal(t, 2*time.Second, result.RequeueAfter)
			assert.Equal(t, []string{"CLUSTER MEET 10.0.1.1 6379"}, f.client.commands)
			assert.Zero(t, f.client.takeovers, "safe address repair must not imply takeover authorization")
			stored := &valkeyiov1alpha1.ValkeyCluster{}
			require.NoError(t, f.reconciler.Get(context.Background(), key, stored))
			condition := meta.FindStatusCondition(stored.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
			require.NotNil(t, condition)
			assert.Equal(t, valkeyiov1alpha1.ReasonAwaitingManualTakeover, condition.Reason)
			assert.Contains(t, condition.Message, forceTakeoverAnnotation+"=p1")
		})
	}
}

func TestPromoteOrphanedReplicasRequiresDurableAuthorization(t *testing.T) {
	ctx := context.Background()
	for _, patchErr := range []error{
		errors.New("API unavailable"),
		apierrors.NewConflict(schema.GroupResource{Group: "valkey.io", Resource: "valkeyclusters"}, "c", errors.New("resourceVersion changed")),
	} {
		t.Run(patchErr.Error(), func(t *testing.T) {
			f := newOrphanTakeoverFixture(t, true, "p1", nil)
			attempts := 0
			f.reconciler.Client = interceptor.NewClient(f.reconciler.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					attempts++
					return patchErr
				},
			})
			_, handled := f.reconciler.promoteOrphanedReplicas(ctx, f.cluster, f.state, f.nodes, f.candidates)
			assert.True(t, handled)
			assert.Equal(t, 1, attempts)
			assert.Zero(t, f.client.takeovers, "a failed authorization patch must prevent TAKEOVER")
			stored := &valkeyiov1alpha1.ValkeyCluster{}
			require.NoError(t, f.reconciler.Get(ctx, client.ObjectKeyFromObject(f.cluster), stored))
			assert.Equal(t, "p1", stored.Annotations[forceTakeoverAnnotation])
		})
	}

	t.Run("revoked after the initial read", func(t *testing.T) {
		f := newOrphanTakeoverFixture(t, true, "p1", nil)
		stored := &valkeyiov1alpha1.ValkeyCluster{}
		require.NoError(t, f.reconciler.Get(ctx, client.ObjectKeyFromObject(f.cluster), stored))
		delete(stored.Annotations, forceTakeoverAnnotation)
		require.NoError(t, f.reconciler.Update(ctx, stored))
		// f.cluster retains the earlier annotation, as a reconcile racing a
		// user's revocation would. Consumption must re-read the stored approval.
		_, handled := f.reconciler.promoteOrphanedReplicas(ctx, f.cluster, f.state, f.nodes, f.candidates)
		assert.True(t, handled)
		assert.Zero(t, f.client.takeovers)
		assert.NotContains(t, f.cluster.Annotations, forceTakeoverAnnotation)
	})
}

func TestManualTakeoverWarningAndReadiness(t *testing.T) {
	f := newOrphanTakeoverFixture(t, true, "", nil)
	meta.SetStatusCondition(&f.cluster.Status.Conditions, metav1.Condition{
		Type: valkeyiov1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: valkeyiov1alpha1.ReasonClusterHealthy,
	})
	candidates := make([]orphan, 0, 10)
	for i := range 10 {
		candidates = append(candidates, orphan{
			primaryId: fmt.Sprintf("%040d", i),
			replica:   &valkey.NodeState{Id: strings.Repeat("r", 40), Address: "10.0.0.2"},
		})
	}
	recorder := events.NewFakeRecorder(len(candidates))
	f.reconciler.Recorder = recorder
	f.reconciler.setAwaitingManualTakeover(f.cluster, f.nodes, candidates)
	degraded := meta.FindStatusCondition(f.cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	require.NotNil(t, degraded)
	assert.Greater(t, len(degraded.Message), 1024, "the fixture must exceed the event limit")
	ready := meta.FindStatusCondition(f.cluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status, "manual recovery must clear a previously healthy Ready condition")
	assert.Equal(t, valkeyiov1alpha1.ReasonAwaitingManualTakeover, ready.Reason)
	for range len(candidates) {
		event := <-recorder.Events
		assert.LessOrEqual(t, len(event), 1024)
		assert.Contains(t, event, "Warning AwaitingManualTakeover")
		assert.Contains(t, event, forceTakeoverAnnotation+"=")
	}
	f.reconciler.setAwaitingManualTakeover(f.cluster, f.nodes, candidates)
	select {
	case event := <-recorder.Events:
		t.Fatalf("unchanged recovery instructions must not emit another event: %s", event)
	default:
	}
}

func TestPruneRecoveredForceTakeoverIDsRefreshesRevokedAnnotation(t *testing.T) {
	f := newOrphanTakeoverFixture(t, true, "p1,p2", nil)
	ctx := context.Background()
	stored := &valkeyiov1alpha1.ValkeyCluster{}
	require.NoError(t, f.reconciler.Get(ctx, client.ObjectKeyFromObject(f.cluster), stored))
	stored.Annotations[forceTakeoverAnnotation] = "p2"
	require.NoError(t, f.reconciler.Update(ctx, stored))
	// The initial snapshot still contains p1, but the re-read needs no patch:
	// its recovered approval was already removed by the user.
	state := &valkey.ClusterState{PendingNodes: []*valkey.NodeState{{Id: "p1"}}}
	require.NoError(t, f.reconciler.pruneRecoveredForceTakeoverIDs(ctx, f.cluster, state))
	assert.Equal(t, "p2", f.cluster.Annotations[forceTakeoverAnnotation])
	assert.False(t, hasAuthorizedOrphan(f.cluster, f.candidates), "the stale snapshot must not retain p1's authorization")
}

func TestPromoteOrphanedReplicasDoesNotReuseAuthorization(t *testing.T) {
	f := newOrphanTakeoverFixture(t, true, "p1", nil)
	ctx := context.Background()
	for range 2 {
		_, handled := f.reconciler.promoteOrphanedReplicas(ctx, f.cluster, f.state, f.nodes, f.candidates)
		assert.True(t, handled)
	}
	assert.Equal(t, 1, f.client.takeovers, "a stale topology must not allow a second TAKEOVER with consumed authorization")
}
