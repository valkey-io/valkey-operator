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
	"reflect"
	"testing"
)

func TestParseSlotsRange(t *testing.T) {
	// Slot range
	slots, err := parseSlotsRange("0-16383")
	if err != nil {
		t.Errorf("Expected not expected, got %v", err)
	}
	expect := SlotsRange{0, 16383}
	if slots != expect {
		t.Errorf("Expected %v, got %v", expect, slots)
	}

	// Single slot range
	slots, err = parseSlotsRange("5")
	if err != nil {
		t.Errorf("Expected not expected, got %v", err)
	}
	expect = SlotsRange{5, 5}
	if slots != expect {
		t.Errorf("Expected %v, got %v", expect, slots)
	}
}

func TestParseSlotsRanges(t *testing.T) {
	ranges, err := parseSlotsRanges([]string{"0-5460", "5461-10922", "10923-16383"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []SlotsRange{{0, 5460}, {5461, 10922}, {10923, 16383}}
	if !reflect.DeepEqual(ranges, expected) {
		t.Errorf("expected %v, got %v", expected, ranges)
	}

	// Migrating/importing entries from CLUSTER NODES should be skipped.
	ranges, err = parseSlotsRanges([]string{"0-5460", "[5461->-abc123]", "[5462-<-def456]"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected = []SlotsRange{{0, 5460}}
	if !reflect.DeepEqual(ranges, expected) {
		t.Errorf("expected %v, got %v", expected, ranges)
	}

	// Empty input.
	ranges, err = parseSlotsRanges([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ranges) != 0 {
		t.Errorf("expected empty, got %v", ranges)
	}

	// Only migration entries — should return empty.
	ranges, err = parseSlotsRanges([]string{"[5461->-abc123]"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ranges) != 0 {
		t.Errorf("expected empty, got %v", ranges)
	}
}

func TestSubtractSlotsRange(t *testing.T) {
	base := SlotsRange{0, 16383}
	remove := SlotsRange{10, 16380}
	expect := []SlotsRange{{0, 9}, {16381, 16383}}
	result := SubtractSlotsRange(base, remove)
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}

	base = SlotsRange{0, 10}
	remove = SlotsRange{5, 10}
	expect = []SlotsRange{{0, 4}}
	result = SubtractSlotsRange(base, remove)
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}

	base = SlotsRange{0, 10}
	remove = SlotsRange{0, 9}
	expect = []SlotsRange{{10, 10}}
	result = SubtractSlotsRange(base, remove)
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}

	base = SlotsRange{0, 10}
	remove = SlotsRange{0, 10}
	result = SubtractSlotsRange(base, remove)
	if result != nil {
		t.Errorf("Expected nil, got %v", result)
	}
}

func TestGetUnassignedSlots(t *testing.T) {
	// A shard with no unassigned slots
	cluster := ClusterState{
		Shards: []*ShardState{
			{
				Slots: []SlotsRange{{0, 16383}},
			},
		},
	}
	result := cluster.GetUnassignedSlots()
	if len(result) != 0 {
		t.Errorf("Expected empty array, got %v", result)
	}

	// A single shard with the unassigned slot 0
	cluster = ClusterState{
		Shards: []*ShardState{
			{
				Slots: []SlotsRange{{1, 16383}},
			},
		},
	}
	result = cluster.GetUnassignedSlots()
	expect := []SlotsRange{{0, 0}}
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}

	// Three shards with unassigned slots
	cluster = ClusterState{
		Shards: []*ShardState{
			{
				Slots: []SlotsRange{{100, 200}, {300, 400}},
			},
			{
				Slots: []SlotsRange{{700, 800}},
			},
			{
				Slots: []SlotsRange{{500, 600}},
			},
		},
	}
	result = cluster.GetUnassignedSlots()
	expect = []SlotsRange{{0, 99}, {201, 299}, {401, 499}, {601, 699}, {801, 16383}}
	if !reflect.DeepEqual(result, expect) {
		t.Errorf("Expected %v, got %v", expect, result)
	}
}
