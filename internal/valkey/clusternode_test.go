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
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func TestParseClusterNodes(t *testing.T) {
	t.Run("primary and replica line", func(t *testing.T) {
		raw := "abc123 10.0.0.1:6379@16379 myself,master - 0 1700000000000 7 connected 0-5460 10923-16383\n" +
			"def456 10.0.0.2:6379@16379 slave abc123 0 1700000000001 7 connected\n"

		nodes := ParseClusterNodes(raw)
		if len(nodes) != 2 {
			t.Fatalf("expected 2 nodes, got %d", len(nodes))
		}

		primary := nodes[0]
		if primary.Id != "abc123" {
			t.Errorf("expected id abc123, got %q", primary.Id)
		}
		if primary.Host != "10.0.0.1" {
			t.Errorf("expected host 10.0.0.1, got %q", primary.Host)
		}
		if primary.Flags != FlagMyself|FlagMaster {
			t.Errorf("unexpected flags %09b", primary.Flags)
		}
		if primary.PrimaryId != "-" {
			t.Errorf("expected primary id -, got %q", primary.PrimaryId)
		}
		if want := []SlotsRange{{0, 5460}, {10923, 16383}}; !reflect.DeepEqual(primary.Slots, want) {
			t.Errorf("expected slots %v, got %v", want, primary.Slots)
		}
		if !primary.IsMyself() || !primary.IsPrimary() {
			t.Error("expected myself and master flags to be reported")
		}

		replica := nodes[1]
		if replica.PrimaryId != "abc123" {
			t.Errorf("expected replica to point at abc123, got %q", replica.PrimaryId)
		}
		if replica.IsPrimary() {
			t.Error("replica should not report as primary")
		}
		if len(replica.Slots) != 0 {
			t.Errorf("expected no slots, got %v", replica.Slots)
		}
	})

	t.Run("skips short and empty lines", func(t *testing.T) {
		raw := "\nabc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-16383\ntruncated line\n" +
			"def456 10.0.0.2:6379@16379 master - 0 0 1\n\n"
		if nodes := ParseClusterNodes(raw); len(nodes) != 1 {
			t.Fatalf("expected 1 node, got %d", len(nodes))
		}
	})

	t.Run("empty output", func(t *testing.T) {
		if nodes := ParseClusterNodes(""); len(nodes) != 0 {
			t.Errorf("expected no nodes, got %v", nodes)
		}
	})

	t.Run("single slot field", func(t *testing.T) {
		raw := "abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 42\n"
		nodes := ParseClusterNodes(raw)
		if want := []SlotsRange{{42, 42}}; !reflect.DeepEqual(nodes[0].Slots, want) {
			t.Errorf("expected %v, got %v", want, nodes[0].Slots)
		}
	})

	t.Run("hostname in endpoint", func(t *testing.T) {
		raw := "abc123 10.0.0.1:6379@16379,node-0.svc myself,master - 0 0 1 connected 0-16383\n"
		nodes := ParseClusterNodes(raw)
		if nodes[0].Host != "10.0.0.1" {
			t.Errorf("expected host 10.0.0.1, got %q", nodes[0].Host)
		}
	})

	// Valkey writes the endpoint as "%s:%i@%i" with a bare IP, so an IPv6
	// address arrives unbracketed with its own colons before the port.
	t.Run("ipv6 endpoint", func(t *testing.T) {
		raw := "abc123 fd00::2:6379@16379 myself,master - 0 0 1 connected 0-16383\n"
		nodes := ParseClusterNodes(raw)
		if nodes[0].Host != "fd00::2" {
			t.Errorf("expected host fd00::2, got %q", nodes[0].Host)
		}
	})

	// Bracketed IPv6 is not what Valkey emits, but nodes.conf and older
	// entries might carry it, so the brackets are stripped.
	t.Run("bracketed ipv6 endpoint", func(t *testing.T) {
		raw := "abc123 [fd00::2]:6379@16379 myself,master - 0 0 1 connected 0-16383\n"
		nodes := ParseClusterNodes(raw)
		if nodes[0].Host != "fd00::2" {
			t.Errorf("expected host fd00::2, got %q", nodes[0].Host)
		}
	})

	t.Run("noaddr entry has no host", func(t *testing.T) {
		raw := "dead1 :0@0 master,noaddr - 0 0 0 disconnected\n"
		nodes := ParseClusterNodes(raw)
		if nodes[0].Host != "" {
			t.Errorf("expected empty host, got %q", nodes[0].Host)
		}
		if !nodes[0].HasNoAddress() {
			t.Error("expected noaddr flag")
		}
		if nodes[0].IsFailing() {
			t.Error("noaddr alone should not report as failing")
		}
	})

	t.Run("nofailover flag", func(t *testing.T) {
		raw := "r1 10.0.0.2:6379@16379 myself,slave,nofailover p1 0 0 1 connected\n" +
			"p1 10.0.0.1:6379@16379 master,fail - 0 0 1 disconnected\n"
		nodes := ParseClusterNodes(raw)
		if !nodes[0].IsAutomaticFailoverDisabled() {
			t.Error("expected nofailover flag")
		}
		if nodes[1].IsAutomaticFailoverDisabled() {
			t.Error("a fail flag should not report as nofailover")
		}
	})
}

