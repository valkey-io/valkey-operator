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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	primaryID = "07c37dfeb235213a872192d90877d0cd55635b91"
	replicaID = "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca"
	otherID   = "67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1"
)

// scriptRun is one run of scripts/primary-handoff.sh against a temporary data
// dir, with stub valkey-server and valkey-cli binaries on PATH.
type scriptRun struct {
	out      string
	nodeKept bool   // nodes.conf survived
	dataDir  string // for further file checks
	cliCalls int    // all valkey-cli calls the replica stub answered
}

// stubCLI controls the stub valkey-cli: ROLE reports a replica of 10.0.0.1
// (or of primaryHost when set) promotedAfter times, then a primary. A
// negative promotedAfter keeps it a replica forever, and hang makes every
// call sleep past the script's per-call timeout.
type stubCLI struct {
	promotedAfter      int
	primaryHost        string
	hang               bool
	clusterNodes       *string // nil returns a promoted primary with slots
	clusterError       bool
	clusterHang        bool     // hang only the CLUSTER NODES calls
	clusterNodesSecond *string  // non-nil: second and later CLUSTER NODES answers
	seedFiles          []string // extra files to create in the data dir
}

// runPrimaryHandoffScript runs the script with the given nodes.conf and valkey.conf.
func runPrimaryHandoffScript(t *testing.T, nodesConf, valkeyConf string, cli stubCLI) scriptRun {
	t.Helper()
	cmd, dataDir, calls := primaryHandoffTestCommand(t, nodesConf, valkeyConf, cli)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	_, statErr := os.Stat(filepath.Join(dataDir, "nodes.conf"))
	n := 0
	if b, err := os.ReadFile(calls); err == nil {
		n, _ = strconv.Atoi(string(b[:len(b)-1]))
	}
	return scriptRun{out: string(out), nodeKept: statErr == nil, dataDir: dataDir, cliCalls: n}
}

// primaryHandoffTestCommand prepares the script run: a data dir with the given
// nodes.conf and snapshot files, stub binaries on PATH, and the command. It
// returns the command unstarted, the data dir and the file counting all CLI
// calls the stub answered.
func primaryHandoffTestCommand(t *testing.T, nodesConf, valkeyConf string, cli stubCLI) (cmd *exec.Cmd, dataDir, calls string) {
	t.Helper()
	dir := t.TempDir()
	dataDir = filepath.Join(dir, "data")
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "valkey-server"),
		[]byte("#!/bin/sh\necho \"valkey-server $*\"\n"), 0o755))
	calls = filepath.Join(dir, "calls")
	primaryHost := cli.primaryHost
	if primaryHost == "" {
		primaryHost = "10.0.0.1"
	}
	// The stub logs the password and arguments of each call to calls.auth.
	// With ROTATE_TO set it rewrites the password file after the first call,
	// as the kubelet does when the Secret rotates.
	script := "#!/bin/sh\n" +
		"n=$(cat " + calls + " 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + calls + "\n" +
		"echo \"${VALKEYCLI_AUTH:-} $*\" >> " + calls + ".auth\n" +
		"if [ $n -eq 1 ] && [ -n \"${ROTATE_TO:-}\" ]; then echo \"$ROTATE_TO\" > \"$VALKEY_OPERATOR_PASSWORD_FILE\"; fi\n"
	if cli.hang {
		script += "exec sleep 30\n"
	}
	nodes := replicaID + " 10.0.0.2:6379@16379 myself,master - 0 0 2 connected 0-5460\n"
	if cli.clusterNodes != nil {
		nodes = *cli.clusterNodes
	}
	nodesFile := filepath.Join(dir, "cluster-nodes")
	require.NoError(t, os.WriteFile(nodesFile, []byte(nodes), 0o644))
	nodesSecondFile := ""
	if cli.clusterNodesSecond != nil {
		nodesSecondFile = filepath.Join(dir, "cluster-nodes-2")
		require.NoError(t, os.WriteFile(nodesSecondFile, []byte(*cli.clusterNodesSecond), 0o644))
	}
	clusterExit := 0
	if cli.clusterError {
		clusterExit = 1
	}
	script += "previous=; last=\nfor arg do previous=$last; last=$arg; done\n" +
		"if [ \"$previous $last\" = 'CLUSTER NODES' ]; then " +
		"c=$(cat " + calls + "-cluster 2>/dev/null || echo 0); c=$((c+1)); echo $c > " + calls + "-cluster; "
	if cli.clusterHang {
		script += "exec sleep 30; fi\n"
	} else {
		script += "if [ $c -ge 2 ] && [ -n '" + nodesSecondFile + "' ] && [ -f '" + nodesSecondFile + "' ]; then cat '" + nodesSecondFile + "'; else cat " + nodesFile + "; fi; " +
			"exit " + strconv.Itoa(clusterExit) + "; fi\n"
	}
	script += "[ \"$last\" = ROLE ] || exit 1\n"
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

	cmd = exec.Command(shellPath, "scripts/primary-handoff.sh", filepath.Join(dir, "valkey.conf"), "--port", "6379")
	// TMPDIR points nowhere: the root filesystem of the server container may
	// be read-only, so the script must not depend on a writable /tmp.
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "VALKEY_DATA_DIR="+dataDir, "TMPDIR=/nonexistent")
	return cmd, dataDir, calls
}

