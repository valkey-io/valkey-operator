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
	"github.com/stretchr/testify/require"
	vclient "github.com/valkey-io/valkey-go"
	vmock "github.com/valkey-io/valkey-go/mock"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
	"go.uber.org/mock/gomock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	recoveryPrimaryIP   = "10.0.0.1"
	recoveryCandidateIP = "10.0.0.2"
)

func recoveryNode(id, address, table string, info map[string]string) *valkey.NodeState {
	if info != nil {
		if _, found := info["connected_slaves"]; !found {
			info["connected_slaves"] = "0"
		}
	}
	node := &valkey.NodeState{
		Id: id, Address: address, Port: DefaultPort, ShardId: "shard-" + id,
		ClusterInfo: map[string]string{
			"cluster_known_nodes": "2", "cluster_state": "ok", "cluster_slots_ok": "16384",
			"cluster_slots_pfail": "0", "cluster_slots_fail": "0",
		},
		Info: info,
	}
	node.SetClusterNodesForTesting(table)
	node.Flags = node.Myself().Flags
	return node
}

func recoveryFixtureState(candidateTable string) (*valkey.ClusterState, *valkey.NodeState, *valkey.NodeState) {
	primary := recoveryNode("primary", recoveryPrimaryIP,
		"primary 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n",
		map[string]string{"loading": "0"})
	candidate := recoveryNode("candidate", recoveryCandidateIP, candidateTable,
		map[string]string{"loading": "0"})
	state := &valkey.ClusterState{
		Shards: []*valkey.ShardState{{
			Id: "shard-primary", PrimaryId: primary.Id,
			Slots: []valkey.SlotsRange{{Start: 0, End: valkey.TotalSlots - 1}}, Nodes: []*valkey.NodeState{primary},
		}},
		PendingNodes: []*valkey.NodeState{candidate},
	}
	return state, primary, candidate
}

func recoveryNodeResources(cluster *valkeyiov1alpha1.ValkeyCluster) *valkeyiov1alpha1.ValkeyNodeList {
	owner := func() []metav1.OwnerReference {
		controlled := true
		return []metav1.OwnerReference{{APIVersion: valkeyiov1alpha1.GroupVersion.String(), Kind: "ValkeyCluster", Name: cluster.Name, UID: cluster.UID, Controller: &controlled}}
	}
	node := func(name, ip, index string) valkeyiov1alpha1.ValkeyNode {
		return valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: cluster.Namespace, UID: types.UID(name + "-uid"), Generation: 1,
				Labels: map[string]string{LabelCluster: cluster.Name, LabelShardIndex: "0", LabelNodeIndex: index}, OwnerReferences: owner(),
			},
			Spec:   valkeyiov1alpha1.ValkeyNodeSpec{WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{ObservedGeneration: 1, PodIP: ip},
		}
	}
	return &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		node("c-0-0", recoveryPrimaryIP, "0"), node("c-0-1", recoveryCandidateIP, "1"),
	}}
}

func recoveryCluster() *valkeyiov1alpha1.ValkeyCluster {
	return &valkeyiov1alpha1.ValkeyCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "cluster-uid"},
		Spec:       valkeyiov1alpha1.ValkeyClusterSpec{Shards: 1, ReplicasPerShard: 1, WorkloadType: valkeyiov1alpha1.WorkloadTypeStatefulSet},
	}
}

