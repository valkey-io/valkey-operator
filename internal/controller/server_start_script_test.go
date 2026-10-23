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

// scriptRun is one run of scripts/server-start.sh against a temporary data
// dir, with stub valkey-server and valkey-cli binaries on PATH.
type scriptRun struct {
	out      string
	nodeKept bool   // nodes.conf survived
	dataDir  string // for further file checks
	cliCalls int    // ROLE calls the replica stub answered
}

// stubCLI is the ROLE answer of the stub valkey-cli: a replica of 10.0.0.1
// (or of primaryHost when set) promotedAfter times, then a primary. A
// negative promotedAfter keeps it a replica forever, and hang makes every
// call sleep past the script's per-call timeout.
type stubCLI struct {
	promotedAfter int
	primaryHost   string
	hang          bool
	seedFiles     []string // extra files to create in the data dir
}

// runStartScript runs the script with the given nodes.conf and valkey.conf.
func runStartScript(t *testing.T, nodesConf, valkeyConf string, cli stubCLI) scriptRun {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "valkey-server"),
		[]byte("#!/bin/sh\necho \"valkey-server $*\"\n"), 0o755))
	calls := filepath.Join(dir, "calls")
	primaryHost := cli.primaryHost
	if primaryHost == "" {
		primaryHost = "10.0.0.1"
	}
	script := "#!/bin/sh\n" +
		"n=$(cat " + calls + " 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + calls + "\n"
	if cli.hang {
		script += "sleep 30\n"
	}
	script += "if [ " + strconv.Itoa(cli.promotedAfter) + " -ge 0 ] && [ $n -gt " + strconv.Itoa(cli.promotedAfter) + " ]; then printf 'master\\n0\\n'; " +
		"else printf 'slave\\n" + primaryHost + "\\n6379\\nconnected\\n5\\n'; fi\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "valkey-cli"), []byte(script), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "valkey.conf"), []byte(valkeyConf), 0o644))
	if nodesConf != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "nodes.conf"), []byte(nodesConf), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "dump.rdb"), []byte("REDIS0011"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "temp-42.rdb"), []byte("REDIS0011"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "appendonlydir"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "appendonlydir", "appendonly.aof.1.base.rdb"), []byte("REDIS0011"), 0o644))
		for _, f := range cli.seedFiles {
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, f), []byte("REDIS0011"), 0o644))
		}
	}

	cmd := exec.Command(shellPath, "scripts/server-start.sh", filepath.Join(dir, "valkey.conf"), "--port", "6379")
	// TMPDIR points nowhere: the root filesystem of the server container may
	// be read-only, so the script must not depend on a writable /tmp.
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "VALKEY_DATA_DIR="+dataDir, "TMPDIR=/nonexistent")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	_, statErr := os.Stat(filepath.Join(dataDir, "nodes.conf"))
	n := 0
	if b, err := os.ReadFile(calls); err == nil {
		n, _ = strconv.Atoi(string(b[:len(b)-1]))
	}
	return scriptRun{out: string(out), nodeKept: statErr == nil, dataDir: dataDir, cliCalls: n}
}

// assertSnapshotKept checks the snapshot files seeded by runStartScript.
func assertSnapshotKept(t *testing.T, r scriptRun, kept bool) {
	t.Helper()
	for _, f := range []string{"dump.rdb", "temp-42.rdb"} {
		if kept {
			assert.FileExists(t, filepath.Join(r.dataDir, f), f)
		} else {
			assert.NoFileExists(t, filepath.Join(r.dataDir, f), f)
		}
	}
	if kept {
		assert.DirExists(t, filepath.Join(r.dataDir, "appendonlydir"))
	} else {
		assert.NoDirExists(t, filepath.Join(r.dataDir, "appendonlydir"))
	}
}

// The server command of a cluster node runs serverStartCommand, which falls
// back to the server binary when the mounted ConfigMap has no server-start.sh,
// as an external ConfigMap may not. The script path is absolute, so this
// exercises the fallback on a machine without it.
func TestServerStartCommandFallsBackToServer(t *testing.T) {
	if _, err := os.Stat("/scripts/" + serverStartScriptKey); err == nil {
		t.Skip("/scripts/" + serverStartScriptKey + " exists on this machine")
	}
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "valkey-server"),
		[]byte("#!/bin/sh\necho \"valkey-server $*\"\n"), 0o755))
	cmd := exec.Command(shellPath, "-c", serverStartCommand, "valkey-server", "/config/valkey.conf", "--port", "6379")
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, "valkey-server /config/valkey.conf --port 6379\n", string(out))
}

