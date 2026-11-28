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

package valkey

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	vclient "github.com/valkey-io/valkey-go"
	"golang.org/x/sync/errgroup"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DialFunc returns a client for the Valkey node at address (host:port). The
// client belongs to whoever supplies the DialFunc; callers never call Close on
// it. It must be safe for concurrent calls: GetClusterState dials addresses in
// parallel.
type DialFunc func(ctx context.Context, address string) (vclient.Client, error)

// NodeState represents the current state of an inspected cluster node.
type NodeState struct {
	// Client is for commands only. It belongs to whoever supplies the
	// DialFunc; never call Close on it.
	Client      vclient.Client
	Address     string
	Port        int
	Id          string
	Flags       NodeFlags
	ShardId     string
	Info        map[string]string
	ClusterInfo map[string]string

	// nodes is this node's peer table, parsed once from CLUSTER NODES by the
	// scrape.
	nodes []ClusterNode
}

// KnowsNode reports whether this node's peer table holds an entry for the given
// node ID, i.e. whether gossip has introduced that member yet. An ID that only
// appears as another entry's primary, or as a migration marker peer, does not
// count.
func (n *NodeState) KnowsNode(id string) bool {
	for _, entry := range n.nodes {
		if entry.Id == id {
			return true
		}
	}
	return false
}

// SeesSlotsOwnedBy reports whether this node's peer table assigns every given
// slot to the node with the given ID. Gossip carries a node's ID before its
// slots, so KnowsNode can hold while this does not.
func (n *NodeState) SeesSlotsOwnedBy(id string, slots []int) bool {
	for _, entry := range n.nodes {
		if entry.Id != id {
			continue
		}
		for _, slot := range slots {
			if !slices.ContainsFunc(entry.Slots, func(r SlotsRange) bool { return r.Start <= slot && slot <= r.End }) {
				return false
			}
		}
		return true
	}
	return false
}

// ReplicationOffset returns this node's processed replication offset from
// INFO replication (slave_repl_offset), or -1 when it is unavailable. A higher
// value means the replica is more caught up with its primary.
func (n *NodeState) ReplicationOffset() int64 {
	v, ok := n.Info["slave_repl_offset"]
	if !ok {
		return -1
	}
	offset, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return -1
	}
	return offset
}

// HighestOffsetReplica returns the replica with the greatest replication offset,
// the one most caught up with its primary. Replicas whose offset is unavailable
// sort last and ties keep slice order. Returns nil for an empty slice.
func HighestOffsetReplica(replicas []*NodeState) *NodeState {
	var best *NodeState
	var bestOffset int64
	for _, replica := range replicas {
		if offset := replica.ReplicationOffset(); best == nil || offset > bestOffset {
			best, bestOffset = replica, offset
		}
	}
	return best
}

// ShardState represents the current state of a shard.
type ShardState struct {
	Id        string
	PrimaryId string
	Slots     []SlotsRange
	Nodes     []*NodeState
}

// ClusterState represents the current state of a cluster.
type ClusterState struct {
	Shards       []*ShardState
	PendingNodes []*NodeState

	// targets are the pod addresses this pass set out to scrape.
	targets map[string]struct{}
}

// TotalSlots is the number of hash slots in a Valkey cluster (0-16383).
const TotalSlots = 16384

// SlotsRange is an interval or a single slot when Start and End are equal.
type SlotsRange struct {
	Start int
	End   int
}

func (s SlotsRange) String() string {
	if s.Start == s.End {
		return fmt.Sprintf("%d", s.Start)
	}
	return fmt.Sprintf("%d-%d", s.Start, s.End)
}

// FormatSlotsRanges formats a slice of SlotsRange as "0-100,500-600".
func FormatSlotsRanges(ranges []SlotsRange) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = r.String()
	}
	return strings.Join(parts, ",")
}