func TestFindDisconnectedReplica(t *testing.T) {
	baseTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"

	t.Run("finds an empty node in an independent membership view", func(t *testing.T) {
		cluster := recoveryCluster()
		nodes := recoveryNodeResources(cluster)
		state, _, candidate := recoveryFixtureState(baseTable)
		got := findDisconnectedReplica(state, nodes)
		require.NotNil(t, got)
		assert.Equal(t, candidate.Id, got.live.Id)
	})

	t.Run("finds node index zero after failover", func(t *testing.T) {
		cluster := recoveryCluster()
		nodes := recoveryNodeResources(cluster)
		nodes.Items[0].Status.PodIP = recoveryCandidateIP
		nodes.Items[1].Status.PodIP = recoveryPrimaryIP
		state, _, candidate := recoveryFixtureState(baseTable)
		got := findDisconnectedReplica(state, nodes)
		require.NotNil(t, got)
		assert.Equal(t, candidate.Id, got.live.Id)
		assert.Equal(t, "c-0-0", got.resource.Name)
	})

	tests := []struct {
		name   string
		mutate func(*valkey.ClusterState, *valkey.NodeState, *valkey.NodeState, *valkeyiov1alpha1.ValkeyNodeList)
	}{
		{"ordinary isolated startup", func(_ *valkey.ClusterState, _ *valkey.NodeState, c *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			c.ClusterInfo["cluster_known_nodes"] = "1"
		}},
		{"candidate knows the intended primary", func(_ *valkey.ClusterState, _ *valkey.NodeState, c *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			c.SetClusterNodesForTesting("candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
				"primary 10.0.0.1:6379@16379 master - 0 0 1 connected 0-16383\n")
		}},
		{"candidate remembers the intended primary with a failed link", func(_ *valkey.ClusterState, _ *valkey.NodeState, c *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			c.SetClusterNodesForTesting("candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
				"primary 10.0.0.1:6379@16379 master,fail? - 0 0 1 disconnected 0-16383\n")
		}},
		{"intended primary knows the candidate", func(_ *valkey.ClusterState, p *valkey.NodeState, _ *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			p.SetClusterNodesForTesting("primary 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n" +
				"candidate 10.0.0.2:6379@16379 master - 0 0 2 connected\n")
		}},
		{"intended primary remembers candidate with a failed link", func(_ *valkey.ClusterState, p *valkey.NodeState, _ *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			p.SetClusterNodesForTesting("primary 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n" +
				"candidate 10.0.0.2:6379@16379 master,fail? - 0 0 2 disconnected\n")
		}},
		{"pending node bridges candidate to intended primary", func(s *valkey.ClusterState, _ *valkey.NodeState, c *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			c.SetClusterNodesForTesting("candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
				"bridge 10.0.0.3:6379@16379 master - 0 0 2 connected\n")
			bridge := recoveryNode("bridge", "10.0.0.3",
				"bridge 10.0.0.3:6379@16379 myself,master - 0 0 2 connected\n"+
					"primary 10.0.0.1:6379@16379 master - 0 0 1 connected 0-16383\n",
				map[string]string{"loading": "0"})
			s.PendingNodes = append(s.PendingNodes, bridge)
		}},
		{"candidate has a dependent replica", func(_ *valkey.ClusterState, _ *valkey.NodeState, c *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			c.SetClusterNodesForTesting(baseTable + "dependent 192.0.2.2:6379@16379 slave candidate 0 0 2 connected\n")
		}},
		{"candidate is loading", func(_ *valkey.ClusterState, _ *valkey.NodeState, c *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			c.Info["loading"] = "1"
		}},
		{"candidate has a connected replication client", func(_ *valkey.ClusterState, _ *valkey.NodeState, c *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			c.Info["connected_slaves"] = "1"
		}},
		{"slot coverage is incomplete", func(s *valkey.ClusterState, _ *valkey.NodeState, _ *valkey.NodeState, _ *valkeyiov1alpha1.ValkeyNodeList) {
			s.Shards[0].Slots[0].End--
		}},
		{"node uses persistence", func(_ *valkey.ClusterState, _ *valkey.NodeState, _ *valkey.NodeState, n *valkeyiov1alpha1.ValkeyNodeList) {
			n.Items[1].Spec.Persistence = &valkeyiov1alpha1.PersistenceSpec{}
		}},
		{"node spec is not observed", func(_ *valkey.ClusterState, _ *valkey.NodeState, _ *valkey.NodeState, n *valkeyiov1alpha1.ValkeyNodeList) {
			n.Items[1].Generation++
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := recoveryCluster()
			nodes := recoveryNodeResources(cluster)
			state, primary, candidate := recoveryFixtureState(baseTable)
			tt.mutate(state, primary, candidate, nodes)
			assert.Nil(t, findDisconnectedReplica(state, nodes))
		})
	}
}

func TestCompleteRecoveryStateRequiresUniqueObservedAddresses(t *testing.T) {
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	state, _, _ := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	assert.True(t, recoveryNodeAddressesComplete(nodes))
	assert.True(t, completeRecoveryState(state, nodes))

	nodes.Items[1].Status.PodIP = ""
	assert.False(t, recoveryNodeAddressesComplete(nodes))

	nodes.Items[1].Status.PodIP = recoveryPrimaryIP
	assert.False(t, recoveryNodeAddressesComplete(nodes))
	assert.False(t, completeRecoveryState(state, nodes))
}

