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
	"strconv"
	"testing"

	vclient "github.com/valkey-io/valkey-go"
	vmock "github.com/valkey-io/valkey-go/mock"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// A primary whose CLUSTER INFO is denied is dropped from the scrape but keeps
// serving the slots peers list for it. Scaling out must not hand those slots
// to the new primary, which would take them over with a higher config epoch
// and drop the old primary's keys.
func TestAssignSlotsSkipsSlotsAnUnscrapedPrimaryStillServes(t *testing.T) {
	ctrl := gomock.NewController(t)
	blob := func(s string) vclient.ValkeyResult { return vmock.Result(vmock.ValkeyBlobString(s)) }
	// Each client expects only the scrape, so an ADDSLOTSRANGE fails the test.
	scrape := func(results ...vclient.ValkeyResult) vclient.Client {
		c := vmock.NewClient(ctrl)
		c.EXPECT().
			DoMulti(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(results)
		return c
	}
	clients := map[string]vclient.Client{
		"10.0.0.1:6379": scrape(blob("abc"), blob("shard-a"), blob("role:master\r\n"), blob("cluster_known_nodes:3\r\n"),
			blob("abc 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 4096-16383\n"+
				"ghi 10.0.0.3:6379@16379 master - 0 0 2 connected 0-4095\n"+
				"new 10.0.0.5:6379@16379 master - 0 0 0 connected\n")),
		"10.0.0.3:6379": scrape(blob("ghi"), blob("shard-b"), blob("role:master\r\n"),
			vmock.Result(vmock.ValkeyError("NOPERM this user has no permissions to run the 'cluster|info' command")),
			blob("ghi 10.0.0.3:6379@16379 myself,master - 0 0 2 connected 0-4095\n")),
		"10.0.0.5:6379": scrape(blob("new"), blob("shard-e"), blob("role:master\r\n"), blob("cluster_known_nodes:3\r\n"),
			blob("new 10.0.0.5:6379@16379 myself,master - 0 0 0 connected\n")),
	}
	dial := func(_ context.Context, address string) (vclient.Client, error) { return clients[address], nil }
	state := valkey.GetClusterState(context.Background(), []string{"10.0.0.1", "10.0.0.3", "10.0.0.5"}, 6379, dial)

	node := func(ip string, shard int) valkeyiov1alpha1.ValkeyNode {
		return valkeyiov1alpha1.ValkeyNode{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelShardIndex: strconv.Itoa(shard), LabelNodeIndex: "0"}},
			Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: ip},
		}
	}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		node("10.0.0.1", 0), node("10.0.0.3", 1), node("10.0.0.5", 4),
	}}
	cluster := &valkeyiov1alpha1.ValkeyCluster{Spec: valkeyiov1alpha1.ValkeyClusterSpec{Shards: 5}}
	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(10)}

	assigned, err := r.assignSlotsToPendingPrimaries(context.Background(), cluster, state, nodes)
	if err != nil || assigned != 0 {
		t.Fatalf("assigned to %d primaries (err %v), want none while the dropped primary may still serve 0-4095", assigned, err)
	}
	if got := state.GetUnassignedSlots(); len(got) != 1 || got[0] != (valkey.SlotsRange{Start: 0, End: 4095}) {
		t.Fatalf("unassigned slots = %v, want 0-4095 still reported so SlotsAssigned stays false", got)
	}
}
