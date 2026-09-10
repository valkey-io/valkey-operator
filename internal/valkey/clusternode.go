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
	"slices"
	"strconv"
	"strings"
)

// clusterNodesMinFields is the number of fields every CLUSTER NODES line has
// before the optional slot fields:
//
//	<id> <ip:port@cport[,hostname]> <flags> <primary> <ping-sent> <pong-recv> <config-epoch> <link-state>
//
// Slot fields, when present, start at index clusterNodesMinFields.
const clusterNodesMinFields = 8

// SlotMigration is one in-flight slot handoff from a CLUSTER NODES line.
// Peer is the other end of the handoff: the node the slot is moving to for a
// migrating marker, and the node it is coming from for an importing marker.
type SlotMigration struct {
	Slot int
	Peer string
}

// ClusterNode is one parsed line of CLUSTER NODES output.
//
// Slots holds only the ranges this node owns. Migrating and Importing hold the
// in-flight markers ("[5461->-<id>]" and "[5461-<-<id>]") that share the same
// trailing fields on the wire. Keeping them apart matters because a node can
// carry a marker while owning no slots at all, and the two cases mean different
// things to a caller deciding whether a node participates in the slot map.
//
// Fields are limited to what callers need, for a minimal memory footprint.
type ClusterNode struct {
	Id        string
	Host      string // bare host from the endpoint field, unbracketed for IPv6
	Flags     []string
	PrimaryId string // "-" on a primary line, the primary's ID on a replica
	Slots     []SlotsRange
	Migrating []SlotMigration
	Importing []SlotMigration
}

// HasFlag reports whether the line carries the given flag.
func (c *ClusterNode) HasFlag(flag string) bool {
	return slices.Contains(c.Flags, flag)
}

// IsMyself reports whether this line describes the node that produced the
// output.
func (c *ClusterNode) IsMyself() bool {
	return c.HasFlag("myself")
}

// IsPrimary reports whether this line describes a primary.
func (c *ClusterNode) IsPrimary() bool {
	return c.HasFlag("master")
}

// IsFailing reports whether the viewer considers this node down. "fail?"
// (pfail) counts: promoting pfail to fail needs gossip between a majority of
// primaries, so an entry can stay at fail? indefinitely once that majority is
// gone.
func (c *ClusterNode) IsFailing() bool {
	return c.HasFlag("fail") || c.HasFlag("fail?")
}

// HasNoAddress reports whether the viewer knows this node's ID but has no
// address for it.
func (c *ClusterNode) HasNoAddress() bool {
	return c.HasFlag("noaddr")
}

// HasSlotAssignment reports whether the line carries any slot field, either an
// owned range or an in-flight migration marker. A primary with no slot field at
// all has not joined the slot map yet, whereas one holding only a marker is
// mid-reshard and is already part of it.
func (c *ClusterNode) HasSlotAssignment() bool {
	return len(c.Slots) > 0 || len(c.Migrating) > 0 || len(c.Importing) > 0
}

// FindMyself returns the entry flagged "myself", or nil when none carries it.
func FindMyself(nodes []ClusterNode) *ClusterNode {
	for i := range nodes {
		if nodes[i].IsMyself() {
			return &nodes[i]
		}
	}
	return nil
}

// ParseClusterNodes parses CLUSTER NODES output into one entry per node.
// Lines with fewer than clusterNodesMinFields fields are skipped, which covers
// the trailing empty line and any partial output.
func ParseClusterNodes(raw string) []ClusterNode {
	var nodes []ClusterNode
	for line := range strings.SplitSeq(raw, "\n") {
		if node, ok := parseClusterNodesLine(line); ok {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// parseClusterNodesLine parses a single CLUSTER NODES line. The bool is false
// when the line is too short to describe a node.
func parseClusterNodesLine(line string) (ClusterNode, bool) {
	fields := strings.Fields(line)
	if len(fields) < clusterNodesMinFields {
		return ClusterNode{}, false
	}

	node := ClusterNode{
		Id:        fields[0],
		Host:      hostFromClusterNodesEndpoint(fields[1]),
		Flags:     strings.Split(fields[2], ","),
		PrimaryId: fields[3],
	}

	// Owned ranges and migration markers are interleaved in the same trailing
	// fields, so split them before parsing the ranges.
	var owned []string
	for _, field := range fields[clusterNodesMinFields:] {
		migration, direction, ok := parseSlotMigration(field)
		switch {
		case !ok:
			owned = append(owned, field)
		case direction == slotMigrating:
			node.Migrating = append(node.Migrating, migration)
		default:
			node.Importing = append(node.Importing, migration)
		}
	}
	if ranges, err := parseSlotsRanges(owned); err == nil {
		node.Slots = ranges
	}

	return node, true
}

type slotMigrationDirection int

const (
	slotMigrating slotMigrationDirection = iota
	slotImporting
)

// parseSlotMigration parses an in-flight slot marker: "[5461->-<id>]" for a
// slot leaving this node and "[5461-<-<id>]" for one arriving. The bool is
// false for any field that is not a well-formed marker, including a plain slot
// range.
func parseSlotMigration(field string) (SlotMigration, slotMigrationDirection, bool) {
	if !strings.HasPrefix(field, "[") || !strings.HasSuffix(field, "]") {
		return SlotMigration{}, slotMigrating, false
	}
	body := field[1 : len(field)-1]

	direction := slotMigrating
	slotPart, peer, found := strings.Cut(body, "->-")
	if !found {
		direction = slotImporting
		slotPart, peer, found = strings.Cut(body, "-<-")
		if !found {
			return SlotMigration{}, slotMigrating, false
		}
	}

	slot, err := strconv.Atoi(slotPart)
	if err != nil || peer == "" {
		return SlotMigration{}, slotMigrating, false
	}
	return SlotMigration{Slot: slot, Peer: peer}, direction, true
}
