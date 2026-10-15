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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	vclient "github.com/valkey-io/valkey-go"
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// migrationStateServer is a scripted RESP server that completes the valkey-go
// client handshake and answers CLUSTER GETSLOTMIGRATIONS with the entries the
// reply callback produces. It lets the controller-level migration-failure
// branches run without a live Valkey, mirroring the helper used by the
// internal/valkey migration state tests.
type migrationStateServer struct {
	listener net.Listener
	addr     string
	reply    func() [][]string
}

func newMigrationStateServer(t *testing.T, reply func() [][]string) *migrationStateServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &migrationStateServer{listener: ln, addr: ln.Addr().String(), reply: reply}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *migrationStateServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		cmd, err := readStateCommand(r)
		if err != nil {
			return
		}
		if _, err := w.WriteString(s.respond(cmd)); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// respond answers one command with a RESP fragment.
func (s *migrationStateServer) respond(cmd []string) string {
	switch {
	case strings.EqualFold(cmd[0], "HELLO"):
		return "%6\r\n$5\r\nproto\r\n:3\r\n$7\r\nversion\r\n$5\r\n9.0.0\r\n" +
			"$4\r\nrole\r\n$6\r\nmaster\r\n$2\r\nid\r\n$40\r\n" +
			"0123456789012345678901234567890123456789\r\n" +
			"$4\r\nmode\r\n$10\r\nstandalone\r\n$7\r\nmodules\r\n*0\r\n"
	case len(cmd) > 1 && strings.EqualFold(cmd[0], "CLUSTER") && strings.EqualFold(cmd[1], "GETSLOTMIGRATIONS"):
		entries := s.reply()
		var b strings.Builder
		fmt.Fprintf(&b, "*%d\r\n", len(entries))
		for _, entry := range entries {
			fmt.Fprintf(&b, "*%d\r\n", len(entry))
			for _, field := range entry {
				if _, err := strconv.Atoi(field); err == nil {
					fmt.Fprintf(&b, ":%s\r\n", field)
				} else {
					fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(field), field)
				}
			}
		}
		return b.String()
	default:
		return "+OK\r\n"
	}
}

// readStateCommand reads one RESP array-of-bulk-strings request.
func readStateCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString(10)
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("unexpected request prefix %q", line[:1])
	}
	count, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	cmd := make([]string, 0, count)
	for range count {
		header, err := r.ReadString(10)
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(strings.TrimRight(header[1:], "\r\n"))
		if err != nil {
			return nil, err
		}
		if n < 0 {
			cmd = append(cmd, "")
			continue
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		cmd = append(cmd, string(buf[:n]))
	}
	return cmd, nil
}

// foreignTargetID is a node ID that is not the planned destination in the
// fixtures below (dst-primary-id); entries targeted at it model a failure
// for some other move.
const foreignTargetID = "4b4f12fdfb58d5e30fef7b9ad3f1651dacbbaba9"

// stateEntry builds one GETSLOTMIGRATIONS entry: flat field/value pairs,
// numeric fields as RESP integers, matching the server wire format. An
// empty targetNode omits the field, modeling a server that does not
// report the migration destination.
func stateEntry(state, targetNode, slotRanges, message string, lastUpdate time.Time) []string {
	entry := []string{
		"name", "5371b28997de6fd0bbe813ad8ebdfdf2faadb308",
		"operation", "EXPORT",
		"slot_ranges", slotRanges,
		"source_node", "93941e777e17fcbc92d4398cc957ffea888f472b",
		"state", state,
		"message", message,
		"last_update_time", strconv.FormatInt(lastUpdate.Unix(), 10),
	}
	if targetNode != "" {
		entry = append(entry, "target_node", targetNode)
	}
	return entry
}

// scriptedSrcNode builds a NodeState whose client dials the scripted server.
// The peer table stays empty, so KnowsNode reports false; the failure branch
// under test is reached before the gossip gate, and no destination dial is
// ever made: MIGRATESLOTS is issued on the source client.
func scriptedSrcNode(t *testing.T, s *migrationStateServer, id string) *valkey.NodeState {
	t.Helper()
	client, err := vclient.NewClient(vclient.ClientOption{
		InitAddress:       []string{s.addr},
		ForceSingleClient: true,
		DisableCache:      true,
		DisableRetry:      true,
		PipelineMultiplex: -1,
	})
	if err != nil {
		t.Fatalf("client init: %v", err)
	}
	t.Cleanup(client.Close)
	host, portStr, _ := net.SplitHostPort(s.addr)
	port, _ := strconv.Atoi(portStr)
	return &valkey.NodeState{Client: client, Address: host, Port: port, Id: id}
}

