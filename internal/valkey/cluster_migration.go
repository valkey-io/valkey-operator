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
	"time"

	vclient "github.com/valkey-io/valkey-go"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// SlotMigrationFailure describes a failed CLUSTER MIGRATESLOTS job, as
// reported by CLUSTER GETSLOTMIGRATIONS on the node that ran it.
type SlotMigrationFailure struct {
	// TargetNode is the node ID the failed job was migrating to, from
	// its target_node field. Empty when the server does not report one.
	TargetNode string
	// Slots are the slot ranges the failed job covered, parsed from its
	// slot_ranges field.
	Slots []SlotsRange
	// Message is the server's own failure description, for example the
	// handshake error reported by the target node.
	Message string
	// LastUpdate is when the server last changed the job's state, used
	// to decide whether the failure is recent enough to still report.
	LastUpdate time.Time
}

// overlaps reports whether the failed job covered any of the given slot
// ranges. Jobs from unrelated migrations (manual resharding, a different
// batch) do not match, so leftover history cannot wedge a rebalance.
func (f *SlotMigrationFailure) overlaps(ranges []SlotsRange) bool {
	for _, owned := range f.Slots {
		for _, planned := range ranges {
			if owned.Start <= planned.End && planned.Start <= owned.End {
				return true
			}
		}
	}
	return false
}

// slotMigrationFailureWindow bounds how long a failed job is reported as
// the reason a rebalance is not making progress. Failed jobs stay visible
// in CLUSTER GETSLOTMIGRATIONS output long after they happened, so without
// a window ancient history would wedge the reconcile loop. While a
// failure is recent the operator holds off re-issuing the same
// MIGRATESLOTS (it would only record another failed job); once the window
// lapses it retries the move, so a transient failure delays the batch by
// at most one window and a persistent failure retries once per window.
const slotMigrationFailureWindow = 2 * time.Minute

// SlotMigrationState reports whether the source node has any non-terminal
// CLUSTER MIGRATESLOTS operations running, and the most recent failed job
// overlapping the slots the caller plans to move.
//
// A job that CLUSTER MIGRATESLOTS accepts runs asynchronously on the
// server; a failure during its handshake or transfer is recorded only in
// CLUSTER GETSLOTMIGRATIONS as a terminal "failed" entry. Nothing else
// reports it, so without reading the job back the next reconcile sees no
// migration in progress and silently re-issues the same move forever.
//
// planned is the set of slot ranges the caller is about to move; a failed
// job that does not overlap it is ignored. targetNode is the node ID the
// caller plans to migrate those slots to; a failed job that reports a
// different target_node is ignored, because it belongs to another move and
// must not wedge (or be misattributed to) this one. An empty targetNode
// disables the destination filter, and a job whose target_node the server
// does not report still matches every destination, so a server without the
// field can never silently unblock the retry loop. A failed job older than
// slotMigrationFailureWindow is not reported either. The node stamps
// last_update_time with its own clock, so the age is measured against the
// node's TIME, read in the same pipeline.
func SlotMigrationState(ctx context.Context, src *NodeState, planned []SlotsRange, targetNode string) (bool, *SlotMigrationFailure, error) {
	log := logf.FromContext(ctx)
	resps := src.Client.DoMulti(ctx,
		src.Client.B().Arbitrary("CLUSTER", "GETSLOTMIGRATIONS").Build(),
		src.Client.B().Time().Build(),
	)
	migrations, err := resps[0].ToArray()
	if err != nil {
		return false, nil, wrapUnsupportedErr(fmt.Errorf("getslotmigrations failed on %s: %w", src.Address, err))
	}
	nodeNow, err := parseTimeReply(resps[1])
	if err != nil {
		return false, nil, fmt.Errorf("time failed on %s: %w", src.Address, err)
	}
	var failed *SlotMigrationFailure
	for _, migration := range migrations {
		fields, parseErr := migration.AsMap()
		if parseErr != nil {
			log.V(1).Info("unable to parse slot migration entry; treating as in progress", "src", src.Address, "error", parseErr)
			return true, nil, nil
		}
		// A missing or non-string field reads as "".
		field := func(name string) string {
			msg := fields[name]
			v, _ := msg.ToString()
			return v
		}
		state := strings.ToLower(field("state"))
		if !isSlotMigrationTerminal(state) {
			return true, nil, nil
		}
		if state != "failed" {
			continue
		}
		job := SlotMigrationFailure{
			TargetNode: field("target_node"),
			Slots:      parseSlotRanges(field("slot_ranges")),
			Message:    strings.TrimSpace(field("message")),
			LastUpdate: time.Unix(0, 0),
		}
		// last_update_time is an integer reply. A missing or unparseable
		// timestamp stays the zero time, and a zero-time job is never
		// within the recency window, so a garbled entry cannot wedge the
		// reconcile.
		if v, ok := fields["last_update_time"]; ok {
			if t, err := v.AsInt64(); err == nil {
				job.LastUpdate = time.Unix(t, 0).UTC()
			}
		}
		// A failure that named a different destination belongs to
		// another move and must not wedge (or speak for) this one. An
		// entry with no reported target_node still matches every
		// destination, so a server that omits the field cannot
		// silently unblock the retry loop.
		if targetNode != "" && job.TargetNode != "" && job.TargetNode != targetNode {
			continue
		}
		if !job.overlaps(planned) {
			continue
		}
		if nodeNow.Sub(job.LastUpdate) > slotMigrationFailureWindow {
			continue
		}
		if failed == nil || job.LastUpdate.After(failed.LastUpdate) {
			failed = &job
		}
	}
	return false, failed, nil
}