// GetClusterState connects to Valkey nodes and scrapes the current state.
//
// Nodes are dialled in parallel, so unreachable addresses cost about one dial
// timeout in all rather than one each. The state is built in address order
// whatever order the nodes answer in.
func GetClusterState(ctx context.Context, addresses []string, port int, dial DialFunc) *ClusterState {
	state := ClusterState{
		Shards:       make([]*ShardState, 0),
		PendingNodes: make([]*NodeState, 0),
		targets:      make(map[string]struct{}, len(addresses)),
	}
	for _, address := range addresses {
		state.targets[address] = struct{}{}
	}

	for _, node := range scrapeNodes(ctx, addresses, port, dial) {
		if node != nil {
			// Check if node is pending to be added. A primary carrying only a
			// migration marker is mid-reshard and already part of the slot map,
			// so any slot assignment counts here, not just owned ranges.
			myself := node.Myself()
			if node.IsPrimary() && (myself == nil || !myself.HasSlotAssignment()) {
				// Node not part of any shard yet.
				state.PendingNodes = append(state.PendingNodes, node)
				continue
			}

			// Find a ShardState with the same ShardId as this node.
			var shard *ShardState
			idx := slices.IndexFunc(state.Shards, func(s *ShardState) bool { return s.Id == node.ShardId })
			if idx >= 0 {
				shard = state.Shards[idx]
			} else {
				// Not know yet, adding it.
				shard = &ShardState{
					Id:    node.ShardId,
					Nodes: make([]*NodeState, 0),
				}
				state.Shards = append(state.Shards, shard)
			}
			// Add node and update shard information.
			shard.Nodes = append(shard.Nodes, node)
			if node.IsPrimary() {
				shard.Slots = node.GetSlots()
				shard.PrimaryId = node.Id
			}
		}
	}
	state.adoptDrainedPrimaries()
	return &state
}

// scrapeConcurrency caps the dials in flight during one scrape. Correctness does
// not need it; it bounds the burst of TCP and TLS handshakes when nothing is
// pooled, as after an operator restart or a credential rotation.
const scrapeConcurrency = 32

// scrapeNodes scrapes each address in parallel and returns the results in
// address order, nil where a node could not be reached. It waits for every
// dial, even after ctx is cancelled: valkey-go dials a new client without the
// caller's context.
func scrapeNodes(ctx context.Context, addresses []string, port int, dial DialFunc) []*NodeState {
	results := make([]*NodeState, len(addresses))
	var g errgroup.Group
	g.SetLimit(scrapeConcurrency)
	var panicked any
	var panicOnce sync.Once
	for i, address := range addresses {
		g.Go(func() error {
			// Hand a panic back to the caller's goroutine, where controller-runtime
			// recovers it. Here it would end the process.
			defer func() {
				if r := recover(); r != nil {
					panicOnce.Do(func() { panicked = r })
				}
			}()
			// Attempt to connect to the Valkey node and extract information.
			results[i] = getNodeState(ctx, address, port, dial)
			return nil
		})
	}
	_ = g.Wait()
	if panicked != nil {
		panic(panicked)
	}
	return results
}

// adoptDrainedPrimaries moves a slot-less pending primary into the shard of a
// replica that names it as its primary, when that shard has no primary of its
// own. A primary drained to zero slots keeps its replica and stays a primary
// (cluster-allow-replica-migration no): it leads an empty shard, it is not a
// node waiting to join. A scale-in reverted right after its drain leaves one
// behind. Any other slot-less primary, a scale-out leader before its replicas
// attach or a stray beside a live primary, stays pending.
func (s *ClusterState) adoptDrainedPrimaries() {
	pending := s.PendingNodes[:0]
	for _, node := range s.PendingNodes {
		idx := slices.IndexFunc(s.Shards, func(shard *ShardState) bool {
			return shard.PrimaryId == "" && slices.ContainsFunc(shard.Nodes, func(n *NodeState) bool {
				return n.PrimaryIdFromSelf() == node.Id
			})
		})
		if idx < 0 {
			pending = append(pending, node)
			continue
		}
		shard := s.Shards[idx]
		shard.Nodes = append(shard.Nodes, node)
		shard.PrimaryId = node.Id
	}
	s.PendingNodes = pending
}

// GetUnassignedSlots returns all unassigned slots
func (s *ClusterState) GetUnassignedSlots() []SlotsRange {
	remaining := []SlotsRange{{0, 16383}}
	for _, shard := range s.Shards {
		for _, slot := range shard.Slots {
			var next []SlotsRange //nolint:prealloc
			for _, base := range remaining {
				parts := SubtractSlotsRange(base, slot)
				next = append(next, parts...)
			}
			remaining = next
		}
	}
	return remaining
}