func TestActiveMembershipConnected(t *testing.T) {
	primary0 := recoveryNode("primary-0", "10.0.0.1",
		"primary-0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n", map[string]string{"loading": "0"})
	primary1 := recoveryNode("primary-1", "10.0.0.2",
		"primary-1 10.0.0.2:6379@16379 myself,master - 0 0 1 connected 8192-16383\n", map[string]string{"loading": "0"})
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{
		{Id: "shard-0", PrimaryId: primary0.Id, Slots: []valkey.SlotsRange{{Start: 0, End: 8191}}, Nodes: []*valkey.NodeState{primary0}},
		{Id: "shard-1", PrimaryId: primary1.Id, Slots: []valkey.SlotsRange{{Start: 8192, End: valkey.TotalSlots - 1}}, Nodes: []*valkey.NodeState{primary1}},
	}}
	assert.False(t, activeMembershipConnected(state), "separate slot-owning components are outside this repair's scope")

	primary0.SetClusterNodesForTesting("primary-0 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
		"primary-1 10.0.0.2:6379@16379 master - 0 0 1 connected 8192-16383\n")
	primary1.SetClusterNodesForTesting("primary-0 10.0.0.1:6379@16379 master - 0 0 1 connected 0-8191\n" +
		"primary-1 10.0.0.2:6379@16379 myself,master - 0 0 1 connected 8192-16383\n")
	assert.True(t, activeMembershipConnected(state))

	primary1.SetClusterNodesForTesting("primary-0 10.0.0.1:6379@16379 master - 0 0 1 connected 0-8191\n" +
		"primary-1 10.0.0.2:6379@16379 myself,master - 0 0 2 connected 0-16383\n")
	assert.False(t, activeMembershipConnected(state), "active nodes must agree on the exact slot owners")
	primary1.SetClusterNodesForTesting("primary-0 10.0.0.1:6379@16379 master - 0 0 1 connected 0-8191\n" +
		"primary-1 10.0.0.2:6379@16379 myself,master - 0 0 1 connected 8192-16383\n")

	primary0.ClusterInfo["cluster_state"] = "ok"
	primary0.ClusterInfo["cluster_slots_ok"] = "8192"
	primary0.ClusterInfo["cluster_slots_pfail"] = "8192"
	assert.False(t, activeMembershipConnected(state), "known peer IDs do not make a failed cluster-bus link healthy")
}

func recoveryFinalClient(t *testing.T, table, info string) vclient.Client {
	return recoveryFinalClientTimes(t, table, info, 1)
}

func recoveryFinalClientTimes(t *testing.T, table, info string, times int) vclient.Client {
	t.Helper()
	if !strings.Contains(info, "connected_slaves:") {
		info += "connected_slaves:0\r\n"
	}
	myself := valkey.FindMyself(valkey.ParseClusterNodes(table))
	require.NotNil(t, myself)
	ctrl := gomock.NewController(t)
	c := vmock.NewClient(ctrl)
	c.EXPECT().DoMulti(gomock.Any(),
		vmock.Match("CLUSTER", "MYID"), vmock.Match("CLUSTER", "NODES"),
		vmock.Match("INFO", "persistence", "replication", "keyspace")).
		Return([]vclient.ValkeyResult{
			vmock.Result(vmock.ValkeyString(myself.Id)),
			vmock.Result(vmock.ValkeyString(table)),
			vmock.Result(vmock.ValkeyString(info)),
		}).Times(times)
	return c
}

func recoveryFinalClientSequence(t *testing.T, table string, infos ...string) vclient.Client {
	t.Helper()
	myself := valkey.FindMyself(valkey.ParseClusterNodes(table))
	require.NotNil(t, myself)
	ctrl := gomock.NewController(t)
	c := vmock.NewClient(ctrl)
	calls := make([]any, 0, len(infos))
	for _, info := range infos {
		if !strings.Contains(info, "connected_slaves:") {
			info += "connected_slaves:0\r\n"
		}
		calls = append(calls, c.EXPECT().DoMulti(gomock.Any(),
			vmock.Match("CLUSTER", "MYID"), vmock.Match("CLUSTER", "NODES"),
			vmock.Match("INFO", "persistence", "replication", "keyspace")).
			Return([]vclient.ValkeyResult{
				vmock.Result(vmock.ValkeyString(myself.Id)),
				vmock.Result(vmock.ValkeyString(table)),
				vmock.Result(vmock.ValkeyString(info)),
			}))
	}
	gomock.InOrder(calls...)
	return c
}

