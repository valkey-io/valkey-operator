//go:build e2e

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

package e2e

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"valkey.io/valkey-operator/test/utils"
)

// keyCount is the number of keys written across the keyspace before the
// failover, and verified afterwards to prove no data was lost.
const keyCount = 50

// failoverDefaultPassword is the password configured for the default user of
// the failover test clusters (via a passwordSecret, following the pattern
// introduced in #292), so valkey-cli commands run authenticated.
const failoverDefaultPassword = "e2eFailoverPassw0rd"

var _ = Describe("Shutdown-on-SIGTERM failover", Label("failover"), func() {
	var valkeyName string

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			utils.CollectDebugInfo(namespace)
		}

		By("cleaning up test resources")
		cmd := exec.Command("kubectl", "delete", "valkeycluster", valkeyName, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "secret", valkeyName+"-users", "--ignore-not-found=true")
		_, _ = utils.Run(cmd)
	})

	It("promotes a replica before a gracefully terminated primary exits", func() {
		valkeyName = "failover-sigterm"
		createFailoverCluster(valkeyName)

		By("identifying the primary and replica of shard 0")
		var primaryPod, replicaPod string
		Eventually(func(g Gomega) {
			primaryPod, replicaPod = getShardRoles(g, valkeyName, 0)
			g.Expect(primaryPod).NotTo(BeEmpty(), "shard 0 has no primary")
			g.Expect(replicaPod).NotTo(BeEmpty(), "shard 0 has no in-sync replica")
		}).WithTimeout(3 * time.Minute).Should(Succeed())

		By("recording the primary pod UID so its replacement can be detected")
		cmd := exec.Command("kubectl", "get", "pod", primaryPod, "-o", "jsonpath={.metadata.uid}")
		oldPrimaryUID, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to get primary pod UID")
		Expect(oldPrimaryUID).NotTo(BeEmpty())

		By("writing keys across the keyspace")
		writeTestKeys(primaryPod)

		By("starting a continuous writer to run through the disruption")
		writer := startContinuousWriter(replicaPod)

		By("gracefully terminating the primary pod (SIGTERM with default grace period)")
		cmd = exec.Command("kubectl", "delete", "pod", primaryPod, "--wait=false")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete primary pod")

		// The handover must complete inside terminationGracePeriodSeconds
		// (default 30s), so the replica has to report role:master within that
		// window; otherwise SIGKILL would have cut the handoff short.
		By("asserting the replica is promoted within the grace period")
		Eventually(func(g Gomega) {
			output, err := execValkeyPodShell(replicaPod, "valkey-cli INFO replication")
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get replication info from replica")
			g.Expect(output).To(ContainSubstring("role:master"),
				"replica was not promoted to primary")
		}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())

		// The graceful handoff is a coordinated failover driven by the
		// terminating primary (CLUSTER FAILOVER FORCE REPLICAID), which logs
		// "Forced failover primary request accepted" on the promoted replica.
		// The crash path goes through FAIL detection and a rank-based election
		// instead. valkey 9.0's replica selection on shutdown is best-effort
		// (it requires exact ack-offset equality at the shutdown instant and
		// intermittently falls back to the crash path, ~1 in 3 under write
		// load in local testing), so the handoff path is reported rather than
		// hard-asserted until the promotion is made deterministic.
		By("reporting which failover path drove the promotion")
		cmd = exec.Command("kubectl", "logs", replicaPod, "-c", "server")
		logs, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to get promoted replica logs")
		if strings.Contains(strings.ToLower(logs), "forced failover primary request accepted") {
			_, _ = fmt.Fprintf(GinkgoWriter, "promotion path: coordinated shutdown handoff (forced failover)\n")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"promotion path: failure detection (shutdown handoff did not engage; see 'Unable to find a replica' on the primary)\n")
		}

		By("asserting every write acknowledged during the disruption is readable")
		acked, maxGap := writer.stop()
		Expect(acked).NotTo(BeEmpty(), "continuous writer recorded no acknowledged writes")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"continuous writer: %d acknowledged writes, longest gap between acknowledged writes: %.2fs\n",
			len(acked), maxGap)
		verifyAcknowledgedWrites(replicaPod, acked)

		// The orderly handoff keeps a writer available throughout the
		// disruption; a crash-style failover leaves the shard writer-less for
		// failure detection plus an election. The bound is deliberately
		// generous to absorb CI noise.
		Expect(maxGap).To(BeNumerically("<", 10.0),
			fmt.Sprintf("shard had no writer for %.2fs during the graceful handoff", maxGap))

		By("waiting for the replaced pod to come back and rejoin as replica")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", primaryPod, "-o", "jsonpath={.metadata.uid}")
			uid, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "replacement pod does not exist yet")
			g.Expect(uid).NotTo(Equal(oldPrimaryUID), "old pod is still terminating")

			output, err := execValkeyPodShell(primaryPod, "valkey-cli INFO replication")
			g.Expect(err).NotTo(HaveOccurred(), "Failed to get replication info from replaced pod")
			g.Expect(output).To(ContainSubstring("role:slave"),
				"replaced pod did not rejoin the shard as a replica")
		}).WithTimeout(4 * time.Minute).Should(Succeed())

		verifyClusterHealthyAndKeysIntact(valkeyName, replicaPod)
	})

})

