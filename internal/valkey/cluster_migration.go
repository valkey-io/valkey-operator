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
	"sort"
	"strconv"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// SlotMigrationInProgress checks whether the source node has any
// non-terminal CLUSTER MIGRATESLOTS operations running.
func SlotMigrationInProgress(ctx context.Context, src *NodeState) (bool, error) {
	log := logf.FromContext(ctx)
	cmd := src.Client.B().Arbitrary("CLUSTER", "GETSLOTMIGRATIONS").Build()
	migrations, err := src.Client.Do(ctx, cmd).ToArray()
	if err != nil {
		return false, wrapUnsupportedErr(fmt.Errorf("getslotmigrations failed on %s: %w", src.Address, err))
	}
	for _, migration := range migrations {
		values, parseErr := migration.AsStrMap()
		if parseErr != nil {
			log.V(1).Info("unable to parse slot migration entry; treating as in progress", "src", src.Address, "error", parseErr)
			return true, nil
		}
		state := strings.ToLower(values["state"])
		if !isSlotMigrationTerminal(state) {
			return true, nil
		}
	}
	return false, nil
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
	sort.Ints(ordered)
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
