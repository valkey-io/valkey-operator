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
	"slices"
	"strconv"
	"strings"

	vclient "github.com/valkey-io/valkey-go"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// NodeState represents the current state of an inspected cluster node.
type NodeState struct {
	Client       vclient.Client
	Address      string
	Port         int
	Id           string
	Flags        []string
	ShardId      string
	Info         map[string]string
	ClusterInfo  map[string]string
	ClusterNodes string
}

// ShardState represents the current state of a shard.
type ShardState struct {
	Id        string
	PrimaryId string
	Slots     []SlotsRange
	Nodes     []*NodeState
}

// ShardState represents the current state of a cluster.
type ClusterState struct {
	Shards       []*ShardState
	PendingNodes []*NodeState
}

// TotalSlots is the number of hash slots in a Valkey cluster (0-16383).
const TotalSlots = 16384

// SlotsRange is an interval or a single slot when Start and End are equal.
type SlotsRange struct {
	Start int
	End   int
}

// GetClusterState connects to Valkey nodes and scrapes the current state.
func GetClusterState(ctx context.Context, addresses []string, port int) *ClusterState {
	state := ClusterState{
		Shards:       make([]*ShardState, 0),
		PendingNodes: make([]*NodeState, 0),
	}

	for _, address := range addresses {
		// Attempt to connect to the Valkey node and extract information.
		node := getNodeState(ctx, address, port)
		if node != nil {
			// Check if node is pending to be added.
			if node.IsPrimary() && len(node.GetSlots()) == 0 {
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
				ranges, _ := parseSlotsRanges(node.GetSlots())
				shard.Slots = ranges
				shard.PrimaryId = node.Id
			}
		}
	}
	return &state
}

// CloseClients disconnects all valkey-go clients.
func (s *ClusterState) CloseClients() {
	for _, node := range s.PendingNodes {
		if node.Client != nil {
			node.Client.Close()
		}
	}
	for _, shard := range s.Shards {
		for _, node := range shard.Nodes {
			if node.Client != nil {
				node.Client.Close()
			}
		}
	}
}

// GetUnassignedSlots returns all unassigned slots
func (s *ClusterState) GetUnassignedSlots() []SlotsRange {
	remaining := []SlotsRange{{0, 16383}}
	for _, shard := range s.Shards {
		for _, slot := range shard.Slots {
			var next []SlotsRange
			for _, base := range remaining {
				parts := SubtractSlotsRange(base, slot)
				next = append(next, parts...)
			}
			remaining = next
		}
	}
	return remaining
}

// GetPrimaryNode returns the primary NodeState object
func (s *ShardState) GetPrimaryNode() *NodeState {
	idx := slices.IndexFunc(s.Nodes, func(n *NodeState) bool { return n.Id == s.PrimaryId })
	if idx >= 0 {
		return s.Nodes[idx]
	}
	return nil
}

// GetSlots returns slots assigned to myself, same format as in CLUSTER NODES.
func (n *NodeState) GetSlots() []string {
	// Parse CLUSTER NODES output.
	// <id> <ip:port@cport[,hostname]> <flags> <master> <ping-sent> <pong-recv> <config-epoch> <link-state> <slot> <slot> ... <slot>
	for line := range strings.SplitSeq(n.ClusterNodes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		flags := strings.Split(fields[2], ",")
		if slices.Contains(flags, "myself") {
			if slices.Contains(flags, "master") {
				// Get slots starting at field 8
				return fields[8:]
			}
		}
	}
	return nil
}

// IsPrimary return true if this is a primary node.
func (n *NodeState) IsPrimary() bool {
	return slices.Contains(n.Flags, "master")
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

// GetFailingNodes returns all known nodes that are failing.
func (n *NodeState) GetFailingNodes() []NodeState {
	nodes := []NodeState{}
	for line := range strings.SplitSeq(n.ClusterNodes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		flags := strings.Split(fields[2], ",")
		if !slices.Contains(flags, "myself") {
			if slices.Contains(flags, "fail") || slices.Contains(flags, "noaddr") {
				// Get IP address from <ip:port@cport[,hostname]>
				if idx := strings.LastIndex(fields[1], ":"); idx != -1 {
					nodes = append(nodes, NodeState{Address: fields[1][:idx], Id: fields[0]})
				}
			}
		}
	}
	return nodes
}

// Connect to a single Valkey node and scrapes its current state.
func getNodeState(ctx context.Context, address string, port int) *NodeState {
	log := logf.FromContext(ctx)

	opt := vclient.ClientOption{
		InitAddress:       []string{fmt.Sprintf("%s:%d", address, port)},
		ForceSingleClient: true, // Don't connect to another cluster node.
	}

	client, err := vclient.NewClient(opt)
	if err != nil {
		log.Info("failed to create Valkey client", "err", err)
		return nil
	}

	node := NodeState{Client: client,
		Address: address,
		Port:    port}

	id, err := client.Do(ctx, client.B().ClusterMyid().Build()).ToString()
	if err != nil {
		log.Error(err, "command failed: CLUSTER MYID")
	}
	node.Id = id

	shardid, err := client.Do(ctx, client.B().ClusterMyshardid().Build()).ToString()
	if err != nil {
		log.Error(err, "command failed: CLUSTER MYSHARDID")
	}
	node.ShardId = shardid

	info, err := client.Do(ctx, client.B().Info().Build()).ToString()
	if err != nil {
		log.Error(err, "command failed: INFO")
	}
	node.Info = infoStringToMap(info)

	cinfo, err := client.Do(ctx, client.B().ClusterInfo().Build()).ToString()
	if err != nil {
		log.Error(err, "command failed: CLUSTER INFO")
	}
	node.ClusterInfo = infoStringToMap(cinfo)

	cnodes, err := client.Do(ctx, client.B().ClusterNodes().Build()).ToString()
	if err != nil {
		log.Error(err, "command failed: CLUSTER NODES")
	}
	// Remove the encoding string included in a verbatim string.
	node.ClusterNodes = strings.TrimPrefix(cnodes, "txt:")

	// Extract flags
	for line := range strings.SplitSeq(node.ClusterNodes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		flags := strings.Split(fields[2], ",")
		if slices.Contains(flags, "myself") {
			node.Flags = flags
		}
	}
	return &node
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

// parseSlotsRange parses strings to find multiple SlotsRange's
func parseSlotsRanges(s []string) ([]SlotsRange, error) {
	ranges := []SlotsRange{}

	for _, part := range s {
		r, err := parseSlotsRange(part)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, r)
	}
	return ranges, nil
}

// parseSlotsRange parses a string to find a single SlotsRange
func parseSlotsRange(s string) (SlotsRange, error) {
	if strings.Contains(s, "-") {
		parts := strings.Split(s, "-")
		if len(parts) != 2 {
			return SlotsRange{}, fmt.Errorf("invalid range: %s", s)
		}
		start, err := strconv.Atoi(parts[0])
		if err != nil {
			return SlotsRange{}, err
		}
		end, err := strconv.Atoi(parts[1])
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