// parseTimeReply reads a TIME reply: Unix seconds and microseconds as two
// bulk strings.
func parseTimeReply(resp vclient.ValkeyResult) (time.Time, error) {
	parts, err := resp.AsStrSlice()
	if err != nil {
		return time.Time{}, err
	}
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("unexpected TIME reply %q", parts)
	}
	sec, err1 := strconv.ParseInt(parts[0], 10, 64)
	usec, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return time.Time{}, fmt.Errorf("unexpected TIME reply %q", parts)
	}
	return time.Unix(sec, usec*1000).UTC(), nil
}

// parseSlotRanges parses the slot_ranges field of a migration job: one or
// more "start-end" ranges separated by spaces, both ends inclusive.
// Unparseable parts are skipped, matching the server's own format.
func parseSlotRanges(s string) []SlotsRange {
	var ranges []SlotsRange
	for part := range strings.FieldsSeq(s) {
		start, end, ok := strings.Cut(part, "-")
		first, err1 := strconv.Atoi(start)
		last, err2 := strconv.Atoi(end)
		if !ok || err1 != nil || err2 != nil {
			continue
		}
		ranges = append(ranges, SlotsRange{Start: first, End: last})
	}
	return ranges
}

func isSlotMigrationTerminal(state string) bool {
	switch state {
	case "success", "failed", "canceled", "cancelled":
		return true
	default:
		return false
	}
}

// MigrateSlotsAtomic issues a single CLUSTER MIGRATESLOTS command
// covering all the given ranges.
func MigrateSlotsAtomic(ctx context.Context, src *NodeState, dst *NodeState, ranges []SlotsRange) error {
	cmd := src.Client.B().Arbitrary("CLUSTER", "MIGRATESLOTS")
	for _, slotRange := range ranges {
		cmd = cmd.Args(
			"SLOTSRANGE",
			strconv.Itoa(slotRange.Start),
			strconv.Itoa(slotRange.End),
			"NODE",
			dst.Id,
		)
	}
	if err := src.Client.Do(ctx, cmd.Build()).Error(); err != nil {
		return wrapUnsupportedErr(fmt.Errorf("migrateslots failed from %s to %s: %w", src.Address, dst.Address, err))
	}
	return nil
}

// SlotsToRanges converts a slice of individual slot numbers into
// a compact slice of contiguous SlotsRange values.
func SlotsToRanges(slots []int) []SlotsRange {
	if len(slots) == 0 {
		return nil
	}
	ordered := append([]int(nil), slots...)
	slices.Sort(ordered)
	ranges := make([]SlotsRange, 0, len(ordered))
	start := ordered[0]
	prev := ordered[0]
	for _, slot := range ordered[1:] {
		if slot == prev+1 {
			prev = slot
			continue
		}
		ranges = append(ranges, SlotsRange{Start: start, End: prev})
		start = slot
		prev = slot
	}
	ranges = append(ranges, SlotsRange{Start: start, End: prev})
	return ranges
}

// wrapUnsupportedErr returns a wrapped error with an upgrade hint if
// the server does not recognise an atomic-migration subcommand.
func wrapUnsupportedErr(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "unknown subcommand") ||
		strings.Contains(msg, "wrong number of arguments") {
		return fmt.Errorf("%w; please upgrade to Valkey 9.0.0 or later for atomic slot migration support", err)
	}
	return err
}

// IsSlotsNotServedByNode reports whether err indicates the requested
// slots are no longer owned by the source node.
func IsSlotsNotServedByNode(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "slots are not served by this node")
}
