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
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	vclient "github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-operator/internal/valkey"
)

// The scripted RESP helpers below mirror the migration-state harness used by
// the internal/valkey tests. They live in this file until the migration-
// failure surfacing PR lands; keep the two copies in sync.

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

// stateEntry builds one GETSLOTMIGRATIONS entry: flat field/value pairs,
// numeric fields as RESP integers, matching the server wire format.
func stateEntry(state, slotRanges, message string, lastUpdate time.Time) []string {
	return []string{
		"name", "5371b28997de6fd0bbe813ad8ebdfdf2faadb308",
		"operation", "EXPORT",
		"slot_ranges", slotRanges,
		"target_node", "4b4f12fdfb58d5e30fef7b9ad3f1651dacbbaba9",
		"source_node", "93941e777e17fcbc92d4398cc957ffea888f472b",
		"state", state,
		"message", message,
		"last_update_time", strconv.FormatInt(lastUpdate.Unix(), 10),
	}
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