// The result must not cost more memory than its entries need: sized once, bounded
// for a reply that is not a table, and not pointing into the reply.
func TestParseClusterNodes_Memory(t *testing.T) {
	t.Run("result slice is sized once", func(t *testing.T) {
		var raw strings.Builder
		for i := range 200 {
			fmt.Fprintf(&raw, "id%03d 10.0.0.%d:6379@16379 slave p1 0 0 1 connected\n", i, i)
		}
		nodes := ParseClusterNodes(raw.String())
		if len(nodes) != 200 {
			t.Fatalf("expected 200 nodes, got %d", len(nodes))
		}
		// The trailing newline can leave one spare entry; growth by append ends
		// with dozens.
		if cap(nodes) > len(nodes)+1 {
			t.Errorf("expected capacity close to %d, got %d", len(nodes), cap(nodes))
		}
	})

	// The reserve comes from the number of newlines, so a reply that is mostly
	// newlines must not size it.
	t.Run("reserve is bounded for a reply that is not a table", func(t *testing.T) {
		nodes := ParseClusterNodes(strings.Repeat("\n", 1<<20))
		if len(nodes) != 0 {
			t.Fatalf("expected no nodes, got %d", len(nodes))
		}
		if cap(nodes) > clusterNodesPreallocMax {
			t.Errorf("expected capacity at most %d, got %d", clusterNodesPreallocMax, cap(nodes))
		}
	})

	t.Run("a table larger than the reserve still parses", func(t *testing.T) {
		var raw strings.Builder
		for i := range clusterNodesPreallocMax + 10 {
			fmt.Fprintf(&raw, "id%05d 10.0.0.1:6379@16379 slave p1 0 0 1 connected\n", i)
		}
		if got := len(ParseClusterNodes(raw.String())); got != clusterNodesPreallocMax+10 {
			t.Errorf("expected %d nodes, got %d", clusterNodesPreallocMax+10, got)
		}
	})

	// A node keeps its table for the whole scrape, so an entry that points into
	// the reply would keep the reply alive too.
	t.Run("entries do not point into the reply", func(t *testing.T) {
		raw := strings.Clone("abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-9 [5461->-def456] [5462-<-ghi789]\n" +
			"def456 10.0.0.2:6379@16379 slave abc123 0 0 1 connected\n")
		start := uintptr(unsafe.Pointer(unsafe.StringData(raw)))
		end := start + uintptr(len(raw))
		inReply := func(s string) bool {
			p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
			return s != "" && p >= start && p < end
		}
		for _, node := range ParseClusterNodes(raw) {
			texts := []string{node.Id, node.Host, node.PrimaryId}
			for _, m := range node.Migrating {
				texts = append(texts, m.Peer)
			}
			for _, m := range node.Importing {
				texts = append(texts, m.Peer)
			}
			for _, s := range texts {
				if inReply(s) {
					t.Errorf("%q points into the reply", s)
				}
			}
		}
	})
}

