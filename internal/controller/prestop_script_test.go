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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubValkeyCLI answers INFO replication from $STUB_DIR/info (role:slave once
// $STUB_DIR/demoted exists), CONFIG GET with $STUB_TIMEOUT, and CLUSTER
// FAILOVER with OK, creating $STUB_DIR/demoted when $STUB_PROMOTE is set. Every
// call is appended to $STUB_DIR/calls as "host port command".
const stubValkeyCLI = `#!/bin/sh
host=""; port=""
while [ $# -gt 0 ]; do
    case "$1" in
        -h) host=$2; shift 2 ;;
        -p) port=$2; shift 2 ;;
        --user|--cacert|--cert|--key) shift 2 ;;
        --tls) shift ;;
        *) break ;;
    esac
done
echo "$host $port $*" >> "$STUB_DIR/calls"
case "$*" in
    "INFO replication")
        if [ -f "$STUB_DIR/demoted" ]; then printf 'role:slave\r\n'; else cat "$STUB_DIR/info"; fi ;;
    "CONFIG GET cluster-manual-failover-timeout")
        printf 'cluster-manual-failover-timeout\n%s\n' "$STUB_TIMEOUT" ;;
    "CLUSTER FAILOVER")
        echo OK
        if [ -n "$STUB_PROMOTE" ]; then touch "$STUB_DIR/demoted"; fi ;;
esac
`

type preStopRun struct {
	failovers []string // "host port" of each CLUSTER FAILOVER sent
	elapsed   time.Duration
}

func runPreStopScript(t *testing.T, info, timeoutMs string, promote bool) preStopRun {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "valkey-cli"), []byte(stubValkeyCLI), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "info"), []byte(info), 0o644))
	script, err := scripts.ReadFile("scripts/" + preStopScriptKey)
	require.NoError(t, err)
	scriptPath := filepath.Join(dir, preStopScriptKey)
	require.NoError(t, os.WriteFile(scriptPath, script, 0o755))

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"STUB_DIR="+dir,
		"STUB_TIMEOUT="+timeoutMs,
	)
	if promote {
		cmd.Env = append(cmd.Env, "STUB_PROMOTE=1")
	}
	start := time.Now()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the hook must always exit 0: %s", out)

	run := preStopRun{elapsed: time.Since(start)}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for line := range strings.SplitSeq(strings.TrimSpace(string(calls)), "\n") {
		if host, ok := strings.CutSuffix(line, " CLUSTER FAILOVER"); ok {
			run.failovers = append(run.failovers, host)
		}
	}
	return run
}

func TestPreStopFailoverScript(t *testing.T) {
	primary := "# Replication\r\nrole:master\r\nconnected_slaves:3\r\n" +
		"slave0:ip=10.0.0.2,port=6379,state=online,offset=90,lag=0\r\n" +
		"slave1:ip=10.0.0.3,port=6379,state=online,offset=120,lag=0\r\n" +
		"slave2:ip=10.0.0.4,port=6379,state=wait_bgsave,offset=500,lag=0\r\n"

	t.Run("a replica does nothing", func(t *testing.T) {
		run := runPreStopScript(t, "role:slave\r\n", "5000", false)
		assert.Empty(t, run.failovers)
	})

	t.Run("a primary without an online replica does nothing", func(t *testing.T) {
		info := "role:master\r\nconnected_slaves:1\r\nslave0:ip=10.0.0.4,port=6379,state=wait_bgsave,offset=500,lag=0\r\n"
		run := runPreStopScript(t, info, "5000", false)
		assert.Empty(t, run.failovers)
	})

	t.Run("a primary hands over to the online replica with the highest offset", func(t *testing.T) {
		run := runPreStopScript(t, primary, "5000", true)
		assert.Equal(t, []string{"10.0.0.3 6379"}, run.failovers)
		assert.Less(t, run.elapsed, 3*time.Second, "returns as soon as the node reports a replica")
	})

	t.Run("an IPv6 replica address stays whole", func(t *testing.T) {
		info := "role:master\r\nslave0:ip=fd00::7,port=6380,state=online,offset=10,lag=0\r\n"
		run := runPreStopScript(t, info, "5000", true)
		assert.Equal(t, []string{"fd00::7 6380"}, run.failovers)
	})

	t.Run("a handoff that does not finish ends after the failover timeout", func(t *testing.T) {
		run := runPreStopScript(t, primary, "1000", false)
		assert.Equal(t, []string{"10.0.0.3 6379"}, run.failovers)
		assert.GreaterOrEqual(t, run.elapsed, time.Second)
		assert.Less(t, run.elapsed, 5*time.Second)
	})
}