// UnlistedSlots returns the slots that no entry in any scraped CLUSTER NODES
// table lists, failed owners included. A node learns a primary's slots only
// from that primary's own messages, so a slot can be owned and still unlisted.
func (s *ClusterState) UnlistedSlots() []SlotsRange {
	remaining := []SlotsRange{{0, 16383}}
	for _, node := range s.AllNodes() {
		for _, entry := range node.nodes {
			for _, slot := range entry.Slots {
				var next []SlotsRange //nolint:prealloc
				for _, base := range remaining {
					next = append(next, SubtractSlotsRange(base, slot)...)
				}
				remaining = next
			}
		}
	}
	return remaining
}

// AssignableSlots returns the unassigned slots a new primary can be given. It
// leaves out ranges a scraped node's CLUSTER NODES table gives to a primary
// this pass did not scrape, at one of the pod addresses it set out to scrape.
// That primary can still be serving them, for example when only its CLUSTER
// INFO failed, and a new primary given them would take them over with a
// higher config epoch. A primary whose address answered as another id has
// been replaced, so its ranges stay assignable, unless a replica still
// follows it. A followed primary's ranges are also left out once its pod is
// gone: the replica holds the keys and can be promoted with the slots.
// GetUnassignedSlots still counts all of these as unassigned.
func (s *ClusterState) AssignableSlots() []SlotsRange {
	free := s.GetUnassignedSlots()
	scraped := make(map[string]struct{})
	liveByAddress := make(map[string]string)
	// Map the scraped nodes by id and by address. A scraped node always has
	// both; the empty checks only guard a hand-built state.
	for _, node := range s.AllNodes() {
		if node.Id == "" {
			continue
		}
		scraped[node.Id] = struct{}{}
		if node.Address != "" {
			liveByAddress[node.Address] = node.Id
		}
	}
	// A followed primary whose slots no scraped table lists may own any free
	// range, so nothing is assignable until its slots are known again.
	followed := s.followedPrimaries(scraped, liveByAddress)
	if s.followsUnknownSlots(scraped, followed) {
		return nil
	}
	// Hold back the slots of each primary a peer table lists but this pass did
	// not scrape, while a replica follows it or it is still at a pod address
	// no other node answered.
	for _, node := range s.AllNodes() {
		for _, entry := range node.nodes {
			if entry.IsMyself() || !entry.IsPrimary() || len(entry.Slots) == 0 {
				continue
			}
			if _, found := scraped[entry.Id]; found {
				continue
			}
			if _, held := followed[entry.Id]; !held {
				if _, isPod := s.targets[entry.Host]; !isPod {
					continue
				}
				if id, answered := liveByAddress[entry.Host]; answered && id != entry.Id {
					continue
				}
			}
			for _, owned := range entry.Slots {
				var next []SlotsRange //nolint:prealloc
				for _, base := range free {
					next = append(next, SubtractSlotsRange(base, owned)...)
				}
				free = next
			}
		}
	}
	return free
}

// followedPrimaries returns the ids of the primaries some replica follows. A
// scraped node's own myself line decides for that node. A replica this pass
// did not scrape counts from a peer's table while its address is a pod address
// no other node answered, as a primary does in AssignableSlots: a failed
// scrape, such as one CLUSTER INFO error, must not end the hold.
func (s *ClusterState) followedPrimaries(scraped map[string]struct{}, liveByAddress map[string]string) map[string]struct{} {
	followed := make(map[string]struct{})
	for _, node := range s.AllNodes() {
		for _, entry := range node.nodes {
			if entry.IsPrimary() || entry.PrimaryId == "" || entry.PrimaryId == "-" {
				continue
			}
			// A myself line is a scraped replica's own view of its primary and
			// counts as it is.
			if !entry.IsMyself() {
				// Other nodes' gossip about a replica that answered this pass
				// may be stale, so ignore it.
				if _, found := scraped[entry.Id]; found {
					continue
				}
				if _, isPod := s.targets[entry.Host]; !isPod {
					continue
				}
				if id, answered := liveByAddress[entry.Host]; answered && id != entry.Id {
					continue
				}
			}
			followed[entry.PrimaryId] = struct{}{}
		}
	}
	return followed
}

// followsUnknownSlots reports whether a replica follows a primary that this
// pass did not scrape and that no scraped table lists with slots. Only a
// primary's own shard learns its slots; a node that joined later lists it
// without them. Such a primary may own any free range, so none is assignable
// until a replica is promoted with the slots or answers again.
func (s *ClusterState) followsUnknownSlots(scraped, followed map[string]struct{}) bool {
	owners := make(map[string]struct{})
	for _, node := range s.AllNodes() {
		for _, entry := range node.nodes {
			if entry.IsPrimary() && len(entry.Slots) > 0 {
				owners[entry.Id] = struct{}{}
			}
		}
	}
	for id := range followed {
		if _, found := scraped[id]; found {
			continue
		}
		if _, known := owners[id]; !known {
			return true
		}
	}
	return false
}