// Owned slots and in-flight markers must stay in separate fields: conflating
// them is the ambiguity this parser exists to remove.
func TestParseClusterNodes_MigrationMarkers(t *testing.T) {
	t.Run("owned range alongside both markers", func(t *testing.T) {
		raw := "abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460 [5461->-def456] [5462-<-ghi789]\n"
		node := ParseClusterNodes(raw)[0]

		if want := []SlotsRange{{0, 5460}}; !reflect.DeepEqual(node.Slots, want) {
			t.Errorf("expected owned %v, got %v", want, node.Slots)
		}
		if want := []SlotMigration{{Slot: 5461, Peer: "def456"}}; !reflect.DeepEqual(node.Migrating, want) {
			t.Errorf("expected migrating %v, got %v", want, node.Migrating)
		}
		if want := []SlotMigration{{Slot: 5462, Peer: "ghi789"}}; !reflect.DeepEqual(node.Importing, want) {
			t.Errorf("expected importing %v, got %v", want, node.Importing)
		}
	})

	// A primary holding only a marker is mid-reshard: it owns no range but is
	// already part of the slot map, so it must not look like a pending node.
	t.Run("marker only owns no slots but has an assignment", func(t *testing.T) {
		raw := "abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected [5461-<-def456]\n"
		node := ParseClusterNodes(raw)[0]

		if len(node.Slots) != 0 {
			t.Errorf("expected no owned slots, got %v", node.Slots)
		}
		if len(node.Importing) != 1 {
			t.Fatalf("expected 1 importing marker, got %v", node.Importing)
		}
		if !node.HasSlotAssignment() {
			t.Error("a marker-only primary should report a slot assignment")
		}
	})

	// A primary with no slot field at all has not joined the slot map.
	t.Run("no slot fields has no assignment", func(t *testing.T) {
		raw := "abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected\n"
		node := ParseClusterNodes(raw)[0]
		if node.HasSlotAssignment() {
			t.Error("expected no slot assignment")
		}
	})

	t.Run("malformed markers are not treated as slots", func(t *testing.T) {
		raw := "abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460 [notaslot->-def456] [5461->-]\n"
		node := ParseClusterNodes(raw)[0]

		if want := []SlotsRange{{0, 5460}}; !reflect.DeepEqual(node.Slots, want) {
			t.Errorf("expected owned slots %v, got %v", want, node.Slots)
		}
		if len(node.Migrating) != 0 || len(node.Importing) != 0 {
			t.Errorf("expected no markers, got migrating=%v importing=%v", node.Migrating, node.Importing)
		}
	})

	// A range that does not parse drops the owned ranges of its line only; the
	// markers on it and the other lines stay.
	t.Run("unparsable range drops the owned slots and keeps the markers", func(t *testing.T) {
		raw := "abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-5460 1-2-3 [5461->-def456]\n" +
			"def456 10.0.0.2:6379@16379 master - 0 0 1 connected 6000-7000\n"
		nodes := ParseClusterNodes(raw)

		if nodes[0].Slots != nil {
			t.Errorf("expected no owned slots, got %v", nodes[0].Slots)
		}
		if want := []SlotMigration{{Slot: 5461, Peer: "def456"}}; !reflect.DeepEqual(nodes[0].Migrating, want) {
			t.Errorf("expected migrating %v, got %v", want, nodes[0].Migrating)
		}
		if want := []SlotsRange{{6000, 7000}}; !reflect.DeepEqual(nodes[1].Slots, want) {
			t.Errorf("expected the next line's slots %v, got %v", want, nodes[1].Slots)
		}
	})
}

func TestParseClusterNodes_Whitespace(t *testing.T) {
	raw := "  abc123\t10.0.0.1:6379@16379  myself,master -  0 0 1 connected   0-9 \t10-19\r\n" +
		"short line\n" +
		"def456 10.0.0.2:6379@16379 slave abc123 0 0 1 connected\n"
	nodes := ParseClusterNodes(raw)

	if len(nodes) != 2 {
		t.Fatalf("expected the short line to be skipped, got %d nodes", len(nodes))
	}
	if nodes[0].Id != "abc123" || nodes[0].PrimaryId != "-" {
		t.Errorf("unexpected fields: id %q, primary %q", nodes[0].Id, nodes[0].PrimaryId)
	}
	if want := []SlotsRange{{0, 9}, {10, 19}}; !reflect.DeepEqual(nodes[0].Slots, want) {
		t.Errorf("expected slots %v, got %v", want, nodes[0].Slots)
	}
}

