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

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	primaryID = "07c37dfeb235213a872192d90877d0cd55635b91"
	replicaID = "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca"
	otherID   = "67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1"
)

// runEntrypoint runs scripts/entrypoint.sh with a data dir holding the given
// nodes.conf and a dump.rdb, and a stub valkey-server on PATH that prints its
// arguments. It returns the output and whether nodes.conf survived.
func runEntrypoint(t *testing.T, nodesConf string) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "valkey-server"),
		[]byte("#!/bin/sh\necho \"valkey-server $*\"\n"), 0o755))
	if nodesConf != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "nodes.conf"), []byte(nodesConf), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "dump.rdb"), []byte("REDIS0011"), 0o644))
	}

	cmd := exec.Command(shellPath, "scripts/entrypoint.sh", "/config/valkey.conf", "--port", "6379")
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "VALKEY_DATA_DIR="+dataDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	_, statErr := os.Stat(filepath.Join(dataDir, "nodes.conf"))
	return string(out), statErr == nil
}

func TestEntrypointScript(t *testing.T) {
	const serverLine = "valkey-server /config/valkey.conf --port 6379"
	primary := primaryID + " 10.0.0.1:6379@16379,valkey-c-0-0-0 myself,master - 0 1760000000000 1 connected 0-5460\n"
	replica := replicaID + " 10.0.0.2:6379@16379,valkey-c-0-1-0 slave " + primaryID + " 0 1760000000000 1 connected\n"
	other := otherID + " 10.0.0.3:6379@16379,valkey-c-1-0-0 master - 0 1760000000000 2 connected 5461-10922\n"
	vars := "vars currentEpoch 3 lastVoteEpoch 0\n"

	t.Run("no nodes.conf starts the server untouched", func(t *testing.T) {
		out, kept := runEntrypoint(t, "")
		assert.Contains(t, out, serverLine)
		assert.False(t, kept)
	})

	t.Run("a primary with a replica starts as a new node", func(t *testing.T) {
		out, kept := runEntrypoint(t, primary+replica+other+vars)
		assert.Contains(t, out, "Previous run was primary "+primaryID+" with a replica")
		assert.Contains(t, out, serverLine)
		assert.False(t, kept)
	})

	t.Run("a primary without a replica keeps its identity", func(t *testing.T) {
		otherReplica := replicaID + " 10.0.0.2:6379@16379,valkey-c-1-1-0 slave " + otherID + " 0 1760000000000 2 connected\n"
		out, kept := runEntrypoint(t, primary+other+otherReplica+vars)
		assert.NotContains(t, out, "Previous run")
		assert.Contains(t, out, serverLine)
		assert.True(t, kept)
	})

	t.Run("a replica keeps its identity", func(t *testing.T) {
		me := replicaID + " 10.0.0.2:6379@16379,valkey-c-0-1-0 myself,slave " + primaryID + " 0 1760000000000 1 connected\n"
		them := primaryID + " 10.0.0.1:6379@16379,valkey-c-0-0-0 master - 0 1760000000000 1 connected 0-5460\n"
		out, kept := runEntrypoint(t, me+them+vars)
		assert.NotContains(t, out, "Previous run")
		assert.Contains(t, out, serverLine)
		assert.True(t, kept)
	})
}
