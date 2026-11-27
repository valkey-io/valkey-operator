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

package valkey

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	vclient "github.com/valkey-io/valkey-go"
)

// migrationServer is a scripted RESP server: it completes the valkey-go
// client init handshake (HELLO 3 and friends) and then answers
// CLUSTER GETSLOTMIGRATIONS with the entries produced by the reply
// callback, recording every command it receives. Each entry is a RESP3 map,
// the form the server sends the operator after HELLO 3.
type migrationServer struct {
	listener net.Listener
	commands chan []string
	addr     string
	reply    func() [][]string

	mu      sync.Mutex
	clock   func() time.Time
	timeErr bool
}

// setClock sets the time the server answers TIME with.
func (s *migrationServer) setClock(clock func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = clock
}

// failTime makes the server answer TIME with an error.
func (s *migrationServer) failTime() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeErr = true
}

func newMigrationServer(t *testing.T, reply func() [][]string) *migrationServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &migrationServer{
		listener: ln,
		commands: make(chan []string, 64),
		addr:     ln.Addr().String(),
		reply:    reply,
		clock:    time.Now,
	}
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

// handle reads commands one at a time and answers each, keeping the
// connection open.
func (s *migrationServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		cmd, err := readCommand(r)
		if err != nil {
			return
		}
		select {
		case s.commands <- cmd:
		default:
		}
		if _, err := w.WriteString(s.respond(cmd)); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// respond answers a single command with a RESP fragment.
func (s *migrationServer) respond(cmd []string) string {
	switch {
	case strings.EqualFold(cmd[0], "HELLO"):
		// Minimal RESP3 map: proto >= 3 keeps the client on the RESP 3
		// path. Fields mirror what a real server returns.
		return "%6\r\n$5\r\nproto\r\n:3\r\n$7\r\nversion\r\n$5\r\n9.0.0\r\n" +
			"$4\r\nrole\r\n$6\r\nmaster\r\n$2\r\nid\r\n$40\r\n" +
			"0123456789012345678901234567890123456789\r\n" +
			"$4\r\nmode\r\n$10\r\nstandalone\r\n$7\r\nmodules\r\n*0\r\n"
	case len(cmd) > 1 && strings.EqualFold(cmd[0], "CLUSTER") && strings.EqualFold(cmd[1], "GETSLOTMIGRATIONS"):
		entries := s.reply()
		var b strings.Builder
		fmt.Fprintf(&b, "*%d\r\n", len(entries))
		for _, entry := range entries {
			// Over RESP3 the server sends each entry as a map.
			fmt.Fprintf(&b, "%%%d\r\n", len(entry)/2)
			for _, field := range entry {
				if _, err := strconv.Atoi(field); err == nil {
					fmt.Fprintf(&b, ":%s\r\n", field)
				} else {
					fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(field), field)
				}
			}
		}
		return b.String()
	case strings.EqualFold(cmd[0], "TIME"):
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.timeErr {
			return "-ERR scripted\r\n"
		}
		return timeReply(s.clock())
	default:
		return "+OK\r\n"
	}
}

