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
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vclient "github.com/valkey-io/valkey-go"
	vmock "github.com/valkey-io/valkey-go/mock"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// fakeValkey answers the commands of a hibernation descent the way a server
// does: CLIENT PAUSE shows in INFO clients, and a save shows as running on
// the first INFO after it starts and finished on the next.
type fakeValkey struct {
	replica    bool
	replid     string
	offset     int64 // master_repl_offset, or slave_repl_offset on a replica
	linkUp     bool
	paused     bool
	dirty      int
	aof        bool
	saving     bool
	saveFails  bool
	lastStatus string
	saves      int
	commands   []string
	// unpauseFails makes that many CLIENT UNPAUSE calls fail.
	unpauseFails int
	// atomicMigration makes CLUSTER GETSLOTMIGRATIONS report one running.
	atomicMigration bool
	// migrationsErr makes CLUSTER GETSLOTMIGRATIONS fail with this error.
	migrationsErr string
	// pauseFails makes CLIENT PAUSE fail.
	pauseFails bool
	// saveRequestFails makes BGSAVE and BGREWRITEAOF fail on request.
	saveRequestFails bool
}

func newFakePrimary() *fakeValkey {
	return &fakeValkey{replid: "abc", offset: 100, dirty: 5, lastStatus: "ok"}
}

func newFakeReplica(offset int64) *fakeValkey {
	return &fakeValkey{replica: true, replid: "abc", offset: offset, linkUp: true, dirty: 5, lastStatus: "ok"}
}

func (f *fakeValkey) info() string {
	var b strings.Builder
	w := func(k, v string) { fmt.Fprintf(&b, "%s:%s\r\n", k, v) }
	b.WriteString("# Replication\r\n")
	if f.replica {
		w("role", "slave")
		link := "down"
		if f.linkUp {
			link = "up"
		}
		w("master_link_status", link)
		w("slave_repl_offset", strconv.FormatInt(f.offset, 10))
	} else {
		w("role", "master")
	}
	w("master_replid", f.replid)
	w("master_repl_offset", strconv.FormatInt(f.offset, 10))
	b.WriteString("# Persistence\r\n")
	running := "0"
	if f.saving {
		running = "1"
	}
	if f.aof {
		w("aof_enabled", "1")
		w("aof_rewrite_in_progress", running)
		w("aof_rewrite_scheduled", "0")
		w("aof_last_bgrewrite_status", f.lastStatus)
		w("aof_last_write_status", "ok")
		w("aof_buffer_length", "0")
		w("aof_pending_bio_fsync", "0")
		w("rdb_changes_since_last_save", strconv.Itoa(f.dirty))
		w("rdb_saves", strconv.Itoa(f.saves))
	} else {
		w("aof_enabled", "0")
		w("rdb_bgsave_in_progress", running)
		w("rdb_last_bgsave_status", f.lastStatus)
		w("rdb_changes_since_last_save", strconv.Itoa(f.dirty))
		w("rdb_saves", strconv.Itoa(f.saves))
	}
	b.WriteString("# Clients\r\n")
	if f.paused {
		w("paused_actions", "write")
	} else {
		w("paused_actions", "none")
	}
	out := b.String()
	if f.saving {
		f.saving = false
		if f.saveFails {
			f.lastStatus = "err"
		} else {
			f.lastStatus = "ok"
			f.dirty = 0
			f.saves++
		}
	}
	return out
}