// FindShardForAddress returns the shard containing a node with the given IP
// address, or nil if no shard contains that address.
func (s *ClusterState) FindShardForAddress(address string) *ShardState {
	for _, shard := range s.Shards {
		for _, node := range shard.Nodes {
			if node.Address == address {
				return shard
			}
		}
	}
	return nil
}

// HasAddress reports whether the node at the given address answered the scrape,
// whether it joined a shard or is still pending. It distinguishes "unreachable"
// from "reachable but not in any shard", which FindShardForAddress alone cannot.
func (s *ClusterState) HasAddress(address string) bool {
	for _, node := range s.AllNodes() {
		if node.Address == address {
			return true
		}
	}
	return false
}

// GetPrimaryNode returns the primary NodeState object
func (s *ShardState) GetPrimaryNode() *NodeState {
	idx := slices.IndexFunc(s.Nodes, func(n *NodeState) bool { return n.Id == s.PrimaryId })
	if idx >= 0 {
		return s.Nodes[idx]
	}
	return nil
}

// GetSyncedReplicas returns replica nodes that are connected and have their
// replication link up (master_link_status:up). A replica that a majority of
// the other live nodes report as failing ("fail" or "fail?") is excluded.
// That view has to come from the peers, since a node's own CLUSTER NODES
// entry never carries a failure flag, and it has to be a majority, since a
// node cut off from the bus flags every peer in its own table and would
// otherwise leave no failover target anywhere.
func (s *ShardState) GetSyncedReplicas(state *ClusterState) []*NodeState {
	var replicas []*NodeState
	for _, node := range s.Nodes {
		if node.Id == s.PrimaryId {
			continue
		}
		if state.IsNodeFailedByMajority(node.Id) {
			continue
		}
		if node.Info["master_link_status"] != "up" {
			continue
		}
		replicas = append(replicas, node)
	}
	return replicas
}

// SetClusterNodesForTesting replaces the node's peer table with one parsed
// from raw CLUSTER NODES output. The scrape fills the table for live nodes;
// tests in other packages use this to build a node with a particular view of
// its peers, and nothing else should.
func (n *NodeState) SetClusterNodesForTesting(raw string) {
	n.nodes = ParseClusterNodes(raw)
}

// Myself returns this node's own entry from its last CLUSTER NODES scrape, or
// nil when the output held no "myself" line.
func (n *NodeState) Myself() *ClusterNode {
	return FindMyself(n.nodes)
}

// GetSlots returns the slot ranges this node owns, in CLUSTER NODES order.
// Owned ranges only: in-flight migration markers are held separately on
// ClusterNode and are not assignable slots. Returns nil for a replica or when
// the output held no "myself" line.
func (n *NodeState) GetSlots() []SlotsRange {
	myself := n.Myself()
	if myself == nil || !myself.IsPrimary() {
		return nil
	}
	return myself.Slots
}

// IsPrimary return true if this is a primary node.
func (n *NodeState) IsPrimary() bool {
	return n.Flags.Has(FlagMaster)
}

// CurrentEpoch returns the cluster's current epoch as reported by this node's
// CLUSTER INFO output.
func (n *NodeState) CurrentEpoch() int64 {
	if epoch, err := strconv.ParseInt(n.ClusterInfo["cluster_current_epoch"], 10, 64); err == nil {
		return epoch
	}
	return 0
}

// IsIsolated returns true if the node's cluster_known_nodes is <= 1,
// meaning it hasn't been introduced to any other cluster member yet.
func (n *NodeState) IsIsolated() bool {
	sval, ok := n.ClusterInfo["cluster_known_nodes"]
	if !ok {
		return false
	}
	val, err := strconv.Atoi(sval)
	return err == nil && val <= 1
}

// IsReplicationInSync returns true if this replica node has its replication
// link up (master_link_status:up in INFO REPLICATION). Primary nodes always
// return true since they don't have a replication link to check.
func (n *NodeState) IsReplicationInSync() bool {
	if n.IsPrimary() {
		return true
	}
	return n.Info["master_link_status"] == "up"
}