// timeReply encodes t as a TIME reply: Unix seconds and microseconds as two
// bulk strings.
func timeReply(t time.Time) string {
	sec := strconv.FormatInt(t.Unix(), 10)
	usec := strconv.Itoa(t.Nanosecond() / 1000)
	return fmt.Sprintf("*2\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(sec), sec, len(usec), usec)
}

// readCommand reads one RESP array-of-bulk-strings request.
func readCommand(r *bufio.Reader) ([]string, error) {
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

// migrationNode builds a real valkey-go client against the scripted server
// and wraps it in the NodeState the migration helpers expect.
func migrationNode(t *testing.T, s *migrationServer) *NodeState {
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
	return &NodeState{Client: client, Address: host, Port: port}
}

// migTargetA and migTargetB are the node IDs used as migration
// destinations in the scripted GETSLOTMIGRATIONS entries below.
const (
	migTargetA = "4b4f12fdfb58d5e30fef7b9ad3f1651dacbbaba9"
	migTargetB = "c3b1e5a9f0d8472b91e6a3f5d2c8b0a4e7f91234"
)

// migrationEntry builds one GETSLOTMIGRATIONS entry as field/value pairs,
// with numeric fields as RESP integers.
func migrationEntry(state, targetNode, slotRanges, message string, lastUpdate time.Time) []string {
	entry := []string{
		"name", "5371b28997de6fd0bbe813ad8ebdfdf2faadb308",
		"operation", "EXPORT",
		"slot_ranges", slotRanges,
		"source_node", "93941e777e17fcbc92d4398cc957ffea888f472b",
		"create_time", strconv.FormatInt(lastUpdate.Add(-time.Minute).Unix(), 10),
		"last_update_time", strconv.FormatInt(lastUpdate.Unix(), 10),
		"last_ack_time", strconv.FormatInt(lastUpdate.Unix(), 10),
		"state", state,
		"message", message,
		"cow_size", "0",
		"remaining_repl_size", "0",
	}
	// The map parser is order-insensitive; an empty targetNode models a
	// server that does not report the field at all.
	if targetNode != "" {
		entry = append(entry, "target_node", targetNode)
	}
	return entry
}

func TestSlotMigrationState_FailedJobReported(t *testing.T) {
	failMsg := "Received error during handshake to target: -ERR Slots are being manually imported"
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", migTargetA, "0-399", failMsg, now.Add(-30*time.Second))}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if inProgress {
		t.Fatal("a failed job is terminal; it must not read as in progress")
	}
	if failed == nil {
		t.Fatal("the failed job overlapping the planned slots was not reported")
	}
	if failed.Message != failMsg {
		t.Errorf("message = %q, want %q", failed.Message, failMsg)
	}
	if failed.TargetNode != migTargetA {
		t.Errorf("target node = %q, want %q", failed.TargetNode, migTargetA)
	}
	if len(failed.Slots) != 1 || failed.Slots[0] != (SlotsRange{Start: 0, End: 399}) {
		t.Errorf("slots = %v, want [0-399]", failed.Slots)
	}
	if got, want := failed.LastUpdate.Unix(), now.Add(-30*time.Second).Unix(); got != want {
		t.Errorf("last update = %v, want %v", failed.LastUpdate, now.Add(-30*time.Second))
	}
}

// TestSlotMigrationState_MessageTrimmed: Valkey stores the target's error
// reply with its CRLF, which must not reach the condition message.
func TestSlotMigrationState_MessageTrimmed(t *testing.T) {
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", migTargetA, "0-399",
			"Received error during handshake to target: -ERR Slots are being manually imported\r\n", now.Add(-30*time.Second))}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	_, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if failed == nil {
		t.Fatal("the failed job was not reported")
	}
	if want := "Received error during handshake to target: -ERR Slots are being manually imported"; failed.Message != want {
		t.Errorf("message = %q, want %q", failed.Message, want)
	}
}

func TestSlotMigrationState_InProgressStillWorks(t *testing.T) {
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{
			migrationEntry("failed", migTargetA, "0-399", "old failure", now.Add(-30*time.Second)),
			migrationEntry("running", migTargetA, "400-799", "", now),
		}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if !inProgress {
		t.Fatal("a running job must read as in progress, regardless of failed history")
	}
	if failed != nil {
		t.Errorf("while a migration runs, no failure should be reported; got %+v", failed)
	}
}

func TestSlotMigrationState_NoOverlapNoReport(t *testing.T) {
	// A failed job left over from an unrelated manual migration covers
	// slots the operator is not planning to move. It must not surface.
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", migTargetA, "16000-16383", "manual reshard gone wrong", now.Add(-30*time.Second))}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if inProgress {
		t.Fatal("no running job")
	}
	if failed != nil {
		t.Fatalf("non-overlapping failed job was reported: %+v", failed)
	}
}

func TestSlotMigrationState_StaleFailureIgnored(t *testing.T) {
	// A failed job older than the window is history: the operator must
	// still be able to retry the move rather than wedging forever.
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", migTargetA, "0-399", "ancient failure", now.Add(-3*slotMigrationFailureWindow))}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if inProgress {
		t.Fatal("no running job")
	}
	if failed != nil {
		t.Fatalf("stale failed job was reported: %+v", failed)
	}
}