func (f *fakeValkey) do(_ context.Context, cmd vclient.Completed) vclient.ValkeyResult {
	args := cmd.Commands()
	f.commands = append(f.commands, strings.Join(args, " "))
	switch strings.ToUpper(args[0]) {
	case "INFO":
		return vmock.Result(vmock.ValkeyString(f.info()))
	case "CLIENT":
		if strings.EqualFold(args[1], "PAUSE") && f.pauseFails {
			return vmock.ErrorResult(fmt.Errorf("connection reset"))
		}
		if strings.EqualFold(args[1], "UNPAUSE") && f.unpauseFails > 0 {
			f.unpauseFails--
			return vmock.ErrorResult(fmt.Errorf("connection reset"))
		}
		f.paused = strings.EqualFold(args[1], "PAUSE")
		return vmock.Result(vmock.ValkeyString("OK"))
	case "CLUSTER":
		if strings.EqualFold(args[1], "GETSLOTMIGRATIONS") && f.migrationsErr != "" {
			return vmock.Result(vmock.ValkeyError(f.migrationsErr))
		}
		if strings.EqualFold(args[1], "GETSLOTMIGRATIONS") && f.atomicMigration {
			return vmock.Result(vmock.ValkeyArray(vmock.ValkeyMap(map[string]vclient.ValkeyMessage{
				"state": vmock.ValkeyString("running"),
			})))
		}
		if strings.EqualFold(args[1], "GETSLOTMIGRATIONS") {
			return vmock.Result(vmock.ValkeyArray())
		}
	case "BGSAVE", "BGREWRITEAOF":
		if f.saveRequestFails {
			return vmock.Result(vmock.ValkeyError("ERR Background save failed to start"))
		}
		f.saving = true
		return vmock.Result(vmock.ValkeyString("Background saving started"))
	}
	return vmock.ErrorResult(fmt.Errorf("unexpected command %v", args))
}

func (f *fakeValkey) nodeState(t gomock.TestReporter, address, id string) *valkey.NodeState {
	c := vmock.NewClient(gomock.NewController(t))
	c.EXPECT().Do(gomock.Any(), gomock.Any()).DoAndReturn(f.do).AnyTimes()
	flags := []string{"master"}
	if f.replica {
		flags = []string{"slave"}
	}
	return &valkey.NodeState{Client: c, Address: address, Id: id, Flags: flags}
}

func (f *fakeValkey) sent(prefix string) int {
	n := 0
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func testNode(name, ip string) *valkeyiov1alpha1.ValkeyNode {
	return &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{}},
		Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: ip},
	}
}