// assertSnapshotKept checks the snapshot files seeded by runPrimaryHandoffScript.
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

// The server command of a cluster node runs replicationAuthShell(true), which
// falls back to the server binary when the mounted ConfigMap has no
// primary-handoff.sh, as an external ConfigMap may not. The script and password
// paths are absolute, so this exercises the fallback, with the password from
// the environment, on a machine without them.
func TestPrimaryHandoffCommandFallsBackToServer(t *testing.T) {
	for _, path := range []string{"/scripts/" + primaryHandoffScriptKey, systemPasswordMountPath + "/" + replicationUser} {
		if _, err := os.Stat(path); err == nil {
			t.Skip(path + " exists on this machine")
		}
	}
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "valkey-server"),
		[]byte("#!/bin/sh\necho \"valkey-server $*\"\n"), 0o755))
	cmd := exec.Command(shellPath, "-c", replicationAuthShell(true), "valkey-server", "/config/valkey.conf", "--port", "6379")
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "PRIMARY_AUTH=re-current")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, "valkey-server /config/valkey.conf --port 6379 --primaryauth re-current\n", string(out))
}

func TestPrimaryHandoffScript(t *testing.T) {
	const serverLine = "valkey-server"
	// Valkey 9 address format, with an empty hostname and a tls-port of 0.
	primary := primaryID + " 10.0.0.1:6379@16379,,tls-port=0,shard-id=abc myself,master - 0 1760000000000 1 connected 0-5460\n"
	replica := replicaID + " 10.0.0.2:6379@16379,,tls-port=0,shard-id=abc slave " + primaryID + " 0 1760000000000 1 connected\n"
	other := otherID + " 10.0.0.3:6379@16379,,tls-port=0,shard-id=def master - 0 1760000000000 2 connected 5461-10922\n"
	vars := "vars currentEpoch 3 lastVoteEpoch 0\n"
	conf := "cluster-node-timeout 1000\n" // the hand-off wait caps at six timeouts: 6s

	t.Run("no nodes.conf starts the server untouched", func(t *testing.T) {
		r := runPrimaryHandoffScript(t, "", conf, stubCLI{})
		assert.Contains(t, r.out, serverLine)
		assert.False(t, r.nodeKept)
		assert.Zero(t, r.cliCalls)
	})

	t.Run("a primary with a replica waits for its promotion and starts as a new node", func(t *testing.T) {
		r := runPrimaryHandoffScript(t, primary+replica+other+vars, conf, stubCLI{promotedAfter: 2})
		assert.Contains(t, r.out, "Previous run was primary "+primaryID+" with a replica")
		assert.Regexp(t, `Replica hand-off done after [0-9]+s; starting as a new node`, r.out)
		assert.Contains(t, r.out, serverLine)
		assert.False(t, r.nodeKept)
		assert.Equal(t, 4, r.cliCalls)
		assertSnapshotKept(t, r, false)
	})

	self := replicaID + " 10.0.0.2:6379@16379 master,myself - 0 0 2 connected"
	for _, tt := range []struct {
		name, nodes        string
		kept, clusterError bool
	}{
		{"transient master without slots keeps the identity", other + self + "\n", true, false},
		{"master with a single slot completes the handoff", other + self + " 5461\n", false, false},
		{"master with a slot range completes the handoff", other + self + " 5461-10922\r\n", false, false},
		{"master with only a migrating marker completes the handoff", self + " [5461->-" + otherID + "]\n", false, false},
		{"master with only an importing marker completes the handoff", self + " [5461-<-" + otherID + "]\n", false, false},
		{"a trailing CR is not a slot assignment", other + self + " \r\n", true, false},
		{"no myself line keeps the identity", other, true, false},
		{"a flag substring is not myself", other + strings.Replace(self, "myself", "notmyself", 1) + " 5461\n", true, false},
		{"an empty CLUSTER NODES keeps the identity", "", true, false},
		{"a truncated myself line keeps the identity", replicaID + " 10.0.0.2:6379@16379 myself,master - 0\n" + other, true, false},
		{"an invalid CLUSTER NODES keeps the identity", other + self + " invalid\n", true, false},
		{"invalid owned slots are discarded together", other + self + " 5461 invalid\n", true, false},
		{"a malformed bracket field is not an assignment", other + self + " 5461 [garbage]\n", true, false},
		{"a migration marker with a malformed field keeps the identity", other + self + " [5461->-" + otherID + "] garbage\n", true, false},
		{"a malformed field before a migration marker keeps the identity", other + self + " garbage [5461->-" + otherID + "]\n", true, false},
		{"a migration marker with a reversed range keeps the identity", other + self + " 10922-5461 [5461->-" + otherID + "]\n", true, false},
		{"a reversed slot range keeps the identity", other + self + " 10922-5461\n", true, false},
		{"an unreadable CLUSTER NODES keeps the identity", other + self + " 5461\n", true, true},
		{"slot 0 completes the handoff", self + " 0\n", false, false},
		{"slot 16383 completes the handoff", self + " 16383\n", false, false},
		{"the full slot range completes the handoff", self + " 0-16383\n", false, false},
		{"a migrating marker at slot 0 completes the handoff", self + " [0->-" + otherID + "]\n", false, false},
		{"an importing marker at slot 16383 completes the handoff", self + " [16383-<-" + otherID + "]\n", false, false},
		{"a leading-zero slot 16383 completes the handoff", self + " 00016383\n", false, false},
		{"many leading zeros preserve a valid slot", self + " " + strings.Repeat("0", 400) + "16383\n", false, false},
		{"slot 16384 keeps the identity", self + " 16384\n", true, false},
		{"range 0-16384 keeps the identity", self + " 0-16384\n", true, false},
		{"range 16384-20000 keeps the identity", self + " 16384-20000\n", true, false},
		{"an out-of-range migrating marker keeps the identity", self + " [16384->-" + otherID + "]\n", true, false},
		{"an out-of-range importing marker keeps the identity", self + " [16384-<-" + otherID + "]\n", true, false},
		{"a leading-zero slot 16384 keeps the identity", self + " 00016384\n", true, false},
		{"a valid slot before slot 16384 keeps the identity", self + " 0 16384\n", true, false},
		{"slot 16384 before a valid slot keeps the identity", self + " 16384 0\n", true, false},
		{"a valid slot before an out-of-range range keeps the identity", self + " 0 0-16384\n", true, false},
		{"an out-of-range range before a valid slot keeps the identity", self + " 0-16384 0\n", true, false},
		{"a valid slot before an out-of-range marker keeps the identity", self + " 0 [16384->-" + otherID + "]\n", true, false},
		{"an out-of-range marker before a valid slot keeps the identity", self + " [16384->-" + otherID + "] 0\n", true, false},
		{"a valid marker before slot 16384 keeps the identity", self + " [0->-" + otherID + "] 16384\n", true, false},
		{"slot 16384 before a valid marker keeps the identity", self + " 16384 [0->-" + otherID + "]\n", true, false},
		{"an overflowing slot keeps the identity", self + " " + strings.Repeat("9", 400) + "\n", true, false},
		{"an overflowing range endpoint keeps the identity", self + " 0-" + strings.Repeat("9", 400) + "\n", true, false},
		{"an overflowing marker slot keeps the identity", self + " [" + strings.Repeat("9", 400) + "->-" + otherID + "]\n", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := runPrimaryHandoffScript(t, primary+replica+other+vars, "cluster-node-timeout 1\n", stubCLI{clusterNodes: &tt.nodes, clusterError: tt.clusterError})
			assert.Equal(t, tt.kept, r.nodeKept, r.out)
			assertSnapshotKept(t, r, tt.kept)
			assert.Contains(t, r.out, serverLine)
			assert.LessOrEqual(t, r.cliCalls, 4, "the handoff wait stays bounded")
			if tt.kept {
				assert.Contains(t, r.out, "still follows this node")
				assert.Contains(t, r.out, "Keeping the previous identity")
				assert.NotContains(t, r.out, "starting as a new node")
			} else {
				assert.Contains(t, r.out, "starting as a new node")
				assert.Equal(t, 2, r.cliCalls, "ROLE and CLUSTER NODES both confirm promotion")
			}
		})
	}

	for _, tt := range []struct {
		name, value, wait string
	}{
		{"double-quoted timeout", `"5000"`, "30s"},
		{"single-quoted timeout", "'5000'", "30s"},
		{"invalid timeout falls back", "invalid", "90s"},
		{"large timeout caps the wait", "9223372036854775807", "300s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := runPrimaryHandoffScript(t, primary+replica+vars, "cluster-node-timeout "+tt.value+"\n", stubCLI{})
			assert.Contains(t, r.out, "waiting up to "+tt.wait)
			assert.Contains(t, r.out, serverLine)
		})
	}

	t.Run("a replica that follows another primary counts as moved on", func(t *testing.T) {
		r := runPrimaryHandoffScript(t, primary+replica+other+vars, conf, stubCLI{promotedAfter: -1, primaryHost: "10.0.0.3", clusterError: true})
		assert.Contains(t, r.out, "starting as a new node")
		assert.False(t, r.nodeKept)
		assert.Equal(t, 1, r.cliCalls, "a replica following another primary needs only ROLE")
	})

	t.Run("a hung CLUSTER NODES after a master ROLE is bounded and keeps the identity", func(t *testing.T) {
		r := runPrimaryHandoffScript(t, primary+replica+vars, "cluster-node-timeout 500\n", stubCLI{clusterHang: true})
		assert.Contains(t, r.out, "Keeping the previous identity")
		assert.True(t, r.nodeKept)
		assert.LessOrEqual(t, r.cliCalls, 6, "each hung call is cut after the per-call timeout")
	})

	t.Run("a transient master that gains slots on a later poll completes the handoff", func(t *testing.T) {
		self := replicaID + " 10.0.0.2:6379@16379 master,myself - 0 0 2 connected"
		empty := other + self + "\n"
		assigned := other + self + " 5461\n"
		r := runPrimaryHandoffScript(t, primary+replica+other+vars, "cluster-node-timeout 1000\n", stubCLI{clusterNodes: &empty, clusterNodesSecond: &assigned})
		assert.Contains(t, r.out, "starting as a new node")
		assert.False(t, r.nodeKept)
		assertSnapshotKept(t, r, false)
	})

	t.Run("the identity is kept when the replica has not moved on by the cap", func(t *testing.T) {
		r := runPrimaryHandoffScript(t, primary+replica+vars, conf, stubCLI{promotedAfter: -1})
		assert.Contains(t, r.out, "Replica 10.0.0.2:6379 still follows this node")
		assert.Regexp(t, `Keeping the previous identity after [5-7]s`, r.out)
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
		assertSnapshotKept(t, r, true)
	})

	t.Run("SIGTERM ends the wait with exit status 143", func(t *testing.T) {
		// The script is PID 1 in the container, where a shell ignores SIGTERM
		// unless it traps it, and the kubelet would wait out the grace period.
		cmd, dataDir, _ := primaryHandoffTestCommand(t, primary+replica+vars, "cluster-node-timeout 50000\n", stubCLI{promotedAfter: -1})
		log, err := os.Create(filepath.Join(filepath.Dir(dataDir), "log"))
		require.NoError(t, err)
		defer func() { _ = log.Close() }()
		cmd.Stdout, cmd.Stderr = log, log
		require.NoError(t, cmd.Start())
		require.Eventually(t, func() bool {
			b, _ := os.ReadFile(log.Name())
			return strings.Contains(string(b), "waiting up to 300s")
		}, 10*time.Second, 50*time.Millisecond)
		require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			assert.Equal(t, 143, exitErr.ExitCode())
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("the script did not exit on SIGTERM")
		}
		assert.FileExists(t, filepath.Join(dataDir, "nodes.conf"), "the identity is kept on an interrupted wait")
	})

	t.Run("a replica that does not answer is bounded per call and keeps the identity", func(t *testing.T) {
		r := runPrimaryHandoffScript(t, primary+replica+vars, "cluster-node-timeout 500\n", stubCLI{promotedAfter: -1, hang: true})
		assert.Contains(t, r.out, "Keeping the previous identity")
		assert.True(t, r.nodeKept)
		assert.LessOrEqual(t, r.cliCalls, 3, "each hung call is cut after the per-call timeout")
	})

	t.Run("an IPv6 address keeps its colons when matched against ROLE", func(t *testing.T) {
		me := primaryID + " fd00::1:6379@16379,,tls-port=0,shard-id=abc myself,master - 0 1760000000000 1 connected 0-5460\n"
		them := replicaID + " fd00::2:6379@16379,,tls-port=0,shard-id=abc slave " + primaryID + " 0 1760000000000 1 connected\n"
		// The stub keeps following fd00::1, which must not pass as another primary.
		r := runPrimaryHandoffScript(t, me+them+vars, conf, stubCLI{promotedAfter: -1, primaryHost: "fd00::1"})
		assert.Contains(t, r.out, "Keeping the previous identity")
		assert.True(t, r.nodeKept)
	})

	t.Run("the whole data dir is emptied, custom and hidden snapshot names included", func(t *testing.T) {
		r := runPrimaryHandoffScript(t, primary+replica+vars, conf, stubCLI{seedFiles: []string{"custom.rdb", ".snapshot.rdb"}})
		assert.False(t, r.nodeKept)
		entries, err := os.ReadDir(r.dataDir)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("a replica flagged fail does not count", func(t *testing.T) {
		failed := replicaID + " 10.0.0.2:6379@16379,,tls-port=0,shard-id=abc slave,fail " + primaryID + " 0 1760000000000 1 connected\n"
		r := runPrimaryHandoffScript(t, primary+failed+other+vars, conf, stubCLI{})
		assert.NotContains(t, r.out, "Previous run")
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
		assert.Zero(t, r.cliCalls)
		assertSnapshotKept(t, r, true)
	})

	t.Run("a replica flagged nofailover is waited for, since the operator takes it over", func(t *testing.T) {
		nofailover := replicaID + " 10.0.0.2:6379@16379,,tls-port=0,shard-id=abc slave,nofailover " + primaryID + " 0 1760000000000 1 connected\n"
		r := runPrimaryHandoffScript(t, primary+nofailover+vars, conf, stubCLI{promotedAfter: 2})
		assert.Contains(t, r.out, "Previous run was primary "+primaryID+" with a replica")
		assert.Contains(t, r.out, "starting as a new node")
		assert.False(t, r.nodeKept)
	})

	t.Run("a primary without a replica keeps its identity", func(t *testing.T) {
		otherReplica := replicaID + " 10.0.0.2:6379@16379,valkey-c-1-1-0 slave " + otherID + " 0 1760000000000 2 connected\n"
		r := runPrimaryHandoffScript(t, primary+other+otherReplica+vars, conf, stubCLI{})
		assert.NotContains(t, r.out, "Previous run")
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
	})

	t.Run("a replica keeps its identity", func(t *testing.T) {
		me := replicaID + " 10.0.0.2:6379@16379,valkey-c-0-1-0 myself,slave " + primaryID + " 0 1760000000000 1 connected\n"
		them := primaryID + " 10.0.0.1:6379@16379,valkey-c-0-0-0 master - 0 1760000000000 1 connected 0-5460\n"
		r := runPrimaryHandoffScript(t, me+them+vars, conf, stubCLI{})
		assert.NotContains(t, r.out, "Previous run")
		assert.Contains(t, r.out, serverLine)
		assert.True(t, r.nodeKept)
	})

	t.Run("ROLE calls send the mounted operator password and pick up a rotation", func(t *testing.T) {
		cmd, _, calls := primaryHandoffTestCommand(t, primary+replica+vars, conf, stubCLI{promotedAfter: 2})
		passwordFile := filepath.Join(filepath.Dir(calls), operatorUser)
		require.NoError(t, os.WriteFile(passwordFile, []byte("op-mounted\n"), 0o644))
		cmd.Env = append(cmd.Env, "VALKEY_USER="+operatorUser, "VALKEYCLI_AUTH=op-env",
			"VALKEY_OPERATOR_PASSWORD_FILE="+passwordFile, "ROTATE_TO=op-rotated")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		auth, err := os.ReadFile(calls + ".auth")
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSpace(string(auth)), "\n")
		// The third ROLE reports a master, and the slot check that follows sends
		// the rotated password with CLUSTER NODES too.
		require.Len(t, lines, 4)
		for i, want := range []string{"op-mounted", "op-rotated", "op-rotated", "op-rotated"} {
			assert.True(t, strings.HasPrefix(lines[i], want+" "), lines[i])
			assert.Contains(t, lines[i], "--user "+operatorUser)
		}
	})

	t.Run("the slot check reloads the operator password after a rotation", func(t *testing.T) {
		cmd, _, calls := primaryHandoffTestCommand(t, primary+replica+vars, conf, stubCLI{promotedAfter: 0})
		passwordFile := filepath.Join(filepath.Dir(calls), operatorUser)
		require.NoError(t, os.WriteFile(passwordFile, []byte("op-mounted\n"), 0o644))
		cmd.Env = append(cmd.Env, "VALKEY_USER="+operatorUser, "VALKEYCLI_AUTH=op-env",
			"VALKEY_OPERATOR_PASSWORD_FILE="+passwordFile, "ROTATE_TO=op-rotated")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		auth, err := os.ReadFile(calls + ".auth")
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSpace(string(auth)), "\n")
		// The first ROLE already reports a master, so the rotation lands between
		// that ROLE and the CLUSTER NODES the slot check sends.
		require.Len(t, lines, 2)
		for i, want := range []string{"op-mounted", "op-rotated"} {
			assert.True(t, strings.HasPrefix(lines[i], want+" "), lines[i])
			assert.Contains(t, lines[i], "--user "+operatorUser)
		}
	})

	t.Run("ROLE calls fall back to the environment password without the mount", func(t *testing.T) {
		cmd, _, calls := primaryHandoffTestCommand(t, primary+replica+vars, conf, stubCLI{promotedAfter: 0})
		cmd.Env = append(cmd.Env, "VALKEY_USER="+operatorUser, "VALKEYCLI_AUTH=op-env",
			"VALKEY_OPERATOR_PASSWORD_FILE="+filepath.Join(filepath.Dir(calls), "missing"))
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		auth, err := os.ReadFile(calls + ".auth")
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(auth), "op-env "), string(auth))
		assert.Contains(t, string(auth), "--user "+operatorUser)
	})
}

func TestScriptsReadTheMountedOperatorPassword(t *testing.T) {
	for _, name := range []string{"liveness-check.sh", "readiness-check.sh", primaryHandoffScriptKey} {
		body, err := scripts.ReadFile("scripts/" + name)
		require.NoError(t, err)
		assert.Contains(t, string(body), systemPasswordMountPath+"/"+operatorUser, name)
	}
}
