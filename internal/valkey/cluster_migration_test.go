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
	"reflect"
	"strings"
	"testing"
)

func TestSlotsToRanges(t *testing.T) {
	tests := []struct {
		name  string
		slots []int
		want  []SlotsRange
	}{
		{name: "nil", slots: nil, want: nil},
		{name: "empty", slots: []int{}, want: nil},
		{name: "single", slots: []int{5}, want: []SlotsRange{{Start: 5, End: 5}}},
		{
			name:  "contiguous",
			slots: []int{0, 1, 2, 3},
			want:  []SlotsRange{{Start: 0, End: 3}},
		},
		{
			name:  "two ranges",
			slots: []int{0, 1, 2, 10, 11, 12},
			want:  []SlotsRange{{Start: 0, End: 2}, {Start: 10, End: 12}},
		},
		{
			name:  "unsorted input",
			slots: []int{12, 0, 11, 2, 10, 1},
			want:  []SlotsRange{{Start: 0, End: 2}, {Start: 10, End: 12}},
		},
		{
			name:  "non-contiguous singles",
			slots: []int{3, 7, 15},
			want:  []SlotsRange{{Start: 3, End: 3}, {Start: 7, End: 7}, {Start: 15, End: 15}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SlotsToRanges(tt.slots)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SlotsToRanges(%v) = %v, want %v", tt.slots, got, tt.want)
			}
		})
	}
}

func TestWrapUnsupportedErr(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantSuffix bool
	}{
		{name: "nil", err: nil, wantSuffix: false},
		{name: "unrelated error", err: errors.New("connection refused"), wantSuffix: false},
		{name: "unknown command", err: errors.New("ERR Unknown command 'CLUSTER|MIGRATESLOTS'"), wantSuffix: true},
		{name: "unknown subcommand", err: errors.New("ERR unknown subcommand 'MIGRATESLOTS'"), wantSuffix: true},
		{name: "wrong number of arguments", err: errors.New("ERR wrong number of arguments for 'cluster|migrateslots' command"), wantSuffix: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapUnsupportedErr(tt.err)
			if tt.err == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if tt.wantSuffix {
				if !strings.Contains(got.Error(), "please upgrade to Valkey 9.0.0") {
					t.Errorf("expected upgrade hint, got: %v", got)
				}
				if !errors.Is(got, tt.err) {
					t.Errorf("wrapped error should preserve original via errors.Is")
				}
			} else {
				if got != tt.err {
					t.Errorf("expected original error returned unchanged, got: %v", got)
				}
			}
		})
	}
}

func TestIsSlotsNotServedByNode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "unrelated", err: errors.New("connection refused"), want: false},
		{name: "match", err: errors.New("ERR Slots are not served by this node"), want: true},
		{name: "match lowercase", err: errors.New("slots are not served by this node"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSlotsNotServedByNode(tt.err); got != tt.want {
				t.Errorf("IsSlotsNotServedByNode(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsSlotMigrationTerminal(t *testing.T) {
	tests := []struct {
		state    string
		terminal bool
	}{
		{"success", true},
		{"failed", true},
		{"canceled", true},
		{"cancelled", true},
		{"running", false},
		{"pending", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := isSlotMigrationTerminal(tt.state); got != tt.terminal {
				t.Errorf("isSlotMigrationTerminal(%q) = %v, want %v", tt.state, got, tt.terminal)
			}
		})
	}
}