func TestPersistenceProgress(t *testing.T) {
	cases := []struct {
		name string
		info map[string]string
		want saveProgress
	}{
		{"rdb running", map[string]string{"rdb_bgsave_in_progress": "1"}, saveRunning},
		{"rdb failed", map[string]string{"rdb_bgsave_in_progress": "0", "rdb_last_bgsave_status": "err", "rdb_changes_since_last_save": "0"}, saveIdle},
		{"rdb changes since save", map[string]string{"rdb_bgsave_in_progress": "0", "rdb_last_bgsave_status": "ok", "rdb_changes_since_last_save": "3"}, saveIdle},
		{"rdb clean", map[string]string{"rdb_bgsave_in_progress": "0", "rdb_last_bgsave_status": "ok", "rdb_changes_since_last_save": "0"}, saveDone},
		{"rdb fields missing", map[string]string{}, saveIdle},
		{"aof rewrite running", map[string]string{"aof_enabled": "1", "aof_rewrite_in_progress": "1"}, saveRunning},
		{"aof rewrite scheduled", map[string]string{"aof_enabled": "1", "aof_rewrite_scheduled": "1"}, saveRunning},
		{"aof rewrite failed", map[string]string{"aof_enabled": "1", "aof_last_bgrewrite_status": "err", "aof_last_write_status": "ok"}, saveIdle},
		{"aof write failed", map[string]string{"aof_enabled": "1", "aof_last_bgrewrite_status": "ok", "aof_last_write_status": "err"}, saveIdle},
		{"aof flushing", map[string]string{"aof_enabled": "1", "aof_last_bgrewrite_status": "ok", "aof_last_write_status": "ok", "aof_pending_bio_fsync": "1"}, saveRunning},
		// The RDB counter does not matter with the AOF on: the AOF holds every write.
		{"aof clean with rdb changes", map[string]string{"aof_enabled": "1", "aof_last_bgrewrite_status": "ok", "aof_last_write_status": "ok", "rdb_changes_since_last_save": "9"}, saveDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := persistenceProgress(tc.info)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestReplicaSync(t *testing.T) {
	primary := map[string]string{"master_replid": "abc", "master_repl_offset": "100"}
	cases := []struct {
		name        string
		replica     map[string]string
		wantPending bool
		wantBroken  bool
	}{
		{"caught up", map[string]string{"master_link_status": "up", "master_replid": "abc", "slave_repl_offset": "100"}, false, false},
		{"lagging", map[string]string{"master_link_status": "up", "master_replid": "abc", "slave_repl_offset": "90"}, true, false},
		{"link down", map[string]string{"master_link_status": "down", "master_replid": "abc", "slave_repl_offset": "100"}, true, true},
		{"other history", map[string]string{"master_link_status": "up", "master_replid": "def", "slave_repl_offset": "100"}, true, true},
		{"unknown offset", map[string]string{"master_link_status": "up", "master_replid": "abc"}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pending, broken := replicaSync(primary, tc.replica)
			assert.Equal(t, tc.wantPending, pending != "", pending)
			assert.Equal(t, tc.wantBroken, broken)
		})
	}
}

func TestWritesPaused(t *testing.T) {
	assert.True(t, writesPaused(map[string]string{"paused_actions": "write"}))
	assert.True(t, writesPaused(map[string]string{"paused_actions": "all"}))
	assert.False(t, writesPaused(map[string]string{"paused_actions": "none"}))
	assert.True(t, writesPaused(map[string]string{}), "a server that does not report pauses is trusted")
}

func TestHibernationTopology(t *testing.T) {
	p, r := testNode("c-0-0", "10.0.0.1"), testNode("c-0-1", "10.0.0.2")
	shard := &valkey.ShardState{Id: "s0", PrimaryId: "p", Nodes: []*valkey.NodeState{
		{Address: "10.0.0.1", Id: "p", Flags: []string{"master"}},
		{Address: "10.0.0.2", Id: "r", Flags: []string{"slave"}},
	}}

	shards, problem := hibernationTopology([]*valkeyiov1alpha1.ValkeyNode{p, r}, &valkey.ClusterState{Shards: []*valkey.ShardState{shard}})
	require.Empty(t, problem)
	require.Len(t, shards, 1)
	assert.Equal(t, "c-0-0", shards[0].primary.node.Name)
	require.Len(t, shards[0].replicas, 1)
	assert.Equal(t, "c-0-1", shards[0].replicas[0].node.Name)

	_, problem = hibernationTopology([]*valkeyiov1alpha1.ValkeyNode{p, r, testNode("c-1-0", "10.0.0.3")}, &valkey.ClusterState{Shards: []*valkey.ShardState{shard}})
	assert.Contains(t, problem, "c-1-0 does not answer")

	_, problem = hibernationTopology([]*valkeyiov1alpha1.ValkeyNode{p, r, testNode("c-1-0", "")}, &valkey.ClusterState{Shards: []*valkey.ShardState{shard}})
	assert.Contains(t, problem, "no pod IP")

	_, problem = hibernationTopology([]*valkeyiov1alpha1.ValkeyNode{p}, &valkey.ClusterState{Shards: []*valkey.ShardState{shard}})
	assert.Contains(t, problem, "has no ValkeyNode")

	pending := &valkey.ClusterState{Shards: []*valkey.ShardState{shard}, PendingNodes: []*valkey.NodeState{{Address: "10.0.0.3"}}}
	_, problem = hibernationTopology([]*valkeyiov1alpha1.ValkeyNode{p, r, testNode("c-1-0", "10.0.0.3")}, pending)
	assert.Contains(t, problem, "not joined")

	orphan := &valkey.ShardState{Id: "s0", Nodes: []*valkey.NodeState{{Address: "10.0.0.2", Id: "r", Flags: []string{"slave"}}}}
	_, problem = hibernationTopology([]*valkeyiov1alpha1.ValkeyNode{r}, &valkey.ClusterState{Shards: []*valkey.ShardState{orphan}})
	assert.Contains(t, problem, "has no primary")
}

func TestDescentStartedAndMayStart(t *testing.T) {
	assert.False(t, descentStarted(nil))
	for _, reason := range []string{valkeyiov1alpha1.ReasonHibernationWaitingForHealthy, valkeyiov1alpha1.ReasonHibernationSaveFailed, valkeyiov1alpha1.ReasonWaking} {
		assert.False(t, descentStarted(&metav1.Condition{Reason: reason}), reason)
	}
	for _, reason := range []string{valkeyiov1alpha1.ReasonHibernating, valkeyiov1alpha1.ReasonHibernated} {
		assert.True(t, descentStarted(&metav1.Condition{Reason: reason}), reason)
	}

	cluster := &valkeyiov1alpha1.ValkeyCluster{ObjectMeta: metav1.ObjectMeta{Generation: 3}}
	assert.False(t, hibernationMayStart(cluster))
	setCondition(cluster, valkeyiov1alpha1.ConditionReady, valkeyiov1alpha1.ReasonClusterHealthy, "", metav1.ConditionTrue)
	assert.True(t, hibernationMayStart(cluster))
	cluster.Generation = 4
	assert.False(t, hibernationMayStart(cluster), "a spec change the reconcile has not seen yet")
}

func TestNodeHibernated(t *testing.T) {
	controller := true
	node := testNode("n", "")
	node.Annotations[valkeyiov1alpha1.AnnotationHibernate] = "true"
	assert.False(t, nodeHibernated(node), "a standalone node ignores the annotation")
	node.OwnerReferences = []metav1.OwnerReference{{Kind: "ValkeyCluster", Name: "c", Controller: &controller}}
	assert.True(t, nodeHibernated(node))
	node.Annotations[valkeyiov1alpha1.AnnotationHibernate] = "yes"
	assert.False(t, nodeHibernated(node), "only the exact value counts")
}

// testShard builds one shard of fake servers: a primary at offset 100 and a
// replica at replicaOffset.
func testShard(t *testing.T, replicaOffset int64) (hibernationShard, *fakeValkey, *fakeValkey) {
	fp, fr := newFakePrimary(), newFakeReplica(replicaOffset)
	return hibernationShard{
		primary:  hibernationMember{node: testNode("c-0-0", "10.0.0.1"), state: fp.nodeState(t, "10.0.0.1", "p")},
		replicas: []hibernationMember{{node: testNode("c-0-1", "10.0.0.2"), state: fr.nodeState(t, "10.0.0.2", "r")}},
	}, fp, fr
}

func TestPrepareShardsPausesWaitsAndSavesEveryNode(t *testing.T) {
	r := &ValkeyClusterReconciler{}
	shard, fp, fr := testShard(t, 90)
	ctx := context.Background()
	uid := metav1.ObjectMeta{UID: "u"}.UID

	points, block := r.prepareShards(ctx, uid, []hibernationShard{shard})
	require.NotNil(t, block)
	assert.Contains(t, block.message, "offset 90 of 100")
	assert.Nil(t, points)
	assert.True(t, fp.paused, "the primary is paused before anything else")
	assert.Zero(t, fp.sent("BGSAVE")+fr.sent("BGSAVE"), "nothing saves before the replica caught up")

	fr.offset = 100
	_, block = r.prepareShards(ctx, uid, []hibernationShard{shard})
	require.NotNil(t, block)
	assert.Contains(t, block.message, "have saved their data")
	assert.Equal(t, 1, fp.sent("BGSAVE"))
	assert.Equal(t, 1, fr.sent("BGSAVE"))

	// The saves show as running on this pass, and finished on the next.
	_, block = r.prepareShards(ctx, uid, []hibernationShard{shard})
	require.NotNil(t, block)
	assert.Contains(t, block.message, "have saved their data")

	points, block = r.prepareShards(ctx, uid, []hibernationShard{shard})
	require.Nil(t, block, "%+v", block)
	assert.Equal(t, map[string]string{"c-0-0": "abc:0:1"}, points)
	assert.Equal(t, 1, fp.saves)
	assert.Equal(t, 1, fr.saves)
	assert.Equal(t, 1, fp.sent("BGSAVE"), "a node saves once per descent")
	assert.Zero(t, fr.sent("CLIENT PAUSE"), "only primaries are paused")
	assert.True(t, fp.paused, "writes stay paused for the stop that follows")
}

func TestPrepareShardsSavesOnceEvenWhenTheDataLooksClean(t *testing.T) {
	r := &ValkeyClusterReconciler{}
	shard, fp, fr := testShard(t, 100)
	fp.dirty, fr.dirty = 0, 0
	ctx := context.Background()

	_, block := r.prepareShards(ctx, "u", []hibernationShard{shard})
	require.NotNil(t, block)
	assert.Equal(t, 1, fp.sent("BGSAVE"), "a clean counter alone does not prove the file on disk is current")
	assert.Equal(t, 1, fr.sent("BGSAVE"))
}

func TestPrepareShardsUsesTheAOFWhenItIsOn(t *testing.T) {
	r := &ValkeyClusterReconciler{}
	shard, fp, fr := testShard(t, 100)
	fp.aof, fr.aof = true, true
	ctx := context.Background()

	for range 3 {
		if _, block := r.prepareShards(ctx, "u", []hibernationShard{shard}); block == nil {
			break
		}
	}
	_, block := r.prepareShards(ctx, "u", []hibernationShard{shard})
	require.Nil(t, block)
	assert.Equal(t, 1, fp.sent("BGREWRITEAOF"))
	assert.Equal(t, 1, fr.sent("BGREWRITEAOF"))
	assert.Zero(t, fp.sent("BGSAVE")+fr.sent("BGSAVE"))
}

func TestPrepareShardsAbortsOnABrokenReplica(t *testing.T) {
	r := &ValkeyClusterReconciler{}
	shard, _, fr := testShard(t, 100)
	fr.linkUp = false

	_, block := r.prepareShards(context.Background(), "u", []hibernationShard{shard})
	require.NotNil(t, block)
	assert.True(t, block.abort)
	assert.Contains(t, block.message, "replication link is down")
}

func TestPrepareShardsFailsAfterRepeatedSaveFailures(t *testing.T) {
	r := &ValkeyClusterReconciler{}
	shard, _, fr := testShard(t, 100)
	fr.saveFails = true
	ctx := context.Background()

	var block *descentBlock
	for range 10 {
		_, block = r.prepareShards(ctx, "u", []hibernationShard{shard})
		if block != nil && block.failed {
			break
		}
	}
	require.NotNil(t, block)
	require.True(t, block.failed, "%+v", block)
	assert.Contains(t, block.message, "c-0-1 could not save its data after 3 attempts")
	assert.Equal(t, hibernationSaveAttempts, fr.sent("BGSAVE"))
}

func TestDataPoint(t *testing.T) {
	rdb := map[string]string{"master_replid": "abc", "master_repl_offset": "100", "rdb_changes_since_last_save": "0", "rdb_saves": "1"}
	assert.Equal(t, "abc:0:1", dataPoint(rdb))
	rdb["master_repl_offset"] = "114"
	assert.Equal(t, "abc:0:1", dataPoint(rdb), "a replication PING does not move it")

	aof := map[string]string{"master_replid": "abc", "aof_enabled": "1", "aof_current_size": "4096",
		"rdb_changes_since_last_save": "12", "rdb_saves": "0"}
	before := dataPoint(aof)
	assert.Equal(t, "abc:aof:4096", before)
	aof["rdb_changes_since_last_save"], aof["rdb_saves"] = "0", "1"
	assert.Equal(t, before, dataPoint(aof), "an RDB save point firing with the AOF on does not move it")
	aof["aof_current_size"] = "4133"
	assert.NotEqual(t, before, dataPoint(aof), "a write grows the AOF")
}

func TestCheckPrimaries(t *testing.T) {
	ctx := context.Background()
	r := &ValkeyClusterReconciler{}
	// A paused primary that saved once with its replicas and changed nothing since.
	savedPrimary := func() *fakeValkey {
		fp := newFakePrimary()
		fp.paused, fp.dirty, fp.saves = true, 0, 1
		return fp
	}
	newState := func(fp *fakeValkey, t *testing.T) *valkey.ClusterState {
		return &valkey.ClusterState{Shards: []*valkey.ShardState{{Id: "s0", PrimaryId: "p",
			Nodes: []*valkey.NodeState{fp.nodeState(t, "10.0.0.1", "p")}}}}
	}
	recordedNode := func() *valkeyiov1alpha1.ValkeyNode {
		node := testNode("c-0-0", "10.0.0.1")
		node.Annotations[valkeyiov1alpha1.AnnotationHibernateDataPoint] = "abc:0:1"
		return node
	}

	t.Run("a primary that still holds the recorded data passes without saving again", func(t *testing.T) {
		fp := savedPrimary()
		assert.Nil(t, r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{recordedNode()}, newState(fp, t)))
		assert.Zero(t, fp.sent("BGSAVE"))
	})

	t.Run("replication PINGs move the offset, not the data, and pass", func(t *testing.T) {
		fp := savedPrimary()
		fp.offset += 14
		assert.Nil(t, r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{recordedNode()}, newState(fp, t)))
	})

	t.Run("a write after the replicas stopped aborts", func(t *testing.T) {
		fp := savedPrimary()
		fp.dirty = 3
		block := r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{recordedNode()}, newState(fp, t))
		require.NotNil(t, block)
		assert.True(t, block.abort)
		assert.Contains(t, block.message, `"abc:0:1" to "abc:3:1"`)
	})

	t.Run("a write hidden by a later save aborts", func(t *testing.T) {
		fp := savedPrimary()
		fp.saves = 2
		block := r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{recordedNode()}, newState(fp, t))
		require.NotNil(t, block)
		assert.True(t, block.abort)
	})

	t.Run("a restarted primary aborts", func(t *testing.T) {
		fp := savedPrimary()
		fp.replid = "def"
		block := r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{recordedNode()}, newState(fp, t))
		require.NotNil(t, block)
		assert.True(t, block.abort)
	})

	t.Run("a primary whose pause lapsed and that took a write aborts, not waits", func(t *testing.T) {
		fp := savedPrimary()
		fp.paused, fp.dirty = false, 4
		block := r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{recordedNode()}, newState(fp, t))
		require.NotNil(t, block)
		assert.True(t, block.abort)
	})

	t.Run("a primary whose pause lapsed without a write waits for the pause", func(t *testing.T) {
		fp := savedPrimary()
		fp.paused = false
		block := r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{recordedNode()}, newState(fp, t))
		require.NotNil(t, block)
		assert.False(t, block.abort)
		assert.Contains(t, block.message, "Waiting for writes to pause")
	})

	t.Run("a primary that moved is not hidden behind one that is waited for", func(t *testing.T) {
		moved := savedPrimary()
		moved.dirty = 2
		second := testNode("c-1-0", "10.0.0.2")
		second.Annotations[valkeyiov1alpha1.AnnotationHibernateDataPoint] = "abc:0:1"
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{{Id: "s1", PrimaryId: "q",
			Nodes: []*valkey.NodeState{moved.nodeState(t, "10.0.0.2", "q")}}}}
		// The first primary does not answer, so it can only be waited for.
		block := r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{testNode("c-0-0", "10.0.0.9"), second}, state)
		require.NotNil(t, block)
		assert.True(t, block.abort)
		assert.Contains(t, block.message, "c-1-0 moved")
	})

	t.Run("a primary without a recorded data point aborts", func(t *testing.T) {
		block := r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{testNode("c-0-0", "10.0.0.1")}, newState(savedPrimary(), t))
		require.NotNil(t, block)
		assert.True(t, block.abort)
	})

	t.Run("a primary that does not answer is waited for", func(t *testing.T) {
		block := r.checkPrimaries(ctx, []*valkeyiov1alpha1.ValkeyNode{testNode("c-0-0", "10.0.0.9")}, &valkey.ClusterState{})
		require.NotNil(t, block)
		assert.False(t, block.abort)
		assert.Contains(t, block.message, "waiting for primary c-0-0 to answer")
	})
}

