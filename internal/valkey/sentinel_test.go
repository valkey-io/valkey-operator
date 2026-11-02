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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSentinelMaster(t *testing.T) {
	tests := []struct {
		name    string
		fields  map[string]string
		want    SentinelMaster
		wantErr bool
	}{
		{
			name: "healthy master",
			fields: map[string]string{
				"name": "cache", "ip": "10.0.0.5", "port": "6379", "runid": "abc",
				"flags": "master", "quorum": "2", "num-slaves": "2", "num-other-sentinels": "2",
			},
			want: SentinelMaster{
				SentinelInstance: SentinelInstance{Name: "cache", IP: "10.0.0.5", Port: 6379, RunID: "abc", Flags: []string{"master"}},
				Quorum:           2, NumReplicas: 2, NumOtherSentinels: 2,
			},
		},
		{
			name:   "failover in progress",
			fields: map[string]string{"name": "cache", "ip": "10.0.0.5", "port": "6379", "flags": "master,o_down,failover_in_progress"},
			want: SentinelMaster{
				SentinelInstance: SentinelInstance{Name: "cache", IP: "10.0.0.5", Port: 6379, Flags: []string{"master", "o_down", "failover_in_progress"}},
			},
		},
		{name: "missing name", fields: map[string]string{"ip": "10.0.0.5"}, wantErr: true},
		{name: "bad port", fields: map[string]string{"name": "cache", "port": "x"}, wantErr: true},
		{name: "bad quorum", fields: map[string]string{"name": "cache", "quorum": "two"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSentinelMaster(tt.fields)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			tt.want.Fields = tt.fields
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSentinelMasterFlags(t *testing.T) {
	m, err := ParseSentinelMaster(map[string]string{"name": "cache", "flags": "master,s_down,failover_in_progress"})
	require.NoError(t, err)
	assert.True(t, m.FailoverInProgress())
	assert.True(t, m.HasFlag("s_down"))
	assert.False(t, m.HasFlag("o_down"))

	m, err = ParseSentinelMaster(map[string]string{"name": "cache", "flags": "master"})
	require.NoError(t, err)
	assert.False(t, m.FailoverInProgress())
}

func TestParseSentinelInstance(t *testing.T) {
	inst, err := ParseSentinelInstance(map[string]string{"name": "10.0.0.6:6379", "ip": "10.0.0.6", "port": "6379", "flags": "slave"})
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.6:6379", inst.Address())
	assert.True(t, inst.HasFlag("slave"))

	inst, err = ParseSentinelInstance(map[string]string{"name": "s1"})
	require.NoError(t, err)
	assert.Empty(t, inst.Flags)
	assert.Zero(t, inst.Port)
}

func TestParseMasterAddr(t *testing.T) {
	tests := []struct {
		name     string
		reply    []string
		wantHost string
		wantPort int
		wantOK   bool
		wantErr  bool
	}{
		{name: "known master", reply: []string{"valkey-cache-0.valkey-cache.ns.svc.cluster.local", "6379"}, wantHost: "valkey-cache-0.valkey-cache.ns.svc.cluster.local", wantPort: 6379, wantOK: true},
		{name: "unknown master", reply: nil},
		{name: "bad length", reply: []string{"10.0.0.5"}, wantErr: true},
		{name: "bad port", reply: []string{"10.0.0.5", "x"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, ok, err := ParseMasterAddr(tt.reply)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantHost, host)
			assert.Equal(t, tt.wantPort, port)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}

func TestParseCKQuorum(t *testing.T) {
	tests := []struct {
		name      string
		reply     string
		serverErr string
		wantOK    bool
		wantMsg   string
		wantErr   bool
	}{
		{name: "quorum ok", reply: "OK 3 usable Sentinels. Quorum and failover authorization can be reached", wantOK: true, wantMsg: "OK 3 usable Sentinels. Quorum and failover authorization can be reached"},
		{name: "no quorum", serverErr: "NOQUORUM 1 usable Sentinels. Not enough available Sentinels to reach the specified quorum for this master", wantMsg: "NOQUORUM 1 usable Sentinels. Not enough available Sentinels to reach the specified quorum for this master"},
		{name: "unknown master", serverErr: "ERR No such master with that name", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, msg, err := ParseCKQuorum(tt.reply, tt.serverErr)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantMsg, msg)
		})
	}
}