func TestSlotMigrationState_MostRecentFailureWins(t *testing.T) {
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{
			migrationEntry("failed", migTargetA, "0-399", "older failure", now.Add(-90*time.Second)),
			migrationEntry("failed", migTargetA, "0-399", "newer failure", now.Add(-30*time.Second)),
		}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	_, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if failed == nil {
		t.Fatal("expected the newer failed job to be reported")
	}
	if failed.Message != "newer failure" {
		t.Errorf("message = %q, want the newer job %q", failed.Message, "newer failure")
	}
}

func TestSlotMigrationState_NewestOfMixedTargets(t *testing.T) {
	// The drain case from the issue: the same batch fails against
	// several destinations. With no destination filter, the most recent
	// failure is the actionable one to report.
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{
			migrationEntry("failed", migTargetA, "0-399", "target A refused", now.Add(-60*time.Second)),
			migrationEntry("failed", migTargetB, "0-399", "target B refused", now.Add(-15*time.Second)),
		}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	_, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, "")
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if failed == nil || failed.Message != "target B refused" {
		t.Fatalf("expected the newest failure, got %+v", failed)
	}
}

func TestSlotMigrationState_ForeignTargetFiltered(t *testing.T) {
	// A failed S->A job must not block, or speak for, a later plan that
	// moves the same slots to B: when the caller names its planned
	// destination, the job is filtered out.
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", migTargetA, "0-399", "handshake refused by A", now.Add(-30*time.Second))}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	_, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetB)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if failed != nil {
		t.Fatalf("a failure for another destination must not be reported, got %+v", failed)
	}

	// The same job is still reported for the destination it failed on.
	_, failed, err = SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if failed == nil || failed.Message != "handshake refused by A" {
		t.Fatalf("the matching-destination failure must still be reported, got %+v", failed)
	}
}

func TestSlotMigrationState_UnknownTargetStillReported(t *testing.T) {
	// A server that does not report target_node cannot be filtered by
	// destination: such a failure still matches every destination, so
	// the retry loop cannot be silently unblocked.
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", "", "0-399", "target-less failure", now.Add(-30*time.Second))}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	_, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetB)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if failed == nil {
		t.Fatal("a failure with no reported destination must still be reported")
	}
	if failed.TargetNode != "" {
		t.Errorf("target node = %q, want empty", failed.TargetNode)
	}
}

func TestSlotMigrationState_SuccessAndEmptyHistory(t *testing.T) {
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("success", migTargetA, "0-399", "", now.Add(-time.Second))}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if inProgress || failed != nil {
		t.Fatalf("a successful job is neither in progress nor a failure; got inProgress=%v failed=%+v", inProgress, failed)
	}

	empty := newMigrationServer(t, func() [][]string { return nil })
	empty.setClock(func() time.Time { return now })
	emptyNode := migrationNode(t, empty)
	inProgress, failed, err = SlotMigrationState(context.Background(), emptyNode, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState empty: %v", err)
	}
	if inProgress || failed != nil {
		t.Fatalf("empty history; got inProgress=%v failed=%+v", inProgress, failed)
	}
}

func TestSlotMigrationState_ZeroTimestampNotRecent(t *testing.T) {
	// A malformed entry whose timestamp parses as the zero time must not
	// wedge the loop: it is never within the recency window, so the
	// operator keeps retrying the move instead of reporting garbage.
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", migTargetA, "0-399", "garbled entry", time.Unix(0, 0))}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if inProgress {
		t.Fatal("no running job")
	}
	if failed != nil {
		t.Fatalf("zero-timestamp failed job was treated as recent: %+v", failed)
	}
}

