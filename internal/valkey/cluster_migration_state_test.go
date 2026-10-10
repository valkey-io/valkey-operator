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
	"testing"
	"time"

	vclient "github.com/valkey-io/valkey-go"
)

// migrationServer is a scripted RESP server: it completes the valkey-go
// client init handshake (HELLO 3 and friends) and then answers
// CLUSTER GETSLOTMIGRATIONS with the entries produced by the reply
// callback, recording every command it receives. Entries use the flat
// field/value array form CLUSTER GETSLOTMIGRATIONS returns, so both RESP 3
// maps and flat arrays parse through AsStrMap, matching production.
type migrationServer struct {
	listener net.Listener
	commands chan []string
	addr     string
	reply    func() [][]string
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

// migrationEntry builds one GETSLOTMIGRATIONS entry: field/value pairs in
// the flat array form, with numeric fields as RESP integers.
func migrationEntry(state, slotRanges, message string, lastUpdate time.Time) []string {
	return []string{
		"name", "5371b28997de6fd0bbe813ad8ebdfdf2faadb308",
		"operation", "EXPORT",
		"slot_ranges", slotRanges,
		"target_node", "4b4f12fdfb58d5e30fef7b9ad3f1651dacbbaba9",
		"source_node", "93941e777e17fcbc92d4398cc957ffea888f472b",
		"create_time", strconv.FormatInt(lastUpdate.Add(-time.Minute).Unix(), 10),
		"last_update_time", strconv.FormatInt(lastUpdate.Unix(), 10),
		"last_ack_time", strconv.FormatInt(lastUpdate.Unix(), 10),
		"state", state,
		"message", message,
		"cow_size", "0",
		"remaining_repl_size", "0",
	}
}

func TestSlotMigrationState_FailedJobReported(t *testing.T) {
	failMsg := "Received error during handshake to target: -ERR Slots are being manually imported"
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("failed", "0-399", failMsg, now.Add(-30*time.Second))}
	})
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
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
	if len(failed.Slots) != 1 || failed.Slots[0] != (SlotsRange{Start: 0, End: 399}) {
		t.Errorf("slots = %v, want [0-399]", failed.Slots)
	}
	if got, want := failed.LastUpdate.Unix(), now.Add(-30*time.Second).Unix(); got != want {
		t.Errorf("last update = %v, want %v", failed.LastUpdate, now.Add(-30*time.Second))
	}
}

func TestSlotMigrationState_InProgressStillWorks(t *testing.T) {
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{
			migrationEntry("failed", "0-399", "old failure", now.Add(-30*time.Second)),
			migrationEntry("running", "400-799", "", now),
		}
	})
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
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
		return [][]string{migrationEntry("failed", "16000-16383", "manual reshard gone wrong", now.Add(-30*time.Second))}
	})
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
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
		return [][]string{migrationEntry("failed", "0-399", "ancient failure", now.Add(-3*slotMigrationFailureWindow))}
	})
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
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
			migrationEntry("failed", "0-399", "older failure", now.Add(-90*time.Second)),
			migrationEntry("failed", "0-399", "newer failure", now.Add(-30*time.Second)),
		}
	})
	node := migrationNode(t, s)

	_, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
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
	// several destinations. The most recent failure is the actionable
	// one to report.
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{
			migrationEntry("failed", "0-399", "target A refused", now.Add(-60*time.Second)),
			migrationEntry("failed", "0-399", "target B refused", now.Add(-15*time.Second)),
		}
	})
	node := migrationNode(t, s)

	_, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if failed == nil || failed.Message != "target B refused" {
		t.Fatalf("expected the newest failure, got %+v", failed)
	}
}

func TestSlotMigrationState_SuccessAndEmptyHistory(t *testing.T) {
	now := time.Now()
	s := newMigrationServer(t, func() [][]string {
		return [][]string{migrationEntry("success", "0-399", "", now.Add(-time.Second))}
	})
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("SlotMigrationState: %v", err)
	}
	if inProgress || failed != nil {
		t.Fatalf("a successful job is neither in progress nor a failure; got inProgress=%v failed=%+v", inProgress, failed)
	}

	empty := newMigrationServer(t, func() [][]string { return nil })
	emptyNode := migrationNode(t, empty)
	inProgress, failed, err = SlotMigrationState(context.Background(), emptyNode, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
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
		return [][]string{migrationEntry("failed", "0-399", "garbled entry", time.Unix(0, 0))}
	})
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
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
		return [][]string{migrationEntry("failed", "0-399", "skewed clock failure", now.Add(10*time.Minute))}
	})
	node := migrationNode(t, s)

	inProgress, failed, err := SlotMigrationState(context.Background(), node, []SlotsRange{{Start: 0, End: 399}}, func() time.Time { return now })
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