func TestPrimaryServes(t *testing.T) {
	primary := testNode("c-0-0", "10.0.0.1")
	primary.Status.Ready = true
	build := func(aSeenByB string) *valkey.ClusterState {
		a := &valkey.NodeState{Address: "10.0.0.1", Port: 6379, Id: "a", Flags: []string{"myself", "master"},
			ClusterInfo: map[string]string{"cluster_state": "ok"}}
		a.SetClusterNodesForTesting("a 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
			"b 10.0.0.2:6379@16379 master - 0 0 2 connected 8192-16383\n")
		b := &valkey.NodeState{Address: "10.0.0.2", Port: 6379, Id: "b", Flags: []string{"myself", "master"},
			ClusterInfo: map[string]string{"cluster_state": "ok"}}
		b.SetClusterNodesForTesting("b 10.0.0.2:6379@16379 myself,master - 0 0 2 connected 8192-16383\n" +
			"a 10.0.0.1:6379@16379 " + aSeenByB + " - 0 0 1 connected 0-8191\n")
		return &valkey.ClusterState{Shards: []*valkey.ShardState{
			{Id: "s0", PrimaryId: "a", Slots: []valkey.SlotsRange{{Start: 0, End: 8191}}, Nodes: []*valkey.NodeState{a}},
			{Id: "s1", PrimaryId: "b", Slots: []valkey.SlotsRange{{Start: 8192, End: 16383}}, Nodes: []*valkey.NodeState{b}},
		}}
	}

	assert.True(t, primaryServes(primary, build("master")))
	assert.False(t, primaryServes(primary, build("master,fail?")), "one member's suspicion can still turn into a failure")
	assert.False(t, primaryServes(primary, build("master,fail")))

	notReady := primary.DeepCopy()
	notReady.Status.Ready = false
	assert.False(t, primaryServes(notReady, build("master")))
	assert.False(t, primaryServes(primary, &valkey.ClusterState{}), "a primary that does not answer")
}