func TestDisconnectedReplicaStillDisposable(t *testing.T) {
	baseTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	emptyInfo := "# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n"
	tests := []struct {
		name, table, info string
		want              bool
	}{
		{"empty independent node", baseTable, emptyInfo, true},
		{"node has keys", baseTable, emptyInfo + "db0:keys=1,expires=0,avg_ttl=0\r\n", false},
		{"node owns a slot", "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected 1\nforeign 192.0.2.1:6379@16379 master - 0 0 1 connected 0 2-16383\n", emptyInfo, false},
		{"node has a migration marker", "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected [1->-foreign]\nforeign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n", emptyInfo, false},
		{"node has a dependent replica", baseTable + "dependent 192.0.2.2:6379@16379 slave candidate 0 0 2 connected\n", emptyInfo, false},
		{"node learned an active member", baseTable + "primary 10.0.0.1:6379@16379 master - 0 0 3 connected 0-16383\n", emptyInfo, false},
		{"node remembers an active member with a failed link", baseTable + "primary 10.0.0.1:6379@16379 master,fail? - 0 0 3 disconnected 0-16383\n", emptyInfo, false},
		{"node is loading", baseTable, strings.Replace(emptyInfo, "loading:0", "loading:1", 1), false},
		{"async loading field does not hide loading", baseTable, strings.Replace(emptyInfo, "loading:0", "loading:1\r\nasync_loading:0", 1), false},
		{"keyspace section is missing", baseTable, "# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n", false},
		{"any keyspace row is rejected", baseTable, emptyInfo + "db0:keys=0,expires=0,avg_ttl=0\r\ndb1:keys=1,expires=0,avg_ttl=0\r\n", false},
		{"connected replication client", baseTable, strings.Replace(emptyInfo, "role:master", "role:master\r\nconnected_slaves:1", 1), false},
		{"handshake is not established foreign membership", "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"temporary 10.0.0.1:6379@16379 handshake - 0 0 0 connected\n", emptyInfo, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, _, candidate := recoveryFixtureState(tt.table)
			candidate.Client = recoveryFinalClient(t, tt.table, tt.info)
			got, err := disconnectedReplicaStillDisposable(context.Background(), candidate, state)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDisconnectedReplicaStillDisposableRejectsFreshPendingBridge(t *testing.T) {
	initialTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	state, _, candidate := recoveryFixtureState(initialTable)
	bridge := recoveryNode("bridge", "10.0.0.3",
		"bridge 10.0.0.3:6379@16379 myself,master - 0 0 2 connected\n"+
			"primary 10.0.0.1:6379@16379 master - 0 0 1 connected 0-16383\n",
		map[string]string{"loading": "0"})
	state.PendingNodes = append(state.PendingNodes, bridge)
	freshTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"bridge 10.0.0.3:6379@16379 master - 0 0 2 connected\n"
	candidate.Client = recoveryFinalClient(t, freshTable,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n")

	safe, err := disconnectedReplicaStillDisposable(context.Background(), candidate, state)
	require.NoError(t, err)
	assert.False(t, safe)
}

func TestDisconnectedReplicaStillDisposableRejectsActiveKnownCandidate(t *testing.T) {
	table := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	state, primary, candidate := recoveryFixtureState(table)
	primary.SetClusterNodesForTesting("primary 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n" +
		"candidate 10.0.0.2:6379@16379 master,fail? - 0 0 2 disconnected\n")
	candidate.Client = recoveryFinalClient(t, table,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n")

	safe, err := disconnectedReplicaStillDisposable(context.Background(), candidate, state)
	require.NoError(t, err)
	assert.False(t, safe)
}

func recoveryScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, valkeyiov1alpha1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

func controlledBy(kind, name string, uid types.UID) []metav1.OwnerReference {
	controlled := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, UID: uid, Controller: &controlled}}
}

func recoveryWorkloadObjects(cluster *valkeyiov1alpha1.ValkeyCluster, nodes *valkeyiov1alpha1.ValkeyNodeList, podUID types.UID) []client.Object {
	candidate := &nodes.Items[1]
	sts, pod := recoveryStatefulSetAndPod(candidate, podUID)
	return []client.Object{cluster, &nodes.Items[0], candidate, sts, pod}
}

func recoveryStatefulSetAndPod(candidate *valkeyiov1alpha1.ValkeyNode, podUID types.UID) (*appsv1.StatefulSet, *corev1.Pod) {
	candidate.Status.PodName = "valkey-c-0-1-0"
	if candidate.Labels[LabelNodeIndex] != "1" {
		candidate.Status.PodName = "valkey-" + candidate.Name + "-0"
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: valkeyNodeResourceName(candidate), Namespace: candidate.Namespace, UID: "sts-uid",
		OwnerReferences: controlledBy("ValkeyNode", candidate.Name, candidate.UID),
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: candidate.Status.PodName, Namespace: candidate.Namespace, UID: podUID,
			OwnerReferences: controlledBy("StatefulSet", sts.Name, sts.UID),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: serverContainerName}},
			Volumes:    []corev1.Volume{{Name: dataVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		},
		Status: corev1.PodStatus{
			PodIP: candidate.Status.PodIP,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: serverContainerName, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	return sts, pod
}

func TestRecoverDisconnectedReplica(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	state, _, candidate := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	finalTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	candidate.Client = recoveryFinalClientTimes(t, finalTable,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n", 2)

	scheme := recoveryScheme(t)
	objects := recoveryWorkloadObjects(cluster, nodes, "pod-uid")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.True(t, requeue)
	assert.Empty(t, blocked)
	storedPod := &corev1.Pod{}
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "valkey-c-0-1-0"}, storedPod)))
	storedNode := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-0-1"}, storedNode))
	assert.Equal(t, "pod-uid", storedNode.Annotations[disconnectedReplicaRecoveryPodUIDAnnotation])
}

func TestRecoverDisconnectedReplicaSkipsUnsafeCandidate(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	secondResource := nodes.Items[1].DeepCopy()
	secondResource.Name = "c-0-2"
	secondResource.UID = "c-0-2-uid"
	secondResource.Labels = map[string]string{LabelCluster: cluster.Name, LabelShardIndex: "0", LabelNodeIndex: "2"}
	secondResource.Status.PodIP = "10.0.0.3"
	nodes.Items = append(nodes.Items, *secondResource)

	firstTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign-1 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	state, _, first := recoveryFixtureState(firstTable)
	secondTable := "candidate-2 10.0.0.3:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign-2 192.0.2.2:6379@16379 master - 0 0 1 connected 0-16383\n"
	second := recoveryNode("candidate-2", "10.0.0.3", secondTable, map[string]string{"loading": "0"})
	state.PendingNodes = append(state.PendingNodes, second)
	first.Client = recoveryFinalClient(t, firstTable,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\ndb0:keys=1,expires=0,avg_ttl=0\r\n")
	second.Client = recoveryFinalClientTimes(t, secondTable,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n", 2)

	firstSTS, firstPod := recoveryStatefulSetAndPod(&nodes.Items[1], "first-pod-uid")
	secondSTS, secondPod := recoveryStatefulSetAndPod(&nodes.Items[2], "second-pod-uid")
	scheme := recoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		cluster, &nodes.Items[0], &nodes.Items[1], &nodes.Items[2], firstSTS, firstPod, secondSTS, secondPod,
	).WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	r := &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10)}

	requeue, blocked, err := r.replaceDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.True(t, requeue)
	assert.Empty(t, blocked)
	assert.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(firstPod), &corev1.Pod{}))
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(secondPod), &corev1.Pod{})))
}