// plainDstNode builds a destination primary that needs no client: neither the
// failure branch nor the gossip gate dials it, and its address appears only in
// the surfaced error message.
func plainDstNode(address, id string) *valkey.NodeState {
	return &valkey.NodeState{Address: address, Id: id}
}

// rebalanceFixture builds a two-shard state whose slot counts differ by more
// than one slot, so PlanRebalanceMove plans a move from src to dst covering
// slots 0-399 (capped by rebalanceSlotBatchSize).
func rebalanceFixture(t *testing.T, server *migrationStateServer) (src, dst *valkey.ShardState) {
	t.Helper()
	srcNode := scriptedSrcNode(t, server, "src-primary-id")
	dstNode := plainDstNode("10.0.0.2", "dst-primary-id")
	src = &valkey.ShardState{
		Id:        "src-shard",
		PrimaryId: "src-primary-id",
		Slots:     []valkey.SlotsRange{{Start: 0, End: 16383}},
		Nodes:     []*valkey.NodeState{srcNode},
	}
	dst = &valkey.ShardState{
		Id:        "dst-shard",
		PrimaryId: "dst-primary-id",
		Nodes:     []*valkey.NodeState{dstNode},
	}
	return src, dst
}

func TestRebalanceSlotsSurfacesMigrationFailure(t *testing.T) {
	// A failed migration job overlapping the planned move, within the
	// recency window, must surface as an error carrying the job message
	// instead of being silently retried every reconcile.
	server := newMigrationStateServer(t, func() [][]string {
		return [][]string{stateEntry("failed", "dst-primary-id", "0-399", "handshake failed: ERR synthesize", time.Now())}
	})
	src, dst := rebalanceFixture(t, server)

	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(64)}
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Shards = 2

	// Sanity: the planner must select a move from src to dst.
	move, err := valkey.PlanRebalanceMove([]*valkey.ShardState{src, dst}, 2, rebalanceSlotBatchSize)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if move == nil {
		t.Fatal("expected a planned move")
	}

	inProgress, err := r.rebalanceSlots(context.Background(), cluster, []*valkey.ShardState{src, dst})
	if err == nil {
		t.Fatalf("expected error from surfaced migration failure, got inProgress=%v", inProgress)
	}
	if !strings.Contains(err.Error(), "handshake failed: ERR synthesize") {
		t.Fatalf("error must carry the failed job message, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "slot migration from ") {
		t.Fatalf("error must name the move, got %q", err.Error())
	}
	var migrationFailed *slotMigrationFailedError
	if !errors.As(err, &migrationFailed) {
		t.Fatalf("rebalance failure must stay distinguishable as an async slot migration failure, got %T", err)
	}
}

func TestRebalanceSlotsAncientFailureDoesNotBlock(t *testing.T) {
	// A failed job older than the recency window must not block the
	// rebalance: after the window lapses the move is no longer held, so
	// the reconcile proceeds past the failure gate (here up to the gossip
	// gate, which requeues without error).
	server := newMigrationStateServer(t, func() [][]string {
		return [][]string{stateEntry("failed", "dst-primary-id", "0-399", "ancient failure", time.Now().Add(-6*time.Minute))}
	})
	src, dst := rebalanceFixture(t, server)

	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(64)}
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Shards = 2

	inProgress, err := r.rebalanceSlots(context.Background(), cluster, []*valkey.ShardState{src, dst})
	if err != nil {
		t.Fatalf("ancient failure must not error, got %v", err)
	}
	if !inProgress {
		t.Fatal("expected the reconcile to proceed past the failure gate")
	}
}

