//go:build e2e
// +build e2e

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
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

// The backup specs run a backup Job by hand from a suspended CronJob, so a
// spec never waits for a schedule: the schedule below never comes round in
// practice, and suspend keeps it from firing if it does.
const (
	backupSchedule = "0 0 1 1 *"

	// backupKeyCount keys spread over every shard. They occupy at most this
	// many of the 16384 slots, so a restore always leaves empty slots for
	// the operator to fill.
	backupKeyCount  = 1000
	backupKeyPrefix = "e2e:backup:"

	backupClusterReadyTimeout = 5 * time.Minute
	backupJobTimeout          = 5 * time.Minute
)

// Covers spec.backup and spec.restoreFrom end to end against a real
// S3-compatible server: a snapshot taken from the replicas of a running
// cluster seeds a new cluster with the same data, and the restore guard
// keeps a restarting pod from loading the snapshot again.
var _ = Describe("ValkeyCluster backup and restore", Ordered, Label("ValkeyCluster", "Backup"), func() {
	const (
		s3Namespace     = "s3-e2e-restore"
		credentials     = "s3-e2e-restore-credentials"
		prefix          = "e2e-backup"
		sourceCluster   = "backup-source"
		restoredCluster = "backup-restored"
		mismatchCluster = "backup-mismatch"
		shards          = 3
		replicas        = 1
		afterBackupKey  = "e2e:after-backup"
		afterRestoreKey = "e2e:after-restore"
	)

	// snapshot is the snapshot the manual backup took, which the restore
	// specs seed their clusters from.
	var snapshot string

	BeforeAll(func() {
		installS3Server(s3Namespace)
		createS3CredentialsSecret(credentials)

		By("creating the source cluster with a suspended backup schedule")
		deleteBackupCluster(sourceCluster)
		applyBackupManifest(backupClusterManifest(sourceCluster, shards, replicas,
			backupSpecYAML(s3Endpoint(s3Namespace), prefix, credentials, 0)))
		waitBackupClusterReady(sourceCluster, shards)

		By("writing keys across every shard")
		writeBackupKeys(backupShardPod(sourceCluster, 0, 0), backupKeyCount)
		Eventually(func(g Gomega) {
			for shard := range shards {
				primary := backupShardPrimary(g, sourceCluster, shard, replicas)
				g.Expect(backupDBSize(g, primary)).To(BeNumerically(">", 0),
					fmt.Sprintf("shard %d holds no keys, the snapshot would not cover it", shard))
			}
		}).Should(Succeed())

		// The backup reads each shard from a replica, so a replica that has
		// not caught up yet would leave keys out of the snapshot.
		waitBackupReplicasCaughtUp(sourceCluster, shards, replicas)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
			dumpS3Server(s3Namespace)
			dumpRestoreLogs(restoredCluster, shards)
			dumpRestoreLogs(mismatchCluster, 2)
		}
	})

	AfterAll(func() {
		for _, name := range []string{sourceCluster, restoredCluster, mismatchCluster} {
			cmd := exec.Command("kubectl", "delete", "valkeycluster", name, "--ignore-not-found=true", "--wait=false")
			_, _ = utils.Run(cmd)
		}
	})

	It("renders the backup CronJob and its scripts owned by the cluster", func() {
		name := backupResourceName(sourceCluster)
		for _, kind := range []string{"cronjob", "configmap"} {
			owner := kubectlJSONPath(Default, kind, name,
				"{.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}")
			Expect(owner).To(Equal("ValkeyCluster/"+sourceCluster), fmt.Sprintf("%s %s owner", kind, name))
		}
		Expect(kubectlJSONPath(Default, "cronjob", name, "{.spec.schedule}")).To(Equal(backupSchedule))
		Expect(kubectlJSONPath(Default, "cronjob", name, "{.spec.suspend}")).To(Equal("true"))
		Expect(kubectlJSONPath(Default, "cronjob", name, "{.spec.concurrencyPolicy}")).To(Equal("Forbid"))
	})

	It("uploads one RDB per shard and a manifest covering every slot", func() {
		snapshot, _ = runBackupJob(sourceCluster, "backup-source-manual")

		By("verifying the snapshot holds exactly the shard RDBs and the manifest")
		Expect(listSnapshots(Default, s3Namespace, prefix)).To(Equal([]string{snapshot}))
		Expect(snapshotFiles(Default, s3Namespace, prefix, snapshot)).To(Equal(
			[]string{"manifest.json", "shard-0.rdb", "shard-1.rdb", "shard-2.rdb"}))
		for shard := range shards {
			file := fmt.Sprintf("shard-%d.rdb", shard)
			magic, size := readSnapshotFile(Default, s3Namespace, prefix, snapshot, file, 5)
			Expect(strconv.Atoi(size)).To(BeNumerically(">", 0), file+" is empty")
			// RDB files start with "REDIS", or "VALKEY" from RDB version 80.
			Expect(magic).To(Or(Equal("REDIS"), Equal("VALKE")), file+" is not an RDB file")
		}

		By("verifying the manifest")
		raw, _ := readSnapshotFile(Default, s3Namespace, prefix, snapshot, "manifest.json", 0)
		var manifest snapshotManifest
		Expect(json.Unmarshal([]byte(raw), &manifest)).To(Succeed(), "manifest.json: "+raw)
		Expect(manifest.Version).To(Equal(1))
		Expect(manifest.Cluster).To(Equal(sourceCluster))
		Expect(manifest.Shards).To(HaveLen(shards))

		primaries := backupPrimaryIDs(backupShardPod(sourceCluster, 0, 0))
		covered := make([]int, 16384)
		for i, shard := range manifest.Shards {
			Expect(shard.Index).To(Equal(i))
			Expect(shard.File).To(Equal(fmt.Sprintf("shard-%d.rdb", i)))
			Expect(shard.SourceRole).To(Equal("replica"), fmt.Sprintf("shard %d was not read from a replica", i))
			Expect(shard.Source).NotTo(Equal(shard.Primary))
			Expect(primaries).To(ContainElement(shard.Primary))
			for _, r := range strings.Fields(shard.Slots) {
				start, end := parseBackupSlotRange(r)
				for slot := start; slot <= end; slot++ {
					covered[slot]++
				}
			}
		}
		for slot, n := range covered {
			Expect(n).To(Equal(1), fmt.Sprintf("slot %d is covered %d times by the manifest", slot, n))
		}

		By("writing a key after the snapshot, which a restore must not bring back")
		out, err := backupValkeyShell(backupShardPod(sourceCluster, 0, 0), "valkey-cli -c set "+afterBackupKey+" x")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("OK"))
	})

	It("seeds a new cluster with the same number of shards from the snapshot", func() {
		Expect(snapshot).NotTo(BeEmpty(), "needs the snapshot of the previous spec")

		By("creating a cluster that restores from the snapshot")
		deleteBackupCluster(restoredCluster)
		applyBackupManifest(backupClusterManifest(restoredCluster, shards, replicas,
			restoreSpecYAML(s3Endpoint(s3Namespace), credentials, prefix+"/"+snapshot)))

		By("verifying only the first node of each shard restores")
		Eventually(func(g Gomega) {
			for shard := range shards {
				g.Expect(kubectlJSONPath(g, "pod", backupShardPod(restoredCluster, shard, 0), "{.spec.initContainers[*].name}")).
					To(Equal("restore-fetch restore-install"))
				g.Expect(kubectlJSONPath(g, "pod", backupShardPod(restoredCluster, shard, 1), "{.spec.initContainers[*].name}")).
					To(BeEmpty())
			}
		}).Should(Succeed())

		waitBackupClusterReady(restoredCluster, shards)
		waitBackupReplicasCaughtUp(restoredCluster, shards, replicas)

		By("verifying each first node loaded its shard's RDB")
		for shard := range shards {
			logs, err := backupInitContainerLogs(backupShardPod(restoredCluster, shard, 0), "restore-install")
			Expect(err).NotTo(HaveOccurred())
			Expect(logs).To(ContainSubstring(fmt.Sprintf("loaded shard-%d.rdb as", shard)))
		}

		By("verifying the restored cluster serves every slot and the snapshot's data")
		pod := backupShardPod(restoredCluster, 0, 0)
		info, err := backupValkeyShell(pod, "valkey-cli cluster info")
		Expect(err).NotTo(HaveOccurred())
		Expect(info).To(ContainSubstring("cluster_state:ok"))
		Expect(info).To(ContainSubstring("cluster_slots_assigned:16384"))
		Eventually(func(g Gomega) {
			g.Expect(backupPrimaryDBSizeSum(g, restoredCluster, shards, replicas)).To(Equal(backupKeyCount))
		}).Should(Succeed())
		verifyBackupKeys(pod, backupKeyCount)
		out, err := backupValkeyShell(pod, "valkey-cli -c exists "+afterBackupKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("0"), "a key written after the snapshot was restored")
		out, err = backupValkeyShell(pod, "valkey-cli -c set "+afterRestoreKey+" x")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("OK"))

		By("verifying the operator assigned the slots the snapshot left unowned")
		Eventually(func() (int, error) {
			return countBackupClusterEvents(restoredCluster, "SlotGapsFilled")
		}).Should(BeNumerically(">=", 1))
	})

	It("does not load the snapshot again when a restored primary restarts", func() {
		// The previous spec wrote a key; it has to be on the replica before
		// the primary goes, or the count below depends on the failover.
		waitBackupReplicasCaughtUp(restoredCluster, shards, replicas)

		pod := backupShardPod(restoredCluster, 0, 0)
		info, err := backupValkeyShell(pod, "valkey-cli info replication")
		Expect(err).NotTo(HaveOccurred())
		if !strings.Contains(info, "role:master") {
			Fail(fmt.Sprintf("%s loaded the snapshot but is not a primary after the restore:\n%s", pod, info))
		}
		oldUID := kubectlJSONPath(Default, "pod", pod, "{.metadata.uid}")

		By("deleting the restored primary's pod")
		cmd := exec.Command("kubectl", "delete", "pod", pod, "--wait=true", "--timeout=3m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for its replacement to finish the restore init containers")
		Eventually(func(g Gomega) {
			g.Expect(kubectlJSONPath(g, "pod", pod, "{.metadata.uid}")).NotTo(Equal(oldUID))
			g.Expect(kubectlJSONPath(g, "pod", pod, "{.status.initContainerStatuses[*].state.terminated.exitCode}")).
				To(Equal("0 0"))
		}, backupClusterReadyTimeout).Should(Succeed())

		By("verifying the snapshot was staged again but the guard kept it out")
		// The data dir is an emptyDir, so the fetch stages the RDB again;
		// only the install guard, seeing the shard's slots served, keeps it
		// out. "not restoring" alone would also match its other exits.
		logs, err := backupInitContainerLogs(pod, "restore-fetch")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("staged shard-0.rdb"))
		logs, err = backupInitContainerLogs(pod, "restore-install")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(And(ContainSubstring("are already served by"), ContainSubstring("not restoring")))

		By("verifying the cluster recovers with its data")
		waitBackupClusterReady(restoredCluster, shards)
		waitBackupReplicasCaughtUp(restoredCluster, shards, replicas)
		Eventually(func(g Gomega) {
			g.Expect(backupPrimaryDBSizeSum(g, restoredCluster, shards, replicas)).To(Equal(backupKeyCount + 1))
		}).Should(Succeed())
	})

	It("refuses a snapshot with a different number of shards", func() {
		Expect(snapshot).NotTo(BeEmpty(), "needs the snapshot of the earlier spec")

		By("creating a two-shard cluster that restores from the three-shard snapshot")
		deleteBackupCluster(mismatchCluster)
		applyBackupManifest(backupClusterManifest(mismatchCluster, 2, 0,
			restoreSpecYAML(s3Endpoint(s3Namespace), credentials, prefix+"/"+snapshot)))

		// The pod restarts the failed init container, so its status swings
		// between Init:Error and Init:CrashLoopBackOff; the exit code is
		// what stays.
		pod := backupShardPod(mismatchCluster, 0, 0)
		By("waiting for the fetch to fail")
		Eventually(func(g Gomega) {
			fetch := `{.status.initContainerStatuses[?(@.name=="restore-fetch")]`
			current := kubectlJSONPath(g, "pod", pod, fetch+".state.terminated.exitCode}")
			last := kubectlJSONPath(g, "pod", pod, fetch+".lastState.terminated.exitCode}")
			g.Expect([]string{current, last}).To(ContainElement(SatisfyAll(Not(BeEmpty()), Not(Equal("0")))))
		}, backupClusterReadyTimeout).Should(Succeed())

		logs, err := backupInitContainerLogs(pod, "restore-fetch")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("has 3 shard(s), this cluster has 2"))

		cr, err := utils.GetValkeyClusterStatus(mismatchCluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(cr.Status.ReadyShards).To(BeZero(), "a shard came up although its snapshot was refused")

		deleteBackupCluster(mismatchCluster)
	})
})

