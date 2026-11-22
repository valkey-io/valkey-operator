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
	"strconv"
	"strings"
	"unique"
)

// clusterNodesMinFields is the number of fields every CLUSTER NODES line has
// before the optional slot fields:
//
//	<id> <ip:port@cport[,hostname]> <flags> <primary> <ping-sent> <pong-recv> <config-epoch> <link-state>
//
// Slot fields, when present, start at index clusterNodesMinFields.
const clusterNodesMinFields = 8

// clusterNodesPreallocMax caps the entries reserved up front, so a malicious
// reply cannot force a large allocation.
const clusterNodesPreallocMax = 4096

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
	Flags     NodeFlags
	PrimaryId string // "-" on a primary line, the primary's ID on a replica
	Slots     []SlotsRange
	Migrating []SlotMigration
	Importing []SlotMigration
}

// NodeFlags is the set of flags on a CLUSTER NODES line.
type NodeFlags uint16

const (
	FlagMyself NodeFlags = 1 << iota
	FlagMaster
	FlagSlave
	FlagPFail
	FlagFail
	FlagHandshake
	FlagNoAddr
	FlagNoFailover
	FlagNoFlags
)

var nodeFlagNames = [...]struct {
	flag NodeFlags
	name string
}{
	{FlagMyself, "myself"},
	{FlagMaster, "master"},
	{FlagSlave, "slave"},
	{FlagPFail, "fail?"},
	{FlagFail, "fail"},
	{FlagHandshake, "handshake"},
	{FlagNoAddr, "noaddr"},
	{FlagNoFailover, "nofailover"},
	{FlagNoFlags, "noflags"},
}

// flagNamed returns the flag with the given wire name, or 0 for a name Valkey
// does not write.
func flagNamed(name string) NodeFlags {
	for _, f := range nodeFlagNames {
		if f.name == name {
			return f.flag
		}
	}
	return 0
}

// parseNodeFlags reads the comma-separated flags field. A name outside the set
// above is dropped; no caller reads one.
func parseNodeFlags(s string) NodeFlags {
	var flags NodeFlags
	for s != "" {
		var name string
		name, s, _ = strings.Cut(s, ",")
		flags |= flagNamed(name)
	}
	return flags
}

// HasFlag reports whether the line carries flag.
func (c *ClusterNode) HasFlag(flag NodeFlags) bool {
	return c.Flags&flag != 0
}

// IsMyself reports whether this line describes the node that produced the
// output.
func (c *ClusterNode) IsMyself() bool {
	return c.HasFlag(FlagMyself)
}

// IsPrimary reports whether this line describes a primary.
func (c *ClusterNode) IsPrimary() bool {
	return c.HasFlag(FlagMaster)
}

// IsFailing reports whether the viewer considers this node down. "fail?"
// (pfail) counts: promoting pfail to fail needs gossip between a majority of
// primaries, so an entry can stay at fail? indefinitely once that majority is
// gone.
func (c *ClusterNode) IsFailing() bool {
	return c.HasFlag(FlagFail) || c.HasFlag(FlagPFail)
}

// HasNoAddress reports whether the viewer knows this node's ID but has no
// address for it.
func (c *ClusterNode) HasNoAddress() bool {
	return c.HasFlag(FlagNoAddr)
}

// IsAutomaticFailoverDisabled reports whether the node carries the nofailover
// flag, set for example by cluster-replica-no-failover. Such a replica does not
// promote itself, but still accepts CLUSTER FAILOVER TAKEOVER.
func (c *ClusterNode) IsAutomaticFailoverDisabled() bool {
	return c.HasFlag(FlagNoFailover)
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

// ParseClusterNodesMyself returns the entry flagged "myself" in CLUSTER NODES
// output, or nil when none carries it. Only lines containing "myself" are
// parsed, so a caller that needs the viewer alone does not pay for the table.
func ParseClusterNodesMyself(raw string) *ClusterNode {
	for line := range strings.SplitSeq(raw, "\n") {
		if !strings.Contains(line, "myself") {
			continue
		}
		// The substring can also sit in an id or hostname; the flag decides.
		if node, ok := parseClusterNodesLine(line); ok && node.IsMyself() {
			return &node
		}
	}
	return nil
}

// ParseClusterNodes parses CLUSTER NODES output into one entry per node.
// Lines with fewer than clusterNodesMinFields fields are skipped, which covers
// the trailing empty line and any partial output.
func ParseClusterNodes(raw string) []ClusterNode {
	// One entry per line at most. Growing the slice by append would allocate
	// about twice the final size on every one of the scrape's node tables. A
	// table beyond the cap still parses; the slice grows by append past it.
	nodes := make([]ClusterNode, 0, min(strings.Count(raw, "\n")+1, clusterNodesPreallocMax))
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
	// The leading fields are read in place; the slot fields that follow are
	// handled one at a time, so no slice of fields is built.
	var head [clusterNodesMinFields]string
	rest := line
	for i := range head {
		head[i], rest = nextField(rest)
		if head[i] == "" {
			return ClusterNode{}, false
		}
	}

	node := ClusterNode{
		Id:        shared(head[0]),
		Host:      shared(hostFromClusterNodesEndpoint(head[1])),
		Flags:     parseNodeFlags(head[2]),
		PrimaryId: shared(head[3]),
	}

	// Owned ranges and migration markers are interleaved in the same trailing
	// fields. A range that does not parse drops the owned ranges of the line
	// and keeps the markers. A "[" field that is not a well-formed marker is
	// skipped.
	ranges := []SlotsRange{}
	rangesOK := true
	for {
		var field string
		field, rest = nextField(rest)
		if field == "" {
			break
		}
		migration, direction, ok := parseSlotMigration(field)
		switch {
		case !ok:
			if !rangesOK || strings.HasPrefix(field, "[") {
				continue
			}
			r, err := parseSlotsRange(field)
			if err != nil {
				rangesOK = false
				continue
			}
			ranges = append(ranges, r)
		case direction == slotMigrating:
			node.Migrating = append(node.Migrating, migration)
		default:
			node.Importing = append(node.Importing, migration)
		}
	}
	if rangesOK {
		node.Slots = ranges
	}

	return node, true
}

// nextField returns the first field of s, which is delimited by ASCII
// whitespace, and the text after it. The field is empty when s has none left.
func nextField(s string) (field, rest string) {
	start := 0
	for start < len(s) && isASCIISpace(s[start]) {
		start++
	}
	end := start
	for end < len(s) && !isASCIISpace(s[end]) {
		end++
	}
	return s[start:end], s[end:]
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
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
	return SlotMigration{Slot: slot, Peer: shared(peer)}, direction, true
}

// shared returns s as a copy that is not part of the reply it was cut from.
// Equal values share one copy until the next garbage collection, because unique
// forgets a value once no Handle to it is held. Every scraped node parses a
// table that lists all the nodes, so the ids and hosts repeat in each of them.
// Cutting them from the reply would keep each reply alive, and copying them per
// entry would keep one copy per node and table.
func shared(s string) string {
	return unique.Make(s).Value()
}