// HasReplicaOf returns true if any live node in the cluster state reports
// itself as a replica of the given node ID. This is used to prevent
// CLUSTER FORGET from racing with auto-failover: forgetting a failed
// primary from other primaries removes it from their node tables, which
// prevents them from voting in the replica's failover election.
func (s *ClusterState) HasReplicaOf(nodeId string) bool {
	for _, shard := range s.Shards {
		for _, node := range shard.Nodes {
			if node.PrimaryIdFromSelf() == nodeId {
				return true
			}
		}
	}
	return false
}

// clusterSize returns cluster_size from CLUSTER INFO: cluster->size in Valkey,
// every primary that owns slots, reachable or not. Takes the largest value any
// scraped node reports, in case gossip has not fully propagated.
func (s *ClusterState) clusterSize() int {
	var size int
	for _, shard := range s.Shards {
		for _, node := range shard.Nodes {
			if n, err := strconv.Atoi(node.ClusterInfo["cluster_size"]); err == nil && n > size {
				size = n
			}
		}
	}
	return size
}

// HasFailoverQuorum returns true if a majority of slot-owning primaries are
// reachable. Valkey requires a majority of primaries to vote in a failover
// election; if quorum is unreachable, no automatic failover can succeed.
// Uses cluster_size from CLUSTER INFO (total primaries with slots, including
// failed ones) as the denominator.
func (s *ClusterState) HasFailoverQuorum() bool {
	if len(s.Shards) == 0 {
		return false
	}
	var livePrimaries int
	for _, shard := range s.Shards {
		if shard.GetPrimaryNode() != nil && len(shard.Slots) > 0 {
			livePrimaries++
		}
	}
	clusterSize := s.clusterSize()
	if clusterSize == 0 {
		return false
	}
	return livePrimaries > (clusterSize / 2)
}

// IsNodeFailedByMajority reports whether nodeId is down by the evidence the
// peers hold, weighing the two flags the way Valkey does. A confirmed "fail"
// is authoritative from any viewer: Valkey only sets it once a majority of
// primaries reported the node unreachable, and then broadcasts it, so one
// table carrying it already stands for a cluster-wide decision. A "fail?" is
// one node's own opinion, and a node cut off from the cluster bus marks every
// peer that way in its own table, so it counts only with the quorum Valkey
// itself requires before promoting it: reports from at least size/2+1 voting
// primaries, where size is cluster_size from CLUSTER INFO, every primary that
// owns slots. That is cluster->size in Valkey and the denominator
// HasFailoverQuorum already uses, so a primary the scrape never reached, or
// one that has no entry for nodeId yet, still counts as a voter that has not
// reported. A replica's suspicion never reaches that vote. IsNodeFailed keeps
// the any-viewer rule for both flags on the takeover path, where one report
// is the trigger.
func (s *ClusterState) IsNodeFailedByMajority(nodeId string) bool {
	var reports int
	for _, shard := range s.Shards {
		for _, node := range shard.Nodes {
			if node.Id == nodeId {
				continue // its own entry never carries a failure flag
			}
			for _, entry := range node.nodes {
				if entry.Id != nodeId {
					continue
				}
				if entry.HasFlag(FlagFail) {
					return true
				}
				if node.isVotingPrimary() && entry.HasFlag(FlagPFail) {
					reports++
				}
				break
			}
		}
	}
	size := s.clusterSize()
	return size > 0 && reports >= size/2+1
}

// isVotingPrimary mirrors clusterNodeIsVotingPrimary: a primary that owns
// slots, which is the only kind of node whose failure reports Valkey counts.
func (n *NodeState) isVotingPrimary() bool {
	myself := n.Myself()
	return n.IsPrimary() && myself != nil && myself.HasSlotAssignment()
}

// IsNodeFailed returns true if any live node reports the given node ID as
// "fail" or "fail?" in CLUSTER NODES. Includes "fail?" (pfail) because when
// majority of primaries are down, pfail can never promote to fail.
func (s *ClusterState) IsNodeFailed(nodeId string) bool {
	for _, shard := range s.Shards {
		for _, node := range shard.Nodes {
			for _, entry := range node.nodes {
				if entry.Id == nodeId && entry.IsFailing() {
					return true
				}
			}
		}
	}
	return false
}