// Covers spec.backup.retention and clearing spec.backup. It has a cluster and
// an S3 server of its own, so it runs whether or not the restore specs pass.
var _ = Describe("ValkeyCluster backup retention and removal", Ordered, Label("ValkeyCluster", "Backup"), func() {
	const (
		s3Namespace      = "s3-e2e-retention"
		credentials      = "s3-e2e-retention-credentials"
		prefix           = "e2e-retention"
		retentionCluster = "backup-retention"
		retention        = 2
	)

	BeforeAll(func() {
		installS3Server(s3Namespace)
		createS3CredentialsSecret(credentials)

		By("creating a cluster that keeps two snapshots")
		deleteBackupCluster(retentionCluster)
		applyBackupManifest(backupClusterManifest(retentionCluster, 1, 1,
			backupSpecYAML(s3Endpoint(s3Namespace), prefix, credentials, retention)))
		waitBackupClusterReady(retentionCluster, 1)
		writeBackupKeys(backupShardPod(retentionCluster, 0, 0), 10)
		waitBackupReplicasCaughtUp(retentionCluster, 1, 1)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.CollectDebugInfo(namespace)
			dumpS3Server(s3Namespace)
		}
	})

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "valkeycluster", retentionCluster, "--ignore-not-found=true", "--wait=false")
		_, _ = utils.Run(cmd)
	})

	It("prunes the oldest snapshots beyond retention", func() {
		var snapshots []string
		var lastUploadLog string
		for i := range retention + 1 {
			var snapshot string
			snapshot, lastUploadLog = runBackupJob(retentionCluster, fmt.Sprintf("backup-retention-%d", i))
			// Snapshot names have one-second resolution; two runs in the same
			// second would overwrite each other rather than be pruned.
			Expect(snapshots).NotTo(ContainElement(snapshot), "two backup runs produced the same snapshot name")
			snapshots = append(snapshots, snapshot)
		}

		Expect(lastUploadLog).To(ContainSubstring("pruning snapshot " + snapshots[0]))
		Expect(listSnapshots(Default, s3Namespace, prefix)).To(Equal(snapshots[1:]))
	})

	It("removes the CronJob and its scripts when spec.backup is cleared", func() {
		cmd := exec.Command("kubectl", "patch", "valkeycluster", retentionCluster, "--type=json",
			"-p", `[{"op":"remove","path":"/spec/backup"}]`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		name := backupResourceName(retentionCluster)
		Eventually(func(g Gomega) {
			for _, kind := range []string{"cronjob", "configmap"} {
				cmd := exec.Command("kubectl", "get", kind, name, "--ignore-not-found=true", "-o", "name")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty(), fmt.Sprintf("%s %s still exists", kind, name))
			}
		}).Should(Succeed())

		waitBackupClusterReady(retentionCluster, 1)
	})
})