func TestRecoverDisconnectedReplicaDoesNotMarkUnsafeCandidate(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	state, _, candidate := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	finalTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	candidate.Client = recoveryFinalClient(t, finalTable,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\ndb0:keys=1,expires=0,avg_ttl=0\r\n")

	scheme := recoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(recoveryWorkloadObjects(cluster, nodes, "pod-uid")...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Empty(t, blocked)
	stored := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-0-1"}, stored))
	assert.NotContains(t, stored.Annotations, disconnectedReplicaRecoveryPodUIDAnnotation)
	assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "valkey-c-0-1-0"}, &corev1.Pod{}))
}

func TestRecoverDisconnectedReplicaClearsMarkerWhenFinalEvidenceChanges(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	state, _, candidate := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	finalTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	emptyInfo := "# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n"
	candidate.Client = recoveryFinalClientSequence(t, finalTable, emptyInfo,
		emptyInfo+"db0:keys=1,expires=0,avg_ttl=0\r\n")

	scheme := recoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(recoveryWorkloadObjects(cluster, nodes, "pod-uid")...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Empty(t, blocked)
	stored := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-0-1"}, stored))
	assert.NotContains(t, stored.Annotations, disconnectedReplicaRecoveryPodUIDAnnotation)
	assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "valkey-c-0-1-0"}, &corev1.Pod{}))
}

func TestReconcileRecoversDisconnectedReplicaBeforeNodeUpdate(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	for i := range nodes.Items {
		desired := buildClusterValkeyNode(cluster, 0, i)
		desired.Spec.ConfigRevision = pinnedConfigRevision(renderPinnedConfig(desired))
		require.NoError(t, setDesiredWorkloadRevision(desired))
		nodes.Items[i].Spec = desired.Spec
		nodes.Items[i].Labels = desired.Labels
	}
	nodes.Items[0].Status.Ready = true
	nodes.Items[1].Status.Ready = false
	state, _, candidate := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	finalTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	candidate.Client = recoveryFinalClientTimes(t, finalTable,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n", 2)

	scheme := recoveryScheme(t)
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(recoveryWorkloadObjects(cluster, nodes, "pod-uid")...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	pool := valkey.NewPool(valkey.DefaultIdleTTL, func(vclient.ClientOption) (vclient.Client, error) {
		t.Error("unexpected live Valkey connection; reconcile must use the supplied snapshot")
		return nil, errors.New("unexpected dial")
	})
	t.Cleanup(pool.Close)
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(100),
		ValkeyClients:    NewClientProvider(c, c, pool),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}
	precondition, err := r.handleUnchangedValkeyNode(ctx, &nodes.Items[1])
	require.NoError(t, err)
	assert.Equal(t, nodeRequeued, precondition)

	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.NoError(t, err)
	assert.Equal(t, 2*time.Second, result.RequeueAfter)
	assert.True(t, apierrors.IsNotFound(c.Get(ctx,
		client.ObjectKey{Namespace: cluster.Namespace, Name: "valkey-c-0-1-0"}, &corev1.Pod{})))
	storedNode := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "c-0-1"}, storedNode))
	assert.Equal(t, "pod-uid", storedNode.Annotations[disconnectedReplicaRecoveryPodUIDAnnotation])
	storedCluster := &valkeyiov1alpha1.ValkeyCluster{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cluster), storedCluster))
	ready := meta.FindStatusCondition(storedCluster.Status.Conditions, valkeyiov1alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, "Recovering disconnected replica", ready.Message)
}