// createFailoverCluster creates a ValkeyCluster with one replica per shard
// and a password-protected default user, and waits for it to become Ready.
func createFailoverCluster(valkeyName string) {
	GinkgoHelper()

	By("creating a ValkeyCluster with one replica per shard")
	valkeyYaml := fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %[1]s-users
data:
  defaultpw: %[2]s
---
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %[1]s
spec:
  shards: 3
  replicas: 1
  users:
    - name: default
      enabled: true
      permissions: "+@all ~* &*"
      passwordSecret:
        name: %[1]s-users
        keys: [defaultpw]
`, valkeyName, base64.StdEncoding.EncodeToString([]byte(failoverDefaultPassword)))

	manifestFile := filepath.Join(os.TempDir(), fmt.Sprintf("%s.yaml", valkeyName))
	err := os.WriteFile(manifestFile, []byte(valkeyYaml), 0644)
	Expect(err).NotTo(HaveOccurred(), "Failed to write manifest file")
	defer func() {
		Expect(os.Remove(manifestFile)).To(Succeed())
	}()

	cmd := exec.Command("kubectl", "create", "-f", manifestFile)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create ValkeyCluster CR")

	By("waiting for the cluster to become Ready")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "valkeycluster", valkeyName,
			"-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to get ValkeyCluster Ready condition")
		g.Expect(output).To(Equal("True"), "ValkeyCluster is not Ready")
	}).WithTimeout(4 * time.Minute).Should(Succeed())
}

// writeTestKeys writes keyCount keys across the keyspace via the given pod.
func writeTestKeys(pod string) {
	GinkgoHelper()

	script := fmt.Sprintf(
		"ok=0; for i in $(seq 1 %d); do "+
			"r=$(valkey-cli -c set e2e:failover:$i v$i); "+
			"[ \"$r\" = \"OK\" ] && ok=$((ok+1)); "+
			"done; echo written=$ok", keyCount)
	output, err := execValkeyPodShell(pod, script)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to write keys: %s", output))
	Expect(output).To(ContainSubstring(fmt.Sprintf("written=%d", keyCount)),
		fmt.Sprintf("Not all keys were written: %s", output))
}

// verifyClusterHealthyAndKeysIntact asserts the ValkeyCluster returns to
// Ready, the cluster reports a healthy state, and every test key survived the
// disruption.
func verifyClusterHealthyAndKeysIntact(valkeyName, pod string) {
	GinkgoHelper()

	By("asserting the ValkeyCluster returns to Ready")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "valkeycluster", valkeyName,
			"-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to get ValkeyCluster Ready condition")
		g.Expect(output).To(Equal("True"), "ValkeyCluster did not return to Ready")
	}).Should(Succeed())

	By("asserting the cluster reports a healthy state")
	Eventually(func(g Gomega) {
		output, err := execValkeyPodShell(pod, "valkey-cli CLUSTER INFO")
		g.Expect(err).NotTo(HaveOccurred(), "Failed to get cluster info")
		g.Expect(output).To(ContainSubstring("cluster_state:ok"))
	}).Should(Succeed())

	By("asserting no keys were lost across the disruption")
	script := fmt.Sprintf(
		"ok=0; for i in $(seq 1 %d); do "+
			"v=$(valkey-cli -c get e2e:failover:$i); "+
			"[ \"$v\" = \"v$i\" ] && ok=$((ok+1)); "+
			"done; echo readable=$ok", keyCount)
	Eventually(func(g Gomega) {
		output, err := execValkeyPodShell(pod, script)
		g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to read keys back: %s", output))
		g.Expect(output).To(ContainSubstring(fmt.Sprintf("readable=%d", keyCount)),
			fmt.Sprintf("Some keys were lost across the disruption: %s", output))
	}).Should(Succeed())
}

// continuousWriter tracks a background write loop running inside a pod.
type continuousWriter struct {
	cmd    *exec.Cmd
	output *strings.Builder
}

// startContinuousWriter starts a background loop inside the pod that writes
// uniquely-numbered keys through the cluster for writerDurationSeconds,
// recording a timestamped ack/fail line per attempt. It is started against
// the shard's replica so cluster-mode redirects follow the primary across the
// handoff.
const writerDurationSeconds = 20

func startContinuousWriter(pod string) *continuousWriter {
	GinkgoHelper()

	// POSIX-sh only: the image's /bin/sh is dash, which has no $SECONDS.
	script := fmt.Sprintf(
		"end=$(($(date +%%s)+%d)); i=0; "+
			"while [ \"$(date +%%s)\" -lt \"$end\" ]; do "+
			"r=$(valkey-cli -c set e2e:cw:$i v$i 2>/dev/null | tail -n 1); "+
			"if [ \"$r\" = \"OK\" ]; then echo \"ack $i $(date +%%s.%%N)\"; "+
			"else echo \"fail $i $(date +%%s.%%N)\"; fi; "+
			"i=$((i+1)); "+
			"done", writerDurationSeconds)
	cmd := exec.Command("kubectl", "exec", pod, "-c", "server", "--",
		"sh", "-c", fmt.Sprintf("export VALKEYCLI_AUTH=%q; ", failoverDefaultPassword)+script)

	w := &continuousWriter{cmd: cmd, output: &strings.Builder{}}
	cmd.Stdout = w.output
	cmd.Stderr = w.output
	Expect(cmd.Start()).To(Succeed(), "Failed to start continuous writer")
	return w
}

// stop waits for the writer loop to finish and returns the map of
// acknowledged key indices to their expected values, plus the longest gap in
// seconds between consecutive acknowledged writes (the shard's effective
// write-unavailability window).
func (w *continuousWriter) stop() (acked map[string]string, maxGap float64) {
	GinkgoHelper()

	Expect(w.cmd.Wait()).To(Succeed(), "continuous writer failed: %s", w.output.String())

	acked = map[string]string{}
	lastAckTime := -1.0
	for _, line := range utils.GetNonEmptyLines(w.output.String()) {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		status, idx := fields[0], fields[1]
		var ts float64
		if _, err := fmt.Sscanf(fields[2], "%f", &ts); err != nil {
			continue
		}
		if status != "ack" {
			continue
		}
		if lastAckTime >= 0 && ts-lastAckTime > maxGap {
			maxGap = ts - lastAckTime
		}
		lastAckTime = ts
		acked[idx] = "v" + idx
	}
	return acked, maxGap
}

// verifyAcknowledgedWrites asserts that every key the continuous writer got
// an OK for is readable with the expected value — i.e. no acknowledged write
// was dropped during the handoff.
func verifyAcknowledgedWrites(pod string, acked map[string]string) {
	GinkgoHelper()

	maxIdx := 0
	for idx := range acked {
		var i int
		_, err := fmt.Sscanf(idx, "%d", &i)
		Expect(err).NotTo(HaveOccurred())
		if i > maxIdx {
			maxIdx = i
		}
	}

	script := fmt.Sprintf(
		"for i in $(seq 0 %d); do v=$(valkey-cli -c get e2e:cw:$i 2>/dev/null | tail -n 1); echo \"$i $v\"; done", maxIdx)
	output, err := execValkeyPodShell(pod, script)
	Expect(err).NotTo(HaveOccurred(), "Failed to read back acknowledged writes")

	readable := map[string]string{}
	for _, line := range utils.GetNonEmptyLines(output) {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			readable[fields[0]] = fields[1]
		}
	}

	var lost []string
	for idx, want := range acked {
		if readable[idx] != want {
			lost = append(lost, idx)
		}
	}
	Expect(lost).To(BeEmpty(),
		fmt.Sprintf("%d acknowledged write(s) were lost across the handoff: %v", len(lost), lost))
}

// execValkeyPodShell runs a shell script inside the pod's server container
// with VALKEYCLI_AUTH set to the default user's password, so every valkey-cli
// invocation in the script runs authenticated. The operator injects
// VALKEYCLI_AUTH with the _operator user's password for the probe scripts;
// it must be overridden here because valkey-cli auto-sends it as the default
// user's AUTH credential.
func execValkeyPodShell(pod string, script string) (string, error) {
	cmd := exec.Command("kubectl", "exec", pod, "-c", "server", "--",
		"sh", "-c", fmt.Sprintf("export VALKEYCLI_AUTH=%q; ", failoverDefaultPassword)+script)
	return utils.Run(cmd)
}

// getShardRoles returns the pod names of the primary and replica of the given
// shard. Roles are read live from INFO replication on each pod rather than
// from ValkeyNode status, which can lag behind role changes (see #261). The
// replica is only accepted once its replication link is up, so the failover
// is not attempted against a still-syncing replica.
func getShardRoles(g Gomega, clusterName string, shardIndex int) (primaryPod, replicaPod string) {
	cmd := exec.Command("kubectl", "get", "pods",
		"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/shard-index=%d", clusterName, shardIndex),
		"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to list shard pods")

	for _, pod := range utils.GetNonEmptyLines(output) {
		pod = strings.TrimSpace(pod)
		info, err := execValkeyPodShell(pod, "valkey-cli INFO replication")
		g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to get replication info from %s", pod))
		switch {
		case strings.Contains(info, "role:master"):
			primaryPod = pod
		case strings.Contains(info, "role:slave") && strings.Contains(info, "master_link_status:up"):
			replicaPod = pod
		}
	}
	return primaryPod, replicaPod
}