func TestParseSlotRanges(t *testing.T) {
	cases := []struct {
		in   string
		want []SlotsRange
	}{
		{"", nil},
		{"0-399", []SlotsRange{{Start: 0, End: 399}}},
		{"0-10 100-200", []SlotsRange{{Start: 0, End: 10}, {Start: 100, End: 200}}},
		{"866-866", []SlotsRange{{Start: 866, End: 866}}},
		{"garbage", nil},
		{"0-10 garbage 20-30", []SlotsRange{{Start: 0, End: 10}, {Start: 20, End: 30}}},
	}
	for _, tc := range cases {
		got := parseSlotRanges(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseSlotRanges(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSlotMigrationState_FutureTimestampStillReported(t *testing.T) {
	// A node whose clock runs ahead stamps last_update_time in the future.
	// The failure still happened, so it must still be reported on first
	// sight: now().Sub(LastUpdate) goes negative, which the window check
	// does not reject. The skew extends how long the entry is held by at
	// most the clock difference; it never wedges the reconcile forever
	// because real time keeps advancing past the window.
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", migTargetA, "0-399", "skewed clock failure", now.Add(10*time.Minute))}
	})
	s.setClock(func() time.Time { return now })
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if inProgress {
		t.Fatal("no running job")
	}
	if failed == nil {
		t.Fatal("a future-dated failed job must still be reported on first sight")
	}
	if failed.Message != "skewed clock failure" {
		t.Errorf("message = %q, want %q", failed.Message, "skewed clock failure")
	}
}

// TestSlotMigrationState_NodeClockBehind: the node stamps last_update_time
// with its own clock. A node three minutes behind the operator must still
// report a failure from ten seconds ago.
func TestSlotMigrationState_NodeClockBehind(t *testing.T) {
	nodeNow := time.Now().Add(-3 * time.Minute)
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", migTargetA, "0-399", "handshake failed", nodeNow.Add(-10*time.Second))}
	})
	s.setClock(func() time.Time { return nodeNow })
	node := migrationNode(t, s)

	_, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if failed == nil {
		t.Fatal("a failure ten seconds old on the node's clock was not reported")
	}
}

// TestSlotMigrationState_TimeErrorFallsBackToOperatorClock: a node whose
// aclfile predates the +time grant denies TIME. Failed jobs then age on the
// operator's clock: a recent one is reported with its own message, and an
// old one no longer holds the move, so the retry is not blocked forever.
func TestSlotMigrationState_TimeErrorFallsBackToOperatorClock(t *testing.T) {
	for name, tc := range map[string]struct {
		age      time.Duration
		reported bool
	}{
		"recent": {30 * time.Second, true},
		"old":    {10 * time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := newMigrationServer(t, func() [][]string {
				return [][]string{migrationEntry("failed", migTargetA, "0-399", "handshake failed", time.Now().Add(-tc.age))}
			})
			s.failTime()
			node := migrationNode(t, s)

			_, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
			if err != nil {
				t.Fatalf("a TIME error must not block the move, got %v", err)
			}
			if got := failed != nil; got != tc.reported {
				t.Fatalf("reported = %v, want %v (%+v)", got, tc.reported, failed)
			}
			if failed != nil && failed.Message != "handshake failed" {
				t.Errorf("message = %q, want the job's own", failed.Message)
			}
		})
	}
}

// TestSlotMigrationState_TimeErrorIgnoredWithoutFailure: TIME is only needed
// to age a matching failed job. A node that denies TIME (an aclfile from
// before the operator granted it) must still report a running job, and no
// failure when there is none to age.
func TestSlotMigrationState_TimeErrorIgnoredWithoutFailure(t *testing.T) {
	for name, entries := range map[string][][]string{
		"running":      {migrationEntry("failed", migTargetA, "0-399", "handshake failed", time.Now()), migrationEntry("running", migTargetA, "0-399", "", time.Now())},
		"none":         nil,
		"other target": {migrationEntry("failed", migTargetB, "0-399", "handshake failed", time.Now())},
		"other slots":  {migrationEntry("failed", migTargetA, "8000-8399", "handshake failed", time.Now())},
	} {
		t.Run(name, func(t *testing.T) {
			s := newMigrationServer(t, func() [][]string { return entries })
			s.failTime()
			node := migrationNode(t, s)

			inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, migTargetA)
			if err != nil {
				t.Fatalf("TIME is not needed here, got %v", err)
			}
			if failed != nil {
				t.Fatalf("no matching failure, got %+v", failed)
			}
			if want := name == "running"; inProgress != want {
				t.Fatalf("inProgress = %v, want %v", inProgress, want)
			}
		})
	}
}