func TestRecoverDisconnectedReplicaProtectsReplacementUID(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	nodes.Items[1].Annotations = map[string]string{disconnectedReplicaRecoveryPodUIDAnnotation: "old-pod-uid"}
	state, _, candidate := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	finalTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	candidate.Client = recoveryFinalClient(t, finalTable,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n")
	scheme := recoveryScheme(t)
	objects := recoveryWorkloadObjects(cluster, nodes, "new-pod-uid")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Contains(t, blocked, "refusing to replace Pod UID new-pod-uid")
	assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "valkey-c-0-1-0"}, &corev1.Pod{}))
}

func TestRecoverDisconnectedReplicaAllowsReplacementHandshake(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	nodes.Items[1].Annotations = map[string]string{disconnectedReplicaRecoveryPodUIDAnnotation: "old-pod-uid"}
	table := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"temporary 10.0.0.1:6379@16379 handshake - 0 0 0 connected\n"
	state, _, candidate := recoveryFixtureState(table)
	candidate.Client = recoveryFinalClient(t, table,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n")
	scheme := recoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(recoveryWorkloadObjects(cluster, nodes, "new-pod-uid")...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Empty(t, blocked)
	stored := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-0-1"}, stored))
	assert.Equal(t, "old-pod-uid", stored.Annotations[disconnectedReplicaRecoveryPodUIDAnnotation])
	assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "valkey-c-0-1-0"}, &corev1.Pod{}))
}

func TestRecoverDisconnectedReplicaClearsAttemptAfterSync(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	nodes.Items[1].Annotations = map[string]string{disconnectedReplicaRecoveryPodUIDAnnotation: "old-pod-uid"}
	primary := recoveryNode("primary", recoveryPrimaryIP,
		"primary 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n", map[string]string{"loading": "0"})
	replica := recoveryNode("replica", recoveryCandidateIP,
		"replica 10.0.0.2:6379@16379 myself,slave primary 0 0 2 connected\n", map[string]string{
			"loading": "0", "master_sync_in_progress": "0", "master_link_status": "up",
		})
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{{
		Id: "shard-primary", PrimaryId: primary.Id,
		Slots: []valkey.SlotsRange{{Start: 0, End: valkey.TotalSlots - 1}}, Nodes: []*valkey.NodeState{primary, replica},
	}}}
	scheme := recoveryScheme(t)
	objects := recoveryWorkloadObjects(cluster, nodes, "replacement-pod-uid")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.True(t, requeue)
	assert.Empty(t, blocked)
	stored := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-0-1"}, stored))
	assert.NotContains(t, stored.Annotations, disconnectedReplicaRecoveryPodUIDAnnotation)
}

func TestRecoverDisconnectedReplicaClearsAttemptAfterPromotion(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	nodes.Items[1].Annotations = map[string]string{disconnectedReplicaRecoveryPodUIDAnnotation: "old-pod-uid"}
	promoted := recoveryNode("promoted", recoveryCandidateIP,
		"promoted 10.0.0.2:6379@16379 myself,master - 0 0 2 connected 0-16383\n"+
			"old-primary 10.0.0.1:6379@16379 slave promoted 0 0 2 connected\n",
		map[string]string{"loading": "0"})
	oldPrimary := recoveryNode("old-primary", recoveryPrimaryIP,
		"promoted 10.0.0.2:6379@16379 master - 0 0 2 connected 0-16383\n"+
			"old-primary 10.0.0.1:6379@16379 myself,slave promoted 0 0 2 connected\n",
		map[string]string{"loading": "0", "master_sync_in_progress": "0", "master_link_status": "up"})
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{{
		Id: "shard-promoted", PrimaryId: promoted.Id,
		Slots: []valkey.SlotsRange{{Start: 0, End: valkey.TotalSlots - 1}}, Nodes: []*valkey.NodeState{promoted, oldPrimary},
	}}}

	scheme := recoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(recoveryWorkloadObjects(cluster, nodes, "replacement-pod-uid")...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.True(t, requeue)
	assert.Empty(t, blocked)
	stored := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-0-1"}, stored))
	assert.NotContains(t, stored.Annotations, disconnectedReplicaRecoveryPodUIDAnnotation)
}

