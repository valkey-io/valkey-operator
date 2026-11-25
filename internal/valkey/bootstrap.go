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
	"errors"
	"fmt"
	"slices"
)

// SourceShard is one shard of an existing (source) cluster that a bootstrap
// target shard will join as a replica set.
type SourceShard struct {
	// PrimaryId is the node ID of the source primary.
	PrimaryId string
	// PrimaryHost is the host the source primary announces.
	PrimaryHost string
	// Slots are the primary's slot ranges, sorted by start.
	Slots []SlotsRange
}

// PlanBootstrapSource validates the CLUSTER NODES table of a source cluster and
// returns its shards ordered by first slot. Target shard i maps to element i, so
// the mapping is deterministic across reconciles and operator restarts.
//
// It rejects a source that cannot be joined safely: a failing or address-less
// primary, slots in flight, incomplete slot coverage, or a primary count that
// differs from wantShards (resharding on the way in is out of scope).
func PlanBootstrapSource(nodes []ClusterNode, wantShards int) ([]SourceShard, error) {
	var shards []SourceShard
	covered := 0
	for i := range nodes {
		n := &nodes[i]
		if !n.IsPrimary() || !n.HasSlotAssignment() {
			continue
		}
		if n.IsFailing() || n.HasNoAddress() {
			return nil, fmt.Errorf("source primary %s is failing or has no address", n.Id)
		}
		if len(n.Migrating) > 0 || len(n.Importing) > 0 {
			return nil, fmt.Errorf("source primary %s has slots migrating; wait for the migration to finish", n.Id)
		}
		slots := slices.Clone(n.Slots)
		slices.SortFunc(slots, func(a, b SlotsRange) int { return a.Start - b.Start })
		for _, r := range slots {
			covered += r.End - r.Start + 1
		}
		shards = append(shards, SourceShard{PrimaryId: n.Id, PrimaryHost: n.Host, Slots: slots})
	}
	if len(shards) == 0 {
		return nil, errors.New("source reports no primaries owning slots")
	}
	if covered != TotalSlots {
		return nil, fmt.Errorf("source covers %d of %d slots; the source cluster must be healthy", covered, TotalSlots)
	}
	if len(shards) != wantShards {
		return nil, fmt.Errorf("source has %d shards but spec.shards is %d; they must match to bootstrap", len(shards), wantShards)
	}
	slices.SortFunc(shards, func(a, b SourceShard) int { return a.Slots[0].Start - b.Slots[0].Start })
	return shards, nil
}
