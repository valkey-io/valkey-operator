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

package aclscan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOperatorCommands is a regression test: it locks in the set of Valkey
// commands the operator's reconciliation code currently issues. Adding a
// new client.B().Xxx() or Arbitrary() call anywhere under cmd/ or internal/
// should update this list - and, together with it, the "_operator" ACL in
// internal/controller/users.go and the e2e ACL DRYRUN coverage check.
func TestOperatorCommands(t *testing.T) {
	commands, err := OperatorCommands()
	require.NoError(t, err)

	got := make([]string, 0, len(commands))
	for _, c := range commands {
		got = append(got, c.String())
	}

	want := []string{
		"ACL GETUSER",
		"ACL LOAD",
		"ACL USERS",
		"CLUSTER ADDSLOTSRANGE",
		"CLUSTER FAILOVER",
		"CLUSTER FORGET",
		"CLUSTER GETSLOTMIGRATIONS",
		"CLUSTER INFO",
		"CLUSTER MEET",
		"CLUSTER MIGRATESLOTS",
		"CLUSTER MYID",
		"CLUSTER MYSHARDID",
		"CLUSTER NODES",
		"CLUSTER REPLICATE",
		"CLUSTER SET-CONFIG-EPOCH",
		"CONFIG SET",
		"INFO",
	}
	assert.ElementsMatch(t, want, got)
}

func TestBuilderCommandTokens(t *testing.T) {
	valkeyGoDir, err := moduleDir(valkeyGoModule)
	require.NoError(t, err)

	tokens, err := builderCommandTokens(valkeyGoDir + "/internal/cmds")
	require.NoError(t, err)

	assert.Equal(t, []string{"CLUSTER", "INFO"}, tokens["ClusterInfo"])
	assert.Equal(t, []string{"CLUSTER", "SET-CONFIG-EPOCH"}, tokens["ClusterSetConfigEpoch"])
	assert.Equal(t, []string{"CONFIG", "SET"}, tokens["ConfigSet"])
	assert.Equal(t, []string{"INFO"}, tokens["Info"])
	// Arbitrary forwards a caller-supplied slice rather than static
	// literals, so it must not be resolvable via the builder token map;
	// callers are expected to special-case it instead.
	_, ok := tokens["Arbitrary"]
	assert.False(t, ok)
}

func TestDedupe(t *testing.T) {
	in := []Command{
		{Tokens: []string{"CLUSTER", "INFO"}, Pos: "a.go:1"},
		{Tokens: []string{"INFO"}, Pos: "b.go:2"},
		{Tokens: []string{"CLUSTER", "INFO"}, Pos: "c.go:3"},
	}
	got := dedupe(in)
	require.Len(t, got, 2)
	assert.Equal(t, []string{"CLUSTER", "INFO"}, got[0].Tokens)
	assert.Equal(t, []string{"INFO"}, got[1].Tokens)
}

func TestStringLiteralArgs(t *testing.T) {
	assert.Empty(t, stringLiteralArgs(nil))
}