func TestSlotMigrationInFlight(t *testing.T) {
	ctx := context.Background()
	shard := func(fp *fakeValkey, nodes string) []hibernationShard {
		ns := fp.nodeState(t, "10.0.0.1", "p")
		if nodes != "" {
			ns.SetClusterNodesForTesting(nodes)
		}
		return []hibernationShard{{primary: hibernationMember{node: testNode("c-0-0", "10.0.0.1"), state: ns}}}
	}

	assert.Empty(t, slotMigrationInFlight(ctx, shard(newFakePrimary(), "p 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\n")))
	assert.Contains(t, slotMigrationInFlight(ctx, shard(newFakePrimary(),
		"p 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460 [5461->-q]\n")), "slots of c-0-0 are being migrated")

	fp := newFakePrimary()
	fp.atomicMigration = true
	assert.Contains(t, slotMigrationInFlight(ctx, shard(fp, "")), "atomic slot migration is running on c-0-0")
}

func TestPrepareShardsDoesNotCountAFailedSaveRequest(t *testing.T) {
	r := &ValkeyClusterReconciler{}
	shard, fp, fr := testShard(t, 100)
	// Both nodes look clean, so only a save request that went through proves
	// the data on disk is from this descent.
	fp.dirty, fr.dirty = 0, 0
	fr.saveRequestFails = true
	ctx := context.Background()

	var block *descentBlock
	for range 10 {
		_, block = r.prepareShards(ctx, "u", []hibernationShard{shard})
		require.NotNil(t, block, "a node whose save request failed is never saved")
		if block.failed {
			break
		}
	}
	assert.True(t, block.failed)
	assert.Contains(t, block.message, "c-0-1 could not save its data after 3 attempts")
	assert.Contains(t, block.message, "Background save failed to start")
}

func TestPrepareShardsBacksOffWhenAPrimaryRejectsThePause(t *testing.T) {
	r := &ValkeyClusterReconciler{}
	shard, fp, _ := testShard(t, 100)
	fp.pauseFails = true

	_, block := r.prepareShards(context.Background(), "u", []hibernationShard{shard})
	require.NotNil(t, block)
	assert.True(t, block.abort, "writes are resumed rather than held on the primaries already paused")
}

func TestSlotMigrationInFlightQueryErrors(t *testing.T) {
	ctx := context.Background()
	shard := func(fp *fakeValkey) []hibernationShard {
		return []hibernationShard{{primary: hibernationMember{node: testNode("c-0-0", "10.0.0.1"), state: fp.nodeState(t, "10.0.0.1", "p")}}}
	}
	old := newFakePrimary()
	old.migrationsErr = "ERR unknown subcommand 'GETSLOTMIGRATIONS'"
	assert.Empty(t, slotMigrationInFlight(ctx, shard(old)), "a server without atomic migration has none running")

	flaky := newFakePrimary()
	flaky.migrationsErr = "LOADING Valkey is loading the dataset in memory"
	assert.Contains(t, slotMigrationInFlight(ctx, shard(flaky)), "could not check slot migrations on c-0-0")
}