func TestServerStartScript(t *testing.T) {
	const serverLine = "valkey-server"
	// Valkey 9 address format, with an empty hostname and a tls-port of 0.
	primary := primaryID + " 10.0.0.1:6379@16379,,tls-port=0,shard-id=abc myself,master - 0 1760000000000 1 connected 0-5460\n"
	replica := replicaID + " 10.0.0.2:6379@16379,,tls-port=0,shard-id=abc slave " + primaryID + " 0 1760000000000 1 connected\n"
	other := otherID + " 10.0.0.3:6379@16379,,tls-port=0,shard-id=def master - 0 1760000000000 2 connected 5461-10922\n"
	vars := "vars currentEpoch 3 lastVoteEpoch 0\n"
	conf := "cluster-node-timeout 1000\n" // the hand-off wait caps at six timeouts: 6s

	t.Run("no nodes.conf starts the server untouched", func(t *testing.T) {
		r := runStartScript(t, "", conf, stubCLI{})
		assert.Contains(t, r.out, serverLine)
		assert.False(t, r.nodeKept)
		assert.Zero(t, r.cliCalls)
	})

	t.Run("a primary with a replica waits for its promotion and starts as a new node", func(t *testing.T) {
		r := runStartScript(t, primary+replica+other+vars, conf, stubCLI{promotedAfter: 2})
		assert.Contains(t, r.out, "Previous run was primary "+primaryID+" with a replica")
		assert.Regexp(t, `Replica hand-off done after [0-9]+s; starting as a new node`, r.out)
		assert.Contains(t, r.out, serverLine)
		assert.False(t, r.nodeKept)
		assert.Equal(t, 3, r.cliCalls)
		assertSnapshotKept(t, r, false)
	})

	t.Run("a replica that follows another primary counts as moved on", func(t *testing.T) {
		r := runStartScript(t, primary+replica+other+vars, conf, stubCLI{promotedAfter: -1, primaryHost: "10.0.0.3"})
		assert.Contains(t, r.out, "starting as a new node")
		assert.False(t, r.nodeKept)
	})

	t.Run("the identity is kept when the replica has not moved on by the cap", func(t *testing.T) {
		r := runStartScript(t, primary+replica+vars, conf, stubCLI{promotedAfter: -1})
		assert.Contains(t, r.out, "Replica 10.0.0.2:6379 still follows this node")
		assert.Regexp(t, `Keeping the previous identity after [5-7]s`, r.out)
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
		assertSnapshotKept(t, r, true)
	})

	t.Run("a replica that does not answer is bounded per call and keeps the identity", func(t *testing.T) {
		r := runStartScript(t, primary+replica+vars, "cluster-node-timeout 500\n", stubCLI{promotedAfter: -1, hang: true})
		assert.Contains(t, r.out, "Keeping the previous identity")
		assert.True(t, r.nodeKept)
		assert.LessOrEqual(t, r.cliCalls, 3, "each hung call is cut after the per-call timeout")
	})

	t.Run("an IPv6 address keeps its colons when matched against ROLE", func(t *testing.T) {
		me := primaryID + " fd00::1:6379@16379,,tls-port=0,shard-id=abc myself,master - 0 1760000000000 1 connected 0-5460\n"
		them := replicaID + " fd00::2:6379@16379,,tls-port=0,shard-id=abc slave " + primaryID + " 0 1760000000000 1 connected\n"
		// The stub keeps following fd00::1, which must not pass as another primary.
		r := runStartScript(t, me+them+vars, conf, stubCLI{promotedAfter: -1, primaryHost: "fd00::1"})
		assert.Contains(t, r.out, "Keeping the previous identity")
		assert.True(t, r.nodeKept)
	})

	t.Run("the whole data dir is emptied, custom snapshot names included", func(t *testing.T) {
		r := runStartScript(t, primary+replica+vars, conf, stubCLI{seedFiles: []string{"custom.rdb"}})
		assert.False(t, r.nodeKept)
		entries, err := os.ReadDir(r.dataDir)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("a replica flagged fail does not count", func(t *testing.T) {
		failed := replicaID + " 10.0.0.2:6379@16379,,tls-port=0,shard-id=abc slave,fail " + primaryID + " 0 1760000000000 1 connected\n"
		r := runStartScript(t, primary+failed+other+vars, conf, stubCLI{})
		assert.NotContains(t, r.out, "Previous run")
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
		assert.Zero(t, r.cliCalls)
		assertSnapshotKept(t, r, true)
	})

	t.Run("a replica flagged nofailover does not count", func(t *testing.T) {
		nofailover := replicaID + " 10.0.0.2:6379@16379,,tls-port=0,shard-id=abc slave,nofailover " + primaryID + " 0 1760000000000 1 connected\n"
		r := runStartScript(t, primary+nofailover+vars, conf, stubCLI{})
		assert.NotContains(t, r.out, "Previous run")
		assert.True(t, r.nodeKept)
		assert.Zero(t, r.cliCalls)
	})

	t.Run("a primary without a replica keeps its identity", func(t *testing.T) {
		otherReplica := replicaID + " 10.0.0.2:6379@16379,valkey-c-1-1-0 slave " + otherID + " 0 1760000000000 2 connected\n"
		r := runStartScript(t, primary+other+otherReplica+vars, conf, stubCLI{})
		assert.NotContains(t, r.out, "Previous run")
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
	})

	t.Run("a replica keeps its identity", func(t *testing.T) {
		me := replicaID + " 10.0.0.2:6379@16379,valkey-c-0-1-0 myself,slave " + primaryID + " 0 1760000000000 1 connected\n"
		them := primaryID + " 10.0.0.1:6379@16379,valkey-c-0-0-0 master - 0 1760000000000 1 connected 0-5460\n"
		r := runStartScript(t, me+them+vars, conf, stubCLI{})
		assert.NotContains(t, r.out, "Previous run")
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
	})
}