func TestRecoverDisconnectedReplicaKeepsAttemptWithoutCurrentPod(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	nodes.Items[1].Annotations = map[string]string{disconnectedReplicaRecoveryPodUIDAnnotation: "old-pod-uid"}
	primary := recoveryNode("primary", recoveryPrimaryIP,
		"primary 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n", map[string]string{"loading": "0"})
	replica := recoveryNode("replica", recoveryCandidateIP,
		"replica 10.0.0.2:6379@16379 myself,slave primary 0 0 2 connected\n", map[string]string{
			"loading": "0", "master_sync_in_progress": "0", "master_link_status": "up",
		})
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{{
		Id: "shard-primary", PrimaryId: primary.Id,
		Slots: []valkey.SlotsRange{{Start: 0, End: valkey.TotalSlots - 1}}, Nodes: []*valkey.NodeState{primary, replica},
	}}}
	scheme := recoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, &nodes.Items[0], &nodes.Items[1]).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Empty(t, blocked)
	stored := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-0-1"}, stored))
	assert.Equal(t, "old-pod-uid", stored.Annotations[disconnectedReplicaRecoveryPodUIDAnnotation])
}

func TestRecoverDisconnectedReplicaKeepsPodWhenMarkerPatchConflicts(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	state, _, candidate := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	finalTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	candidate.Client = recoveryFinalClient(t, finalTable,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n")
	scheme := recoveryScheme(t)
	objects := recoveryWorkloadObjects(cluster, nodes, "pod-uid")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: valkeyiov1alpha1.GroupVersion.Group, Resource: "valkeynodes"}, "c-0-1", errors.New("changed"))
		},
	}).WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	_, _, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	assert.Error(t, err)
	assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "valkey-c-0-1-0"}, &corev1.Pod{}))
}

type failFirstPodDeleteClient struct {
	client.Client
	failed bool
}

type failFirstMarkerClearClient struct {
	client.Client
	failed bool
}

func (c *failFirstMarkerClearClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	node, isNode := obj.(*valkeyiov1alpha1.ValkeyNode)
	if isNode && node.Annotations[disconnectedReplicaRecoveryPodUIDAnnotation] == "" && !c.failed {
		c.failed = true
		return apierrors.NewConflict(schema.GroupResource{
			Group: valkeyiov1alpha1.GroupVersion.Group, Resource: "valkeynodes",
		}, node.Name, errors.New("changed"))
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *failFirstPodDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, isPod := obj.(*corev1.Pod); isPod && !c.failed {
		c.failed = true
		return errors.New("temporary pod delete failure")
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestRecoverDisconnectedReplicaRetriesSamePodUIDAfterDeleteFailure(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	state, _, candidate := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	finalTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	candidate.Client = recoveryFinalClientTimes(t, finalTable,
		"# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n", 4)

	scheme := recoveryScheme(t)
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(recoveryWorkloadObjects(cluster, nodes, "pod-uid")...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	c := &failFirstPodDeleteClient{Client: base}
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.EqualError(t, err, "temporary pod delete failure")
	assert.True(t, requeue)
	assert.Empty(t, blocked)
	assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "valkey-c-0-1-0"}, &corev1.Pod{}))

	requeue, blocked, err = r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.True(t, requeue)
	assert.Empty(t, blocked)
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "valkey-c-0-1-0"}, &corev1.Pod{})))
}

func TestRecoverDisconnectedReplicaRetriesMarkerCleanupAfterConflict(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	state, _, candidate := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	finalTable := "candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
		"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n"
	emptyInfo := "# Persistence\r\nloading:0\r\n# Replication\r\nrole:master\r\n# Keyspace\r\n"
	keysInfo := emptyInfo + "db0:keys=1,expires=0,avg_ttl=0\r\n"
	candidate.Client = recoveryFinalClientSequence(t, finalTable, emptyInfo, keysInfo, keysInfo)

	scheme := recoveryScheme(t)
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(recoveryWorkloadObjects(cluster, nodes, "pod-uid")...).
		WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	c := &failFirstMarkerClearClient{Client: base}
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	_, _, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.Error(t, err)
	stored := &valkeyiov1alpha1.ValkeyNode{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-0-1"}, stored))
	assert.Equal(t, "pod-uid", stored.Annotations[disconnectedReplicaRecoveryPodUIDAnnotation])

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Empty(t, blocked)
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-0-1"}, stored))
	assert.NotContains(t, stored.Annotations, disconnectedReplicaRecoveryPodUIDAnnotation)
	assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "valkey-c-0-1-0"}, &corev1.Pod{}))
}