// BestReplicaOf returns the replica with the highest replication offset
// that references the given primary ID, or nil if none found.
func (s *ClusterState) BestReplicaOf(primaryId string) *NodeState {
	var replicas []*NodeState
	for _, shard := range s.Shards {
		for _, node := range shard.Nodes {
			if node.PrimaryIdFromSelf() == primaryId {
				replicas = append(replicas, node)
			}
		}
	}
	return HighestOffsetReplica(replicas)
}

// PrimaryIdFromSelf returns the primary node ID that this node reports as its
// own primary in CLUSTER NODES. Returns "-" for primaries, the primary's node
// ID for replicas, and "" when the output held no "myself" line.
func (n *NodeState) PrimaryIdFromSelf() string {
	if myself := n.Myself(); myself != nil {
		return myself.PrimaryId
	}
	return ""
}

// AllNodes returns every reachable node in the state: shard members first,
// then pending nodes.
func (s *ClusterState) AllNodes() []*NodeState {
	var nodes []*NodeState
	for _, shard := range s.Shards {
		nodes = append(nodes, shard.Nodes...)
	}
	return append(nodes, s.PendingNodes...)
}

// FindNodeById returns the reachable node with the given cluster node ID, or
// nil if no such node was scraped.
func (s *ClusterState) FindNodeById(id string) *NodeState {
	for _, node := range s.AllNodes() {
		if node.Id == id {
			return node
		}
	}
	return nil
}

// NodeView is what the scraped nodes report for one node ID.
type NodeView struct {
	Scraped   bool     // the node answered and its own myself line is the view
	Hosts     []string // its scrape address, or every host the tables list for it
	OwnsSlots bool
	// ListedWithSlots is whether any scraped table, its own or a peer's, lists
	// it with slots. The scrape is not one snapshot, so a peer can show a
	// promotion the node's own earlier line does not.
	ListedWithSlots bool
	PrimaryIDs      []string // primaries it is listed as replicating; at most one when Scraped
}

// NodeViews returns one view per node ID any scraped node knows. A node that
// answered is judged by its own myself line alone, since a cut-off peer can
// hold a stale entry for it. Any other ID takes the union of the entries
// across all scraped tables.
func (s *ClusterState) NodeViews() map[string]NodeView {
	nodes := s.AllNodes()
	views := make(map[string]NodeView)

	for _, n := range nodes {
		myself := n.Myself()
		if myself == nil {
			continue
		}
		owns := myself.HasSlotAssignment()
		v := NodeView{Scraped: true, Hosts: []string{n.Address}, OwnsSlots: owns, ListedWithSlots: owns}
		if p := primaryIDOf(myself); p != "" {
			v.PrimaryIDs = []string{p}
		}
		views[n.Id] = v
	}

	merge := func(id, host string, entry *ClusterNode) {
		v := views[id]
		if entry != nil && entry.HasSlotAssignment() {
			v.ListedWithSlots = true
		}
		if v.Scraped {
			views[id] = v
			return
		}
		if host != "" && !slices.Contains(v.Hosts, host) {
			v.Hosts = append(v.Hosts, host)
		}
		if entry != nil {
			v.OwnsSlots = v.OwnsSlots || entry.HasSlotAssignment()
			if p := primaryIDOf(entry); p != "" && !slices.Contains(v.PrimaryIDs, p) {
				v.PrimaryIDs = append(v.PrimaryIDs, p)
			}
		}
		views[id] = v
	}

	for _, n := range nodes {
		if n.Myself() == nil {
			merge(n.Id, n.Address, nil)
		}
		for i := range n.nodes {
			merge(n.nodes[i].Id, n.nodes[i].Host, &n.nodes[i])
		}
	}

	for id, v := range views {
		slices.Sort(v.Hosts)
		slices.Sort(v.PrimaryIDs)
		views[id] = v
	}
	return views
}

// primaryIDOf returns the primary a CLUSTER NODES entry replicates, or "" for a
// primary.
func primaryIDOf(e *ClusterNode) string {
	if e.PrimaryId == "-" {
		return ""
	}
	return e.PrimaryId
}