func TestDrainExcessShardsSurfacesMigrationFailure(t *testing.T) {
	// Scale-in: the src shard (index 1 >= the expected 1) drains into the
	// dst shard (index 0). A failed drain migration within the recency
	// window must surface as an error instead of being re-issued.
	server := newMigrationStateServer(t, func() [][]string {
		return [][]string{stateEntry("failed", "dst-primary-id", "0-399", "transfer failed: ERR drain", time.Now())}
	})
	src, dst := rebalanceFixture(t, server)

	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(64)}
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Shards = 1

	srcPod := &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "src-node", Labels: map[string]string{LabelShardIndex: "1", LabelNodeIndex: "0"}},
		Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: src.Nodes[0].Address},
	}
	dstPod := &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "dst-node", Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "0"}},
		Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: dst.Nodes[0].Address},
	}

	state := &valkey.ClusterState{Shards: []*valkey.ShardState{src, dst}}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{*srcPod, *dstPod}}

	drained, err := r.drainExcessShards(context.Background(), cluster, state, nodes)
	if err == nil {
		t.Fatalf("expected error from surfaced drain migration failure, got drained=%v", drained)
	}
	if !strings.Contains(err.Error(), "transfer failed: ERR drain") {
		t.Fatalf("error must carry the failed job message, got %q", err.Error())
	}
	var migrationFailed *slotMigrationFailedError
	if !errors.As(err, &migrationFailed) {
		t.Fatalf("drain failure must stay distinguishable as an async slot migration failure, got %T", err)
	}
}

func TestRebalanceSlotsForeignDestinationFailureDoesNotBlock(t *testing.T) {
	// A failed S->A job must not wedge a rebalance that now plans the
	// same slots to a different destination B: the move proceeds past
	// the failure gate (here up to the gossip gate, which requeues
	// without error).
	server := newMigrationStateServer(t, func() [][]string {
		return [][]string{stateEntry("failed", foreignTargetID, "0-399", "handshake failed at the old destination", time.Now())}
	})
	src, dst := rebalanceFixture(t, server)

	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(64)}
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Shards = 2

	inProgress, err := r.rebalanceSlots(context.Background(), cluster, []*valkey.ShardState{src, dst})
	if err != nil {
		t.Fatalf("a failure for another destination must not error, got %v", err)
	}
	if !inProgress {
		t.Fatal("expected the rebalance to proceed past the failure gate")
	}
}

func TestDrainExcessShardsForeignDestinationFailureProceeds(t *testing.T) {
	// Scale-in drains must also ignore a leftover failure that targeted a
	// node other than the shard the drain plan picked: the drain
	// proceeds past the failure gate to the gossip requeue.
	server := newMigrationStateServer(t, func() [][]string {
		return [][]string{stateEntry("failed", foreignTargetID, "0-399", "transfer failed to the old destination", time.Now())}
	})
	src, dst := rebalanceFixture(t, server)

	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(64)}
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Shards = 1

	srcPod := &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "src-node", Labels: map[string]string{LabelShardIndex: "1", LabelNodeIndex: "0"}},
		Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: src.Nodes[0].Address},
	}
	dstPod := &valkeyiov1alpha1.ValkeyNode{
		ObjectMeta: metav1.ObjectMeta{Name: "dst-node", Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "0"}},
		Status:     valkeyiov1alpha1.ValkeyNodeStatus{PodIP: dst.Nodes[0].Address},
	}

	state := &valkey.ClusterState{Shards: []*valkey.ShardState{src, dst}}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{*srcPod, *dstPod}}

	requeue, err := r.drainExcessShards(context.Background(), cluster, state, nodes)
	if err != nil {
		t.Fatalf("a failure for another destination must not error, got %v", err)
	}
	if !requeue {
		t.Fatal("expected the drain to proceed past the failure gate and requeue")
	}
}

// recorderEvents drains the fake recorder's buffered events so tests can
// assert on what handleScaleIn emitted.
func recorderEvents(recorder events.EventRecorder) []string {
	rec, ok := recorder.(*events.FakeRecorder)
	if !ok {
		return nil
	}
	var got []string
	reading := true
	for reading {
		select {
		case ev := <-rec.Events:
			got = append(got, ev)
		default:
			reading = false
		}
	}
	return got
}