// The same member is described differently by each viewer. This is the property
// a single per-node type could not express, and it is why the parser returns one
// entry per line rather than a single node view.
func TestParseClusterNodes_ViewersDisagree(t *testing.T) {
	// aaa still has bbb at its old address, flagged fail.
	fromA := "aaa 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
		"bbb 10.0.0.99:6379@16379 master,fail - 0 0 2 disconnected 8192-16383\n"
	// bbb reports itself healthy at its new address.
	fromB := "bbb 10.0.0.2:6379@16379 myself,master - 0 0 2 connected 8192-16383\n" +
		"aaa 10.0.0.1:6379@16379 master - 0 0 1 connected 0-8191\n"

	find := func(raw, id string) *ClusterNode {
		for _, entry := range ParseClusterNodes(raw) {
			if entry.Id == id {
				return &entry
			}
		}
		return nil
	}

	bSeenByA := find(fromA, "bbb")
	if bSeenByA == nil {
		t.Fatal("expected aaa to know bbb")
	}
	if !bSeenByA.IsFailing() || bSeenByA.Host != "10.0.0.99" {
		t.Errorf("aaa should see bbb failing at the stale address, got %+v", bSeenByA)
	}

	bSeenByB := find(fromB, "bbb")
	if bSeenByB == nil {
		t.Fatal("expected bbb to know itself")
	}
	if bSeenByB.IsFailing() || bSeenByB.Host != "10.0.0.2" {
		t.Errorf("bbb should see itself healthy at the new address, got %+v", bSeenByB)
	}

	// Only the viewer's own line carries myself.
	if !bSeenByB.IsMyself() || bSeenByA.IsMyself() {
		t.Error("myself should be set only on the line describing the viewer")
	}
}

func TestClusterNodeIsConnected(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"healthy", "peer 10.0.0.2:6379@16379 master - 0 0 1 connected", true},
		{"link disconnected", "peer 10.0.0.2:6379@16379 master - 0 0 1 disconnected", false},
		{"pfail", "peer 10.0.0.2:6379@16379 master,fail? - 0 0 1 connected", false},
		{"failed", "peer 10.0.0.2:6379@16379 master,fail - 0 0 1 connected", false},
		{"handshake", "peer 10.0.0.2:6379@16379 handshake - 0 0 1 connected", false},
		{"no address", "peer :0@0 master,noaddr - 0 0 1 connected", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := ParseClusterNodes(tc.line)
			if len(nodes) != 1 {
				t.Fatalf("expected one node, got %d", len(nodes))
			}
			if got := nodes[0].IsConnected(); got != tc.want {
				t.Errorf("IsConnected() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFindMyself(t *testing.T) {
	t.Run("finds the myself line", func(t *testing.T) {
		raw := "def456 10.0.0.2:6379@16379 master - 0 0 1 connected 0-5460\n" +
			"abc123 10.0.0.1:6379@16379 myself,slave def456 0 0 1 connected\n"
		myself := FindMyself(ParseClusterNodes(raw))
		if myself == nil {
			t.Fatal("expected a myself entry")
			return
		}
		if myself.Id != "abc123" {
			t.Errorf("expected abc123, got %q", myself.Id)
		}
	})

	t.Run("no myself line", func(t *testing.T) {
		raw := "def456 10.0.0.2:6379@16379 master - 0 0 1 connected 0-5460\n"
		if myself := FindMyself(ParseClusterNodes(raw)); myself != nil {
			t.Errorf("expected nil, got %v", myself)
		}
	})
}

// ParseClusterNodesMyself must agree with FindMyself over the full table.
func TestParseClusterNodesMyself(t *testing.T) {
	const (
		primary = "def456 10.0.0.2:6379@16379 master - 0 0 1 connected 0-5460 [5461->-ghi789]\n"
		replica = "ghi789 10.0.0.3:6379@16379 slave def456 0 0 1 connected\n"
		viewer  = "abc123 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 5461-10922\n"
		// "myself" in the hostname must not match on its own.
		decoy = "jkl012 10.0.0.4:6379@16379,myself-1.example slave def456 0 0 1 connected\n"
	)
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"myself first", viewer + primary + replica},
		{"myself in the middle", primary + viewer + replica},
		{"myself last without a newline", primary + replica + strings.TrimSuffix(viewer, "\n")},
		{"hostname containing myself before the viewer", decoy + viewer},
		{"hostname containing myself and no viewer", primary + decoy},
		{"no myself line", primary + replica},
		{"truncated myself line", primary + "abc123 10.0.0.1:6379@16379 myself,master -\n"},
		{"empty output", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseClusterNodesMyself(tc.raw)
			want := FindMyself(ParseClusterNodes(tc.raw))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("expected %+v, got %+v", want, got)
			}
		})
	}
}