// hostFromClusterNodesEndpoint extracts the bare host from a CLUSTER NODES
// endpoint field (<ip:port@cport[,hostname]>). Valkey writes the endpoint with a
// bare IP, so an IPv6 host carries its own colons (fd00::2:6379@16379) and
// net.SplitHostPort rejects it as ambiguous; the last-colon fallback below
// handles that case. The result compares equal to the bare pod IP Kubernetes
// reports. Returns "" when the field has no host part.
func hostFromClusterNodesEndpoint(endpoint string) string {
	// Drop the cluster-bus suffix and the optional ,hostname after it.
	if i := strings.Index(endpoint, "@"); i != -1 {
		endpoint = endpoint[:i]
	}
	if host, _, err := net.SplitHostPort(endpoint); err == nil {
		return host
	}
	// Unbracketed IPv6 and entries with no port land here: cut at the last
	// colon, which is the port separator, and drop brackets if present.
	if i := strings.LastIndex(endpoint, ":"); i != -1 {
		return strings.Trim(endpoint[:i], "[]")
	}
	return ""
}

// StaleAddressPeer pairs a viewer node with a live cluster member whose
// address in the viewer's node table is outdated.
type StaleAddressPeer struct {
	Viewer *NodeState // node whose table holds the stale entry
	Live   *NodeState // the same member, reachable at its current address
}

// FindStaleAddressPeers detects cluster members that restarted with a new
// address while other nodes' tables (persisted in nodes.conf) still point at
// the old one. This happens when many pods restart at once — e.g. a full
// Kubernetes cluster restart — leaving no surviving member to gossip the new
// addresses from, so the cluster stays partitioned until the peers are
// re-introduced (see valkey-io/valkey-operator#275).
//
// A peer entry is stale when it is flagged failing (fail, fail? or noaddr)
// in the viewer's node table, but a node with the same cluster ID was
// scraped alive at a different address. Entries whose ID matches no live
// node are genuinely dead and left for forgetStaleNodes.
func (s *ClusterState) FindStaleAddressPeers() []StaleAddressPeer {
	all := s.AllNodes()
	live := make(map[string]*NodeState, len(all))
	for _, node := range all {
		live[node.Id] = node
	}
	var stale []StaleAddressPeer
	for _, viewer := range all {
		for _, entry := range viewer.nodes {
			if entry.IsMyself() {
				continue
			}
			// fail? is included: promoting pfail to fail needs gossip
			// between a majority of primaries, which is exactly what a
			// cluster-wide address change breaks — entries can stay at
			// fail? indefinitely.
			if !entry.IsFailing() && !entry.HasNoAddress() {
				continue
			}
			peer, ok := live[entry.Id]
			if !ok {
				continue
			}
			// A noaddr entry carries no endpoint at all (:0@0) — the ID is
			// known but no address ever completed a handshake. A live node
			// with that ID always needs re-introduction.
			if entry.HasNoAddress() {
				stale = append(stale, StaleAddressPeer{Viewer: viewer, Live: peer})
				continue
			}
			if entry.Host != "" && entry.Host != peer.Address {
				stale = append(stale, StaleAddressPeer{Viewer: viewer, Live: peer})
			}
		}
	}
	return stale
}

// GetFailingNodes returns the peer-table entries this node considers failing.
//
// Only "fail" and "noaddr" count here, deliberately not "fail?": the caller
// forgets these nodes, and a pfail entry may still recover on its own.
func (n *NodeState) GetFailingNodes() []ClusterNode {
	var nodes []ClusterNode
	for _, entry := range n.nodes {
		if entry.IsMyself() {
			continue
		}
		if entry.HasFlag(FlagFail) || entry.HasNoAddress() {
			nodes = append(nodes, entry)
		}
	}
	return nodes
}

