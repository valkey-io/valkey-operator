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

func TestDefaultQuorum(t *testing.T) {
	for sentinels, want := range map[int]int{1: 1, 2: 2, 3: 2, 4: 3, 5: 3, 7: 4} {
		if got := DefaultQuorum(sentinels); got != want {
			t.Errorf("DefaultQuorum(%d) = %d, want %d", sentinels, got, want)
		}
	}
}

func TestParseSentinelMaster(t *testing.T) {
	t.Run("healthy master", func(t *testing.T) {
		m, err := ParseSentinelMaster(map[string]string{
			"name": "mymaster", "ip": "10.0.0.1", "port": "6379", "flags": "master",
			"quorum": "2", "num-slaves": "2", "num-other-sentinels": "2",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := SentinelMaster{
			Name: "mymaster", IP: "10.0.0.1", Port: 6379, Flags: []string{"master"},
			Quorum: 2, NumReplicas: 2, NumOtherSentinels: 2,
		}
		if !reflect.DeepEqual(m, want) {
			t.Errorf("got %+v, want %+v", m, want)
		}
		if m.IsDown() || m.FailoverInProgress() {
			t.Error("healthy master should be neither down nor failing over")
		}
	})

	t.Run("flags", func(t *testing.T) {
		for flags, want := range map[string]struct{ down, failover bool }{
			"master,s_down":                      {true, false},
			"master,s_down,o_down":               {true, false},
			"master,o_down,failover_in_progress": {true, true},
			"master,failover_in_progress":        {false, true},
			"":                                   {false, false},
		} {
			m, err := ParseSentinelMaster(map[string]string{"name": "m", "flags": flags})
			if err != nil {
				t.Fatalf("flags %q: unexpected error: %v", flags, err)
			}
			if m.IsDown() != want.down || m.FailoverInProgress() != want.failover {
				t.Errorf("flags %q: down=%v failover=%v, want %v", flags, m.IsDown(), m.FailoverInProgress(), want)
			}
		}
	})

	t.Run("missing optional fields are zero", func(t *testing.T) {
		m, err := ParseSentinelMaster(map[string]string{"name": "m"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m.Port != 0 || m.Quorum != 0 || m.Flags != nil {
			t.Errorf("expected zero values, got %+v", m)
		}
	})

	t.Run("errors", func(t *testing.T) {
		for name, fields := range map[string]map[string]string{
			"no name":       {"ip": "10.0.0.1"},
			"bad port":      {"name": "m", "port": "abc"},
			"bad quorum":    {"name": "m", "quorum": "x"},
			"bad replicas":  {"name": "m", "num-slaves": "-x"},
			"bad sentinels": {"name": "m", "num-other-sentinels": "1.5"},
		} {
			if _, err := ParseSentinelMaster(fields); err == nil {
				t.Errorf("%s: expected an error", name)
			}
		}
	})
}

func TestParseSentinelReplica(t *testing.T) {
	r, err := ParseSentinelReplica(map[string]string{
		"name": "10.0.0.2:6379", "ip": "10.0.0.2", "port": "6379", "flags": "slave,s_down",
		"master-link-status": "err", "slave-priority": "100", "slave-repl-offset": "9007199254740993",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := SentinelReplica{
		Name: "10.0.0.2:6379", IP: "10.0.0.2", Port: 6379, Flags: []string{"slave", "s_down"},
		MasterLinkStatus: "err", ReplicaPriority: 100, ReplicaOffset: 9007199254740993,
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("got %+v, want %+v", r, want)
	}
	if !r.IsDown() {
		t.Error("s_down replica should be down")
	}

	for name, fields := range map[string]map[string]string{
		"no name":    {"ip": "10.0.0.2"},
		"bad port":   {"name": "r", "port": "x"},
		"bad prio":   {"name": "r", "slave-priority": "x"},
		"bad offset": {"name": "r", "slave-repl-offset": "x"},
	} {
		if _, err := ParseSentinelReplica(fields); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseSentinelPeer(t *testing.T) {
	p, err := ParseSentinelPeer(map[string]string{
		"name": "abc", "runid": "abc", "ip": "10.0.0.3", "port": "26379", "flags": "sentinel",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := SentinelPeer{Name: "abc", RunID: "abc", IP: "10.0.0.3", Port: 26379, Flags: []string{"sentinel"}}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("got %+v, want %+v", p, want)
	}

	if _, err := ParseSentinelPeer(map[string]string{"ip": "10.0.0.3"}); err == nil {
		t.Error("expected an error for a missing name")
	}
	if _, err := ParseSentinelPeer(map[string]string{"name": "abc", "port": "x"}); err == nil {
		t.Error("expected an error for a bad port")
	}
}

func TestSentinelMonitorArgs(t *testing.T) {
	got := sentinelMonitorArgs("mymaster", "valkey-0.valkey", 6379, 2)
	want := []string{"MONITOR", "mymaster", "valkey-0.valkey", "6379", "2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSentinelSetArgs(t *testing.T) {
	got := sentinelSetArgs("mymaster", map[string]string{
		"parallel-syncs": "1", "down-after-milliseconds": "30000", "failover-timeout": "180000",
	})
	want := []string{
		"SET", "mymaster",
		"down-after-milliseconds", "30000",
		"failover-timeout", "180000",
		"parallel-syncs", "1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
