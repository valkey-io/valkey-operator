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
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	primaryID = "07c37dfeb235213a872192d90877d0cd55635b91"
	replicaID = "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca"
	otherID   = "67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1"
)

// entrypointRun is one run of scripts/entrypoint.sh against a temporary data
// dir, with stub valkey-server and valkey-cli binaries on PATH.
type entrypointRun struct {
	out      string
	nodeKept bool   // nodes.conf survived
	dataDir  string // for further file checks
	cliCalls int    // ROLE calls the replica stub answered
}

// runEntrypoint runs the script with the given nodes.conf and valkey.conf. The
// stub valkey-cli answers ROLE with "slave of 10.0.0.1" promotedAfter times,
// then "master"; a negative promotedAfter keeps it a replica forever.
func runEntrypoint(t *testing.T, nodesConf, valkeyConf string, promotedAfter int) entrypointRun {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "valkey-server"),
		[]byte("#!/bin/sh\necho \"valkey-server $*\"\n"), 0o755))
	calls := filepath.Join(dir, "calls")
	cli := "#!/bin/sh\n" +
		"n=$(cat " + calls + " 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + calls + "\n" +
		"if [ " + strconv.Itoa(promotedAfter) + " -ge 0 ] && [ $n -gt " + strconv.Itoa(promotedAfter) + " ]; then printf 'master\\n0\\n'; " +
		"else printf 'slave\\n10.0.0.1\\n6379\\nconnected\\n5\\n'; fi\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "valkey-cli"), []byte(cli), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "valkey.conf"), []byte(valkeyConf), 0o644))
	if nodesConf != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "nodes.conf"), []byte(nodesConf), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "dump.rdb"), []byte("REDIS0011"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "temp-42.rdb"), []byte("REDIS0011"), 0o644))
	}

	cmd := exec.Command(shellPath, "scripts/entrypoint.sh", filepath.Join(dir, "valkey.conf"), "--port", "6379")
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "VALKEY_DATA_DIR="+dataDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	_, statErr := os.Stat(filepath.Join(dataDir, "nodes.conf"))
	n := 0
	if b, err := os.ReadFile(calls); err == nil {
		n, _ = strconv.Atoi(string(b[:len(b)-1]))
	}
	return entrypointRun{out: string(out), nodeKept: statErr == nil, dataDir: dataDir, cliCalls: n}
}

func TestEntrypointScript(t *testing.T) {
	const serverLine = "valkey-server"
	// Valkey 9 address format, with an empty hostname and a tls-port of 0.
	primary := primaryID + " 10.0.0.1:6379@16379,,tls-port=0,shard-id=abc myself,master - 0 1760000000000 1 connected 0-5460\n"
	replica := replicaID + " 10.0.0.2:6379@16379,,tls-port=0,shard-id=abc slave " + primaryID + " 0 1760000000000 1 connected\n"
	other := otherID + " 10.0.0.3:6379@16379,,tls-port=0,shard-id=def master - 0 1760000000000 2 connected 5461-10922\n"
	vars := "vars currentEpoch 3 lastVoteEpoch 0\n"
	conf := "cluster-node-timeout 1000\n" // the hand-off wait caps at six timeouts: 6s

	t.Run("no nodes.conf starts the server untouched", func(t *testing.T) {
		r := runEntrypoint(t, "", conf, 0)
		assert.Contains(t, r.out, serverLine)
		assert.False(t, r.nodeKept)
		assert.Zero(t, r.cliCalls)
	})

	t.Run("a primary with a replica waits for its promotion and starts as a new node", func(t *testing.T) {
		r := runEntrypoint(t, primary+replica+other+vars, conf, 2)
		assert.Contains(t, r.out, "Previous run was primary "+primaryID+" with a replica")
		assert.Contains(t, r.out, "Replica hand-off done after 2s")
		assert.Contains(t, r.out, serverLine)
		assert.False(t, r.nodeKept)
		assert.Equal(t, 3, r.cliCalls)
		assert.NoFileExists(t, filepath.Join(r.dataDir, "dump.rdb"))
		assert.NoFileExists(t, filepath.Join(r.dataDir, "temp-42.rdb"))
	})

	t.Run("the wait is capped at six cluster-node-timeouts", func(t *testing.T) {
		r := runEntrypoint(t, primary+replica+vars, conf, -1)
		assert.Contains(t, r.out, "still follows this node after 6s; starting anyway")
		assert.Contains(t, r.out, serverLine)
		assert.False(t, r.nodeKept)
	})

	t.Run("a custom snapshot name from the config is removed", func(t *testing.T) {
		r := runEntrypoint(t, primary+replica+vars, conf+"dbfilename custom.rdb\n", 0)
		assert.False(t, r.nodeKept)
		assert.NoFileExists(t, filepath.Join(r.dataDir, "custom.rdb"))
	})

	t.Run("a replica flagged fail does not count", func(t *testing.T) {
		failed := replicaID + " 10.0.0.2:6379@16379,,tls-port=0,shard-id=abc slave,fail " + primaryID + " 0 1760000000000 1 connected\n"
		r := runEntrypoint(t, primary+failed+other+vars, conf, 0)
		assert.NotContains(t, r.out, "Previous run")
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
		assert.Zero(t, r.cliCalls)
	})

	t.Run("a primary without a replica keeps its identity", func(t *testing.T) {
		otherReplica := replicaID + " 10.0.0.2:6379@16379,valkey-c-1-1-0 slave " + otherID + " 0 1760000000000 2 connected\n"
		r := runEntrypoint(t, primary+other+otherReplica+vars, conf, 0)
		assert.NotContains(t, r.out, "Previous run")
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
	})

	t.Run("a replica keeps its identity", func(t *testing.T) {
		me := replicaID + " 10.0.0.2:6379@16379,valkey-c-0-1-0 myself,slave " + primaryID + " 0 1760000000000 1 connected\n"
		them := primaryID + " 10.0.0.1:6379@16379,valkey-c-0-0-0 master - 0 1760000000000 1 connected 0-5460\n"
		r := runEntrypoint(t, me+them+vars, conf, 0)
		assert.NotContains(t, r.out, "Previous run")
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
	})
}