func BenchmarkParseClusterNodesMyself(b *testing.B) {
	var raw strings.Builder
	for i := range 200 {
		flags := "slave"
		if i == 100 {
			flags = "myself,master"
		}
		fmt.Fprintf(&raw, "%040d 10.0.%d.%d:6379@16379,valkey-%d.example %s - 0 0 1 connected\n", i, i/250, i%250, i, flags)
	}
	b.Run("full table", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = FindMyself(ParseClusterNodes(raw.String()))
		}
	})
	b.Run("myself only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = ParseClusterNodesMyself(raw.String())
		}
	})
}

func TestParseNodeFlags(t *testing.T) {
	t.Run("every flag Valkey writes", func(t *testing.T) {
		var want NodeFlags
		for _, nf := range nodeFlagNames {
			if got := parseNodeFlags(nf.name); got != nf.flag {
				t.Errorf("parseNodeFlags(%q) = %09b, want %09b", nf.name, got, nf.flag)
			}
			want |= nf.flag
		}
		all := "myself,master,slave,fail?,fail,handshake,noaddr,nofailover,noflags"
		if got := parseNodeFlags(all); got != want {
			t.Errorf("parseNodeFlags(%q) = %09b, want %09b", all, got, want)
		}
	})

	// fail? and fail are distinct flags; the majority check reads them apart.
	t.Run("pfail is not fail", func(t *testing.T) {
		node := ClusterNode{Flags: parseNodeFlags("master,fail?")}
		if !node.HasFlag(FlagPFail) || node.HasFlag(FlagFail) {
			t.Errorf("got %09b, want fail? without fail", node.Flags)
		}
	})

	t.Run("an unknown name is dropped", func(t *testing.T) {
		got := parseNodeFlags("slave,future-flag,nofailover")
		if want := FlagSlave | FlagNoFailover; got != want {
			t.Errorf("got %09b, want %09b", got, want)
		}
	})
}

func TestNodeFlagsString(t *testing.T) {
	for _, tc := range []struct {
		flags NodeFlags
		want  string
	}{
		{0, ""},
		{FlagMaster, "master"},
		{FlagMyself | FlagMaster, "myself,master"},
		{FlagSlave | FlagPFail | FlagNoFailover, "slave,fail?,nofailover"},
		// The output follows Valkey's order, not the order the bits are combined in.
		{FlagNoFailover | FlagSlave | FlagMyself, "myself,slave,nofailover"},
	} {
		if got := tc.flags.String(); got != tc.want {
			t.Errorf("NodeFlags(%09b).String() = %q, want %q", uint16(tc.flags), got, tc.want)
		}
	}
}

func TestNodeFlagsHas(t *testing.T) {
	f := FlagSlave | FlagFail
	if !f.Has(FlagFail) || !f.Has(FlagMaster|FlagSlave) {
		t.Errorf("%v: want fail and one of master|slave", f)
	}
	if f.Has(FlagMaster) || f.Has(0) {
		t.Errorf("%v: want neither master nor the empty set", f)
	}
}