// Connect to a single Valkey node and scrapes its current state.
func getNodeState(ctx context.Context, address string, port int, dial DialFunc) *NodeState {
	hostPort := net.JoinHostPort(address, strconv.Itoa(port))
	log := logf.FromContext(ctx).WithValues("address", hostPort)

	client, err := dial(ctx, hostPort)
	if err != nil {
		log.Error(err, "failed to create Valkey client")
		return nil
	}

	node := NodeState{Client: client,
		Address: address,
		Port:    port}

	results := client.DoMulti(ctx,
		client.B().ClusterMyid().Build(),
		client.B().ClusterMyshardid().Build(),
		client.B().Info().Section("persistence", "replication").Build(),
		client.B().ClusterInfo().Build(),
		client.B().ClusterNodes().Build(),
	)

	// The round trip can fail without a dial error: a pooled client may point
	// at a server that went away since its last use, or the caller's context
	// may have expired, as when the role poller's pass budget runs out. All
	// five commands share one wire, so a transport-level failure on the first
	// result means the whole round trip failed, not just one command.
	if len(results) > 0 && results[0].NonValkeyError() != nil {
		log.Error(results[0].NonValkeyError(), "failed to reach Valkey node")
		return nil
	}
	if len(results) != 5 {
		log.Error(fmt.Errorf("expected 5 results from DoMulti, got %d", len(results)), "failed to query node state")
		return nil
	}

	// Identity and topology have to come back whole. An empty id, shard id,
	// or node table is not a node we can place: GetClusterState would group
	// every such node into one shard, skip isolation, and may hand it slots.
	// A Valkey error (BUSY, NOPERM, a renamed command) is that case. Drop the
	// node for this pass, the same as a dial failure. Callers already requeue
	// when a member is missing.
	id, err := requiredScrapeString(results[0])
	if err != nil {
		log.Error(err, "command failed: CLUSTER MYID")
		return nil
	}
	node.Id = id

	shardid, err := requiredScrapeString(results[1])
	if err != nil {
		log.Error(err, "command failed: CLUSTER MYSHARDID")
		return nil
	}
	node.ShardId = shardid

	// INFO is different. A replica with no master_link_status counts as out
	// of sync, and a primary's role comes from CLUSTER NODES, not from INFO.
	// Keep the node and leave replication fields unknown.
	info, err := results[2].ToString()
	if err != nil {
		log.Error(err, "command failed: INFO")
	} else {
		node.Info = infoStringToMap(info)
	}

	cinfo, err := requiredScrapeString(results[3])
	if err != nil {
		log.Error(err, "command failed: CLUSTER INFO")
		return nil
	}
	node.ClusterInfo = infoStringToMap(cinfo)

	cnodes, err := requiredScrapeString(results[4])
	if err != nil {
		log.Error(err, "command failed: CLUSTER NODES")
		return nil
	}
	node.nodes = ParseClusterNodes(cnodes)
	myself := node.Myself()
	if myself == nil {
		log.Error(fmt.Errorf("CLUSTER NODES reply has no myself entry"), "command failed: CLUSTER NODES")
		return nil
	}
	node.Flags = myself.Flags
	return &node
}

// requiredScrapeString returns the reply of one scrape command. A Valkey
// error and an empty reply are both failures: either one leaves a field the
// caller would otherwise treat as known.
func requiredScrapeString(res vclient.ValkeyResult) (string, error) {
	s, err := res.ToString()
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", fmt.Errorf("empty reply")
	}
	return s, nil
}

// Convert a string containing INFO elements to a map.
func infoStringToMap(s string) map[string]string {
	m := map[string]string{}
	// Remove the encoding string included in a verbatim string.
	for line := range strings.SplitSeq(strings.TrimPrefix(s, "txt:"), "\r\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		if key, val, found := strings.Cut(line, ":"); found {
			m[key] = val
		}
	}
	return m
}

// parseSlotsRange parses a string to find a single SlotsRange
func parseSlotsRange(s string) (SlotsRange, error) {
	if startText, endText, isRange := strings.Cut(s, "-"); isRange {
		if strings.Contains(endText, "-") {
			return SlotsRange{}, fmt.Errorf("invalid range: %s", s)
		}
		start, err := strconv.Atoi(startText)
		if err != nil {
			return SlotsRange{}, err
		}
		end, err := strconv.Atoi(endText)
		if err != nil {
			return SlotsRange{}, err
		}
		if start > end {
			return SlotsRange{}, fmt.Errorf("invalid range: start > end")
		}
		return SlotsRange{start, end}, nil
	}

	// Single number range
	n, err := strconv.Atoi(s)
	if err != nil {
		return SlotsRange{}, fmt.Errorf("invalid number: %s", s)
	}
	return SlotsRange{n, n}, nil
}

// SubtractSlotsRange subtracts a SlotsRange from another SlotsRange.
func SubtractSlotsRange(base, remove SlotsRange) []SlotsRange {
	var result []SlotsRange

	// No overlap
	if remove.End < base.Start || remove.Start > base.End {
		return []SlotsRange{base}
	}
	// Add range remaining on the left side.
	if remove.Start > base.Start {
		left := SlotsRange{base.Start, remove.Start - 1}
		result = append(result, left)
	}
	// Add range remaining on the right side.
	if remove.End < base.End {
		right := SlotsRange{remove.End + 1, base.End}
		result = append(result, right)
	}
	return result
}
