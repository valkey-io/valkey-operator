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

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// SlotMigrationInProgress checks whether the source node has any
// non-terminal CLUSTER MIGRATESLOTS operations running.
func SlotMigrationInProgress(ctx context.Context, src *NodeState) (bool, error) {
	inProgress, _, err := SlotMigrationState(ctx, src, nil, nil)
	return inProgress, err
}

// SlotMigrationFailure describes a failed CLUSTER MIGRATESLOTS job, as
// reported by CLUSTER GETSLOTMIGRATIONS on the node that ran it.
type SlotMigrationFailure struct {
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
// job that does not overlap it is ignored. now is injectable for tests; a
// nil now means time.Now. A failed job older than
// slotMigrationFailureWindow is not reported either.
func SlotMigrationState(ctx context.Context, src *NodeState, planned []SlotsRange, now func() time.Time) (bool, *SlotMigrationFailure, error) {
	log := logf.FromContext(ctx)
	cmd := src.Client.B().Arbitrary("CLUSTER", "GETSLOTMIGRATIONS").Build()
	migrations, err := src.Client.Do(ctx, cmd).ToArray()
	if err != nil {
		return false, nil, wrapUnsupportedErr(fmt.Errorf("getslotmigrations failed on %s: %w", src.Address, err))
	}
	if now == nil {
		now = time.Now
	}
	var failed *SlotMigrationFailure
	for _, migration := range migrations {
		values, parseErr := migration.AsStrMap()
		if parseErr != nil {
			log.V(1).Info("unable to parse slot migration entry; treating as in progress", "src", src.Address, "error", parseErr)
			return true, nil, nil
		}
		state := strings.ToLower(values["state"])
		if !isSlotMigrationTerminal(state) {
			return true, nil, nil
		}
		if state != "failed" {
			continue
		}
		fields, fieldErr := migration.AsMap()
		if fieldErr != nil {
			// Not a field/value structure; skip it rather than guessing.
			log.V(1).Info("unable to parse slot migration entry fields", "src", src.Address, "error", fieldErr)
			continue
		}
		job := SlotMigrationFailure{
			Slots:      parseSlotRanges(values["slot_ranges"]),
			Message:    values["message"],
			LastUpdate: time.Unix(0, 0),
		}
		// last_update_time is an integer reply, which AsStrMap does not
		// carry, so read it from the typed field map. A missing or
		// unparseable timestamp stays the zero time, and a zero-time job
		// is never within the recency window, so a garbled entry cannot
		// wedge the reconcile.
		if v, ok := fields["last_update_time"]; ok {
			if t, err := v.AsInt64(); err == nil {
				job.LastUpdate = time.Unix(t, 0).UTC()
			}
		}
		if !job.overlaps(planned) {
			continue
		}
		if now().Sub(job.LastUpdate) > slotMigrationFailureWindow {
			continue
		}
		if failed == nil || job.LastUpdate.After(failed.LastUpdate) {
			failed = &job
		}
	}
	return false, failed, nil
}

// parseSlotRanges parses the slot_ranges field of a migration job: one or
// more "start-end" ranges separated by spaces, both ends inclusive.
// Unparseable parts are skipped, matching the server's own format.
func parseSlotRanges(s string) []SlotsRange {
	var ranges []SlotsRange
	for _, part := range strings.Fields(s) {
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