// snapshotManifest is the manifest.json the backup uploads next to the RDBs.
// Slots holds a shard's slot ranges separated by spaces, e.g. "0-5460".
type snapshotManifest struct {
	Version int    `json:"version"`
	Cluster string `json:"cluster"`
	Shards  []struct {
		Index      int    `json:"index"`
		File       string `json:"file"`
		Slots      string `json:"slots"`
		Primary    string `json:"primary"`
		Source     string `json:"source"`
		SourceRole string `json:"sourceRole"`
	} `json:"shards"`
}

// backupResourceName is the name of the backup CronJob and its ConfigMap.
func backupResourceName(clusterName string) string {
	return "valkey-" + clusterName + "-backup"
}

func backupClusterManifest(name string, shards, replicas int, extraSpec string) string {
	return fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
spec:
  shards: %d
  replicas: %d
%s`, name, shards, replicas, extraSpec)
}

// backupSpecYAML is a spec.backup that only runs when a spec starts a Job
// from it.
func backupSpecYAML(endpoint, prefix, credentials string, retention int) string {
	return fmt.Sprintf(`  backup:
    schedule: %q
    suspend: true
    retention: %d
    storage:
      s3:
        bucket: %s
        endpoint: %s
        prefix: %s
        credentialsSecret: %s
`, backupSchedule, retention, s3Bucket, endpoint, prefix, credentials)
}

func restoreSpecYAML(endpoint, credentials, snapshotPath string) string {
	return fmt.Sprintf(`  restoreFrom:
    storage:
      s3:
        bucket: %s
        endpoint: %s
        credentialsSecret: %s
    path: %s
`, s3Bucket, endpoint, credentials, snapshotPath)
}

func applyBackupManifest(manifest string) {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply:\n"+manifest)
}

// deleteBackupCluster removes a ValkeyCluster and waits for it to be gone, so a
// leftover from an earlier run cannot answer for the cluster a spec creates.
func deleteBackupCluster(name string) {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "delete", "valkeycluster", name,
		"--ignore-not-found=true", "--wait=true", "--timeout=3m")
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to delete ValkeyCluster "+name)
}

func waitBackupClusterReady(name string, shards int) {
	GinkgoHelper()
	By("waiting for " + name + " to reach Ready")
	Eventually(func(g Gomega) {
		cr, err := utils.GetValkeyClusterStatus(name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
		g.Expect(cr.Status.ReadyShards).To(Equal(int32(shards)))
	}, backupClusterReadyTimeout).Should(Succeed())
}

// backupShardPod is the pod of one node of a shard.
func backupShardPod(clusterName string, shard, node int) string {
	return fmt.Sprintf("valkey-%s-%d-%d-0", clusterName, shard, node)
}

// backupValkeyShell runs a script in a pod's server container as the default
// user, which has no password on these clusters. The operator's own
// password in VALKEYCLI_AUTH would otherwise be sent as the default user's.
func backupValkeyShell(pod, script string) (string, error) {
	cmd := exec.Command("kubectl", "exec", pod, "-c", "server", "--",
		"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; "+script)
	return utils.Run(cmd)
}

// backupInfoFields parses the "field:value" lines of an INFO reply.
func backupInfoFields(info string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(info, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			fields[k] = v
		}
	}
	return fields
}

// backupReplicationState returns a node's INFO replication fields, plus its key
// count as "dbsize".
func backupReplicationState(g Gomega, pod string) map[string]string {
	out, err := backupValkeyShell(pod, "valkey-cli info replication; echo dbsize:$(valkey-cli dbsize)")
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read replication state of "+pod)
	return backupInfoFields(out)
}

func backupDBSize(g Gomega, pod string) int {
	n, err := strconv.Atoi(backupReplicationState(g, pod)["dbsize"])
	g.Expect(err).NotTo(HaveOccurred())
	return n
}

// backupShardPrimary returns the pod that is the shard's primary right now.
// Roles are read live, since a failover moves them away from the node
// indexes.
func backupShardPrimary(g Gomega, clusterName string, shard, replicas int) string {
	var primaries []string
	for node := 0; node <= replicas; node++ {
		pod := backupShardPod(clusterName, shard, node)
		if backupReplicationState(g, pod)["role"] == "master" {
			primaries = append(primaries, pod)
		}
	}
	g.Expect(primaries).To(HaveLen(1), fmt.Sprintf("shard %d of %s should have one primary", shard, clusterName))
	return primaries[0]
}

func backupPrimaryDBSizeSum(g Gomega, clusterName string, shards, replicas int) int {
	sum := 0
	for shard := range shards {
		sum += backupDBSize(g, backupShardPrimary(g, clusterName, shard, replicas))
	}
	return sum
}

// waitBackupReplicasCaughtUp waits until every replica's link is up and it
// holds what its primary holds: the same replication offset and key count.
func waitBackupReplicasCaughtUp(clusterName string, shards, replicas int) {
	GinkgoHelper()
	By("waiting for the replicas of " + clusterName + " to catch up")
	Eventually(func(g Gomega) {
		for shard := range shards {
			states := map[string]map[string]string{}
			primary := ""
			for node := 0; node <= replicas; node++ {
				pod := backupShardPod(clusterName, shard, node)
				states[pod] = backupReplicationState(g, pod)
				if states[pod]["role"] == "master" {
					g.Expect(primary).To(BeEmpty(), fmt.Sprintf("shard %d has two primaries", shard))
					primary = pod
				}
			}
			g.Expect(primary).NotTo(BeEmpty(), fmt.Sprintf("shard %d has no primary", shard))
			for pod, state := range states {
				if pod == primary {
					continue
				}
				g.Expect(state["role"]).To(Equal("slave"), pod)
				g.Expect(state["master_link_status"]).To(Equal("up"), pod)
				g.Expect(state["slave_repl_offset"]).To(Equal(states[primary]["master_repl_offset"]), pod)
				g.Expect(state["dbsize"]).To(Equal(states[primary]["dbsize"]), pod)
			}
		}
	}, 3*time.Minute).Should(Succeed())
}

// writeBackupKeys writes count keys through the cluster from the given pod,
// all piped into a single valkey-cli.
func writeBackupKeys(pod string, count int) {
	GinkgoHelper()
	By(fmt.Sprintf("writing %d keys", count))
	out, err := backupValkeyShell(pod, fmt.Sprintf(
		"ok=$(for i in $(seq 1 %d); do echo \"set %s$i v$i\"; done | valkey-cli -c 2>/dev/null | grep -c '^OK$'); "+
			"echo written=$ok",
		count, backupKeyPrefix))
	Expect(err).NotTo(HaveOccurred(), out)
	Expect(out).To(ContainSubstring(fmt.Sprintf("written=%d", count)), "Not all keys were written")
}

// verifyBackupKeys reads back every key writeBackupKeys wrote. An
// "echo KEY:<i>" marker before each GET ties each value to its key, whatever
// else valkey-cli prints around a redirect.
func verifyBackupKeys(pod string, count int) {
	GinkgoHelper()
	out, err := backupValkeyShell(pod, fmt.Sprintf(
		"for i in $(seq 1 %d); do echo \"echo KEY:$i\"; echo \"get %s$i\"; done | valkey-cli -c 2>/dev/null",
		count, backupKeyPrefix))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the keys back")
	values := map[string]string{}
	current := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "KEY:"); ok {
			current = after
			continue
		}
		if current != "" && line != "" {
			values[current] = line
		}
	}
	for i := 1; i <= count; i++ {
		idx := strconv.Itoa(i)
		Expect(values[idx]).To(Equal("v"+idx), fmt.Sprintf("key %s%s", backupKeyPrefix, idx))
	}
}

// backupPrimaryIDs returns the node IDs of the primaries that serve slots,
// as the given pod sees them.
func backupPrimaryIDs(pod string) []string {
	GinkgoHelper()
	out, err := backupValkeyShell(pod, "valkey-cli cluster nodes")
	Expect(err).NotTo(HaveOccurred())
	var ids []string
	for _, line := range utils.GetNonEmptyLines(out) {
		f := strings.Fields(line)
		if len(f) >= 9 && slices.Contains(strings.Split(f[2], ","), "master") && !backupNodeFailing(f[2]) {
			ids = append(ids, f[0])
		}
	}
	return ids
}

// backupNodeFailing reports whether CLUSTER NODES flags mark a node as
// failing. The flags are a comma-separated list, and "nofailover" is not
// "fail".
func backupNodeFailing(flags string) bool {
	for _, flag := range strings.Split(flags, ",") {
		if flag == "fail" || flag == "fail?" {
			return true
		}
	}
	return false
}

func parseBackupSlotRange(r string) (start, end int) {
	GinkgoHelper()
	lo, hi, isRange := strings.Cut(r, "-")
	start, err := strconv.Atoi(lo)
	Expect(err).NotTo(HaveOccurred(), "slot range "+r)
	if !isRange {
		return start, start
	}
	end, err = strconv.Atoi(hi)
	Expect(err).NotTo(HaveOccurred(), "slot range "+r)
	return start, end
}

// countBackupClusterEvents counts a cluster's events with the given reason.
// Events are matched by the cluster's UID, so those of an earlier cluster of
// the same name do not count.
func countBackupClusterEvents(clusterName, reason string) (int, error) {
	cmd := exec.Command("kubectl", "get", "valkeycluster", clusterName, "-o", "jsonpath={.metadata.uid}")
	uid, err := utils.Run(cmd)
	if err != nil {
		return 0, err
	}
	cmd = exec.Command("kubectl", "get", "events",
		"--field-selector", fmt.Sprintf("reason=%s,involvedObject.uid=%s", reason, strings.TrimSpace(uid)),
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	out, err := utils.Run(cmd)
	if err != nil {
		return 0, err
	}
	return len(utils.GetNonEmptyLines(out)), nil
}

var uploadedSnapshot = regexp.MustCompile(`uploaded snapshot (\S+) to`)

// runBackupJob starts a Job from the cluster's backup CronJob, waits for it
// to succeed and returns the snapshot it uploaded and the upload log. The
// logs are read here because the Job belongs to the CronJob and goes with
// it.
func runBackupJob(clusterName, jobName string) (snapshot, uploadLog string) {
	GinkgoHelper()
	By("running backup Job " + jobName)
	cmd := exec.Command("kubectl", "delete", "job", jobName, "--ignore-not-found=true", "--wait=true")
	_, _ = utils.Run(cmd)
	cmd = exec.Command("kubectl", "create", "job", jobName, "--from=cronjob/"+backupResourceName(clusterName))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to start the backup Job")

	logs := func(container string) string {
		cmd := exec.Command("kubectl", "logs", "-l", "job-name="+jobName, "-c", container, "--tail=-1")
		out, _ := utils.Run(cmd)
		_, _ = fmt.Fprintf(GinkgoWriter, "backup Job %s, %s:\n%s\n", jobName, container, out)
		return out
	}
	// Stops at the first failed pod rather than waiting for the retry.
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "job", jobName, "-o", "jsonpath={.status.succeeded}/{.status.failed}")
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		succeeded, failed, _ := strings.Cut(strings.TrimSpace(out), "/")
		if failed != "" && failed != "0" {
			logs("dump")
			logs("upload")
			StopTrying("backup Job " + jobName + " failed").Now()
		}
		g.Expect(succeeded).To(Equal("1"))
	}, backupJobTimeout, 2*time.Second).Should(Succeed())

	logs("dump")
	uploadLog = logs("upload")
	m := uploadedSnapshot.FindStringSubmatch(uploadLog)
	Expect(m).NotTo(BeNil(), "the upload log names no snapshot")
	return m[1], uploadLog
}

// backupInitContainerLogs reads an init container's log, falling back to its
// previous run while it waits to be restarted.
func backupInitContainerLogs(pod, container string) (string, error) {
	cmd := exec.Command("kubectl", "logs", pod, "-c", container)
	out, err := utils.Run(cmd)
	if err == nil && strings.TrimSpace(out) != "" {
		return out, nil
	}
	cmd = exec.Command("kubectl", "logs", pod, "-c", container, "--previous")
	return utils.Run(cmd)
}

// dumpRestoreLogs writes the restore init container logs of a cluster's
// first nodes to the GinkgoWriter, for a failed spec.
func dumpRestoreLogs(clusterName string, shards int) {
	for shard := range shards {
		pod := backupShardPod(clusterName, shard, 0)
		for _, container := range []string{"restore-fetch", "restore-install"} {
			out, err := backupInitContainerLogs(pod, container)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "%s, %s:\n%s\n", pod, container, out)
		}
	}
}