func scaleInFixture(t *testing.T, server *migrationStateServer, specShards int32, srcPrimaryMissing bool) (*valkeyiov1alpha1.ValkeyCluster, *valkey.ClusterState, *valkeyiov1alpha1.ValkeyNodeList) {
	t.Helper()
	if server != nil {
		src, dst := rebalanceFixture(t, server)
		state := &valkey.ClusterState{Shards: []*valkey.ShardState{src, dst}}
		nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
			{ObjectMeta: metav1.ObjectMeta{Name: "src-node", Labels: map[string]string{LabelShardIndex: "1", LabelNodeIndex: "0"}},
				Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: src.Nodes[0].Address}},
			{ObjectMeta: metav1.ObjectMeta{Name: "dst-node", Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "0"}},
				Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: dst.Nodes[0].Address}},
		}}
		cluster := &valkeyiov1alpha1.ValkeyCluster{}
		cluster.Spec.Shards = specShards
		return cluster, state, nodes
	}
	// No scripted server: a drain whose plan cannot find a destination
	// primary errors before any migration state is read.
	src := &valkey.ShardState{
		Id:        "src-shard",
		PrimaryId: "src-primary-id",
		Slots:     []valkey.SlotsRange{{Start: 0, End: 16383}},
		Nodes:     []*valkey.NodeState{plainDstNode("10.0.0.1", "src-primary-id")},
	}
	dst := &valkey.ShardState{
		Id:        "dst-shard",
		PrimaryId: "missing-primary-id",
		Nodes:     []*valkey.NodeState{plainDstNode("10.0.0.2", "dst-node-id")},
	}
	state := &valkey.ClusterState{Shards: []*valkey.ShardState{src, dst}}
	nodes := &valkeyiov1alpha1.ValkeyNodeList{Items: []valkeyiov1alpha1.ValkeyNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "src-node", Labels: map[string]string{LabelShardIndex: "1", LabelNodeIndex: "0"}},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.0.1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "dst-node", Labels: map[string]string{LabelShardIndex: "0", LabelNodeIndex: "0"}},
			Status: valkeyiov1alpha1.ValkeyNodeStatus{PodIP: "10.0.0.2"}},
	}}
	cluster := &valkeyiov1alpha1.ValkeyCluster{}
	cluster.Spec.Shards = specShards
	return cluster, state, nodes
}

func TestHandleScaleInMigrationFailureEmitsSlotRebalanceFailed(t *testing.T) {
	// An asynchronously failed drain migration is the rebalance failure
	// class: handleScaleIn must keep the SlotRebalanceFailed event for
	// it, leave DrainFailed to other drain errors, and hold Degraded=True
	// with reason RebalanceFailed.
	server := newMigrationStateServer(t, func() [][]string {
		return [][]string{stateEntry("failed", "dst-primary-id", "0-399", "transfer failed: ERR drain", time.Now())}
	})
	cluster, state, nodes := scaleInFixture(t, server, 1, false)

	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(64)}

	_, requeue := r.handleScaleIn(context.Background(), cluster, state, nodes)
	if !requeue {
		t.Fatal("expected handleScaleIn to requeue on the surfaced failure")
	}

	got := recorderEvents(r.Recorder)
	var rebalanceFailed, drainFailed bool
	for _, ev := range got {
		if strings.Contains(ev, "SlotRebalanceFailed") {
			rebalanceFailed = true
		}
		if strings.Contains(ev, "DrainFailed") {
			drainFailed = true
		}
	}
	if !rebalanceFailed {
		t.Fatalf("expected a SlotRebalanceFailed event, got %q", got)
	}
	if drainFailed {
		t.Fatalf("an async migration failure must not emit DrainFailed, got %q", got)
	}

	cond := meta.FindStatusCondition(cluster.Status.Conditions, valkeyiov1alpha1.ConditionDegraded)
	if cond == nil {
		t.Fatal("expected a Degraded condition")
	}
	if cond.Reason != valkeyiov1alpha1.ReasonRebalanceFailed {
		t.Fatalf("Degraded reason = %q, want %q", cond.Reason, valkeyiov1alpha1.ReasonRebalanceFailed)
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded status = %q, want True", cond.Status)
	}
}

func TestHandleScaleInDrainPlanErrorEmitsDrainFailed(t *testing.T) {
	// Drain failures that are not asynchronous migration failures keep
	// the DrainFailed event: here the drain plan cannot find a
	// destination primary, which fails before any migration state is read.
	cluster, state, nodes := scaleInFixture(t, nil, 1, true)

	r := &ValkeyClusterReconciler{Recorder: events.NewFakeRecorder(64)}

	_, requeue := r.handleScaleIn(context.Background(), cluster, state, nodes)
	if !requeue {
		t.Fatal("expected handleScaleIn to requeue on the drain failure")
	}

	got := recorderEvents(r.Recorder)
	var rebalanceFailed, drainFailed bool
	for _, ev := range got {
		if strings.Contains(ev, "SlotRebalanceFailed") {
			rebalanceFailed = true
		}
		if strings.Contains(ev, "DrainFailed") {
			drainFailed = true
		}
	}
	if !drainFailed {
		t.Fatalf("expected a DrainFailed event, got %q", got)
	}
	if rebalanceFailed {
		t.Fatalf("a non-migration drain failure must not emit SlotRebalanceFailed, got %q", got)
	}
}