func TestRecoverDisconnectedReplicaBlocksWithoutReplicaSetPermission(t *testing.T) {
	ctx := context.Background()
	cluster := recoveryCluster()
	nodes := recoveryNodeResources(cluster)
	candidateNode := &nodes.Items[1]
	candidateNode.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeDeployment
	candidateNode.Status.PodName = "valkey-c-0-1-pod"
	state, _, _ := recoveryFixtureState(
		"candidate 10.0.0.2:6379@16379 myself,master - 0 0 2 connected\n" +
			"foreign 192.0.2.1:6379@16379 master - 0 0 1 connected 0-16383\n")
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: valkeyNodeResourceName(candidateNode), Namespace: candidateNode.Namespace, UID: "deployment-uid",
		OwnerReferences: controlledBy("ValkeyNode", candidateNode.Name, candidateNode.UID),
	}}
	pod := recoveryCandidatePod(candidateNode, "pod-uid", controlledBy("ReplicaSet", "candidate-rs", "replicaset-uid"))
	scheme := recoveryScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, &nodes.Items[0], candidateNode, deployment, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isReplicaSet := obj.(*appsv1.ReplicaSet); isReplicaSet {
					return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "replicasets"}, key.Name, errors.New("forbidden"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).WithStatusSubresource(&valkeyiov1alpha1.ValkeyCluster{}, &valkeyiov1alpha1.ValkeyNode{}, &corev1.Pod{}).Build()
	r := &ValkeyClusterReconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(10),
		clusterStateFunc: func(context.Context, *valkeyiov1alpha1.ValkeyNodeList) *valkey.ClusterState { return state },
	}

	requeue, blocked, err := r.recoverDisconnectedReplica(ctx, cluster, nodes, state, nil)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Contains(t, blocked, "needs get permission for apps/replicasets")
	assert.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
}

func TestDisconnectedReplicaPodOwnershipAndStorage(t *testing.T) {
	ctx := context.Background()
	t.Run("accepts Deployment ownership chain", func(t *testing.T) {
		cluster := recoveryCluster()
		nodes := recoveryNodeResources(cluster)
		node := &nodes.Items[1]
		node.Spec.WorkloadType = valkeyiov1alpha1.WorkloadTypeDeployment
		node.Status.PodName = "valkey-c-0-1-pod"
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: valkeyNodeResourceName(node), Namespace: node.Namespace, UID: "deployment-uid",
			OwnerReferences: controlledBy("ValkeyNode", node.Name, node.UID),
		}}
		replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name: "valkey-c-0-1-rs", Namespace: node.Namespace, UID: "replicaset-uid",
			OwnerReferences: controlledBy("Deployment", deployment.Name, deployment.UID),
		}}
		pod := recoveryCandidatePod(node, "pod-uid", controlledBy("ReplicaSet", replicaSet.Name, replicaSet.UID))
		scheme := recoveryScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, node, deployment, replicaSet, pod).Build()
		r := &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme}

		got, err := r.disconnectedReplicaPod(ctx, cluster, node)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, pod.UID, got.UID)
	})

	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{"rejects persistent data volume", func(pod *corev1.Pod) {
			pod.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"},
			}
		}},
		{"rejects foreign workload ownership", func(pod *corev1.Pod) {
			pod.OwnerReferences = controlledBy("StatefulSet", "other", "other-uid")
		}},
		{"rejects stale pod address", func(pod *corev1.Pod) {
			pod.Status.PodIP = "10.0.0.99"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := recoveryCluster()
			nodes := recoveryNodeResources(cluster)
			node := &nodes.Items[1]
			node.Status.PodName = "valkey-c-0-1-0"
			statefulSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name: valkeyNodeResourceName(node), Namespace: node.Namespace, UID: "statefulset-uid",
				OwnerReferences: controlledBy("ValkeyNode", node.Name, node.UID),
			}}
			pod := recoveryCandidatePod(node, "pod-uid", controlledBy("StatefulSet", statefulSet.Name, statefulSet.UID))
			tt.mutate(pod)
			scheme := recoveryScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, node, statefulSet, pod).Build()
			r := &ValkeyClusterReconciler{Client: c, APIReader: c, Scheme: scheme}

			got, err := r.disconnectedReplicaPod(ctx, cluster, node)
			require.NoError(t, err)
			assert.Nil(t, got)
		})
	}
}

func recoveryCandidatePod(node *valkeyiov1alpha1.ValkeyNode, uid types.UID, owners []metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: node.Status.PodName, Namespace: node.Namespace, UID: uid, OwnerReferences: owners},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: serverContainerName}},
			Volumes: []corev1.Volume{{
				Name: dataVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
		Status: corev1.PodStatus{
			PodIP: node.Status.PodIP,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: serverContainerName, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}
