//go:build chaos
// +build chaos

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

package chaos

// Chaos test suite. See docs/chaos-testing.md for configuration and usage.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
	"github.com/valkey-io/valkey-operator/test/utils"
)

const namespace = "valkey-operator-system"

// ChaosContext holds the configuration for a chaos test iteration.
type ChaosContext struct {
	ClusterName  string
	Namespace    string
	WorkloadType string
	TargetShards []int
	Shards       int
	MinShards    int
	MaxShards    int
	Replicas     int
	MaxReplicas  int
	// LowestReplicas is the lowest spec.replicas this iteration; replicas added since have not synced.
	LowestReplicas   int
	TolerationSec    int
	CompoundMaxDelay time.Duration
	Rand             *rand.Rand
	// StoppedNodes holds nodes stop-worker-node stopped, for start-worker-node to restart.
	StoppedNodes []string
	// PartitionedNodes holds nodes partition-worker-node isolated, for heal-worker-node to heal.
	PartitionedNodes []string
}

// Scenario defines a named chaos scenario.
type Scenario struct {
	Name                 string
	LosesData            bool     // may lose data, skip verification and re-seed
	LosesDataIfNoReplica bool     // loses data only when replicas == 0
	DestroysReplicas     bool     // removes replicas' data, so a primary lost alongside has no copy left
	IgnoresTargets       bool     // does not act on ctx.TargetShards, so they need not exist when it runs
	DisabledByDefault    bool     // excluded unless explicitly listed in CHAOS_SCENARIOS
	Steps                []string // the scenarios a compound runs, in order
	Inject               func(ctx *ChaosContext) error
}

// losesData reports whether the scenario is expected to lose data, given the
// lowest replica count during the iteration. Scenarios that kill a whole shard
// only lose data when there is no replica to fail over to. A compound loses
// data when one of its steps would, when one step kills a primary while
// another destroys its replicas, or when two steps kill primaries with fewer
// than 2 replicas (the second can hit a promoted primary with an empty replica).
// With AOF a killed node restarts with its keys, so only LosesData applies.
func (s Scenario) losesData(lowestReplicas int, aof bool) bool {
	if len(s.Steps) == 0 {
		return s.LosesData || (!aof && s.LosesDataIfNoReplica && lowestReplicas == 0)
	}
	var primaryKills int
	var destroysReplicas bool
	for _, name := range s.Steps {
		step := scenarioByName(name)
		if step.LosesData {
			return true
		}
		if step.LosesDataIfNoReplica {
			primaryKills++
		}
		destroysReplicas = destroysReplicas || step.DestroysReplicas
	}
	if aof || primaryKills == 0 {
		return false
	}
	return lowestReplicas == 0 || destroysReplicas || (primaryKills >= 2 && lowestReplicas < 2)
}

var allScenarios = []Scenario{
	{Name: "delete-primary-pod", LosesDataIfNoReplica: true, Inject: deletePrimaryPod},
	{Name: "delete-replica-pod", DestroysReplicas: true, Inject: deleteReplicaPod},
	{Name: "delete-primary-pod-graceful", LosesDataIfNoReplica: true, Inject: deletePrimaryPodGraceful},
	{Name: "delete-replica-pod-graceful", DestroysReplicas: true, Inject: deleteReplicaPodGraceful},
	{Name: "evict-primary-pod", LosesDataIfNoReplica: true, Inject: evictPrimaryPod},
	{Name: "evict-replica-pod", DestroysReplicas: true, Inject: evictReplicaPod},
	{Name: "delete-shard-pods", LosesData: true, DestroysReplicas: true, Inject: deleteShardPods},
	{Name: "delete-primary-workload", LosesDataIfNoReplica: true, Inject: deletePrimaryWorkload},
	{Name: "delete-replica-workload", DestroysReplicas: true, Inject: deleteReplicaWorkload},
	{Name: "pause-primary-container", Inject: pausePrimaryContainer},
	{Name: "pause-replica-container", Inject: pauseReplicaContainer},
	{Name: "scale-shards", IgnoresTargets: true, Inject: scaleShards},
	{Name: "scale-replicas", IgnoresTargets: true, Inject: scaleReplicas},
	{Name: "rolling-update", LosesDataIfNoReplica: true, IgnoresTargets: true, Inject: rollingUpdate},
	{Name: "delete-recreate-cluster", LosesData: true, IgnoresTargets: true, Inject: deleteRecreateCluster},
	{Name: "delete-controller-pod", IgnoresTargets: true, Inject: deleteControllerPod},
	{Name: "pause-worker-node", DisabledByDefault: true, LosesData: true, Inject: pauseWorkerNode},
	{Name: "network-partition-primary", DisabledByDefault: true, LosesDataIfNoReplica: true, Inject: networkPartitionPrimary},
	{Name: "network-partition-replica", DisabledByDefault: true, Inject: networkPartitionReplica},
	{Name: "crash-primary", DisabledByDefault: true, LosesDataIfNoReplica: true, Inject: crashPrimary},
	{Name: "crash-replica", DisabledByDefault: true, DestroysReplicas: true, Inject: crashReplica},
	{Name: "stop-worker-node", DisabledByDefault: true, LosesDataIfNoReplica: true, Inject: stopWorkerNode},
	{Name: "start-worker-node", DisabledByDefault: true, IgnoresTargets: true, Inject: startWorkerNode},
	{Name: "partition-worker-node", DisabledByDefault: true, LosesDataIfNoReplica: true, Inject: partitionNode},
	{Name: "heal-worker-node", DisabledByDefault: true, IgnoresTargets: true, Inject: healNode},
}

// defaultCompounds are the compound scenarios run, alongside the single ones,
// when CHAOS_SCENARIOS is unset.
var defaultCompounds = []struct {
	name     string
	maxDelay time.Duration
}{
	{name: "scale-shards+scale-shards", maxDelay: 2 * time.Minute},
	{name: "scale-shards+delete-controller-pod", maxDelay: 2 * time.Minute},
	{name: "scale-replicas+delete-primary-pod"},
	{name: "scale-replicas+evict-primary-pod"},
	{name: "scale-shards+delete-primary-pod"},
}

// defaultCompoundDelay is the longest random delay between compound steps.
const defaultCompoundDelay = 10 * time.Second

var _ = Describe("ValkeyCluster Chaos", Label("chaos"), Ordered, func() {
	var (
		clusterName     = "chaos-test-cluster"
		workloadType    string
		persistence     bool
		aof             bool
		shards          int
		minShards       int
		maxShards       int
		replicas        int
		maxReplicas     int
		numKeys         int
		dataSize        int
		seededKeys      int
		recoveryTimeout time.Duration
		tolerationSec   int
		targetShards    string
		mode            string
		seed            int64
		rnd             *rand.Rand
		scenarios       []Scenario
		cpuPressure     bool
		cpuMin          float64
		cpuMax          float64
		throttledNodes  []string
		workerNodes     []string
		writeRPS        int
		nodeTimeoutMs   int
		nodeTimeoutMin  int
		nodeTimeoutMax  int
		compoundDelay   time.Duration
	)

	BeforeAll(func() {
		// Parse configuration from environment
		workloadType = envOneOf("CHAOS_WORKLOAD_TYPE", "StatefulSet", []string{"StatefulSet", "Deployment"})
		persistence = envBool("CHAOS_PERSISTENCE", false)
		aof = persistence && envBool("CHAOS_AOF", true)
		shards = envIntOrDefault("CHAOS_SHARDS", 3, 1 /* min */)
		minShards = envIntOrDefault("CHAOS_MIN_SHARDS", shards, 1 /* min */)
		maxShards = envIntOrDefault("CHAOS_MAX_SHARDS", shards+3, minShards /* min */)
		replicas = envIntOrDefault("CHAOS_REPLICAS", 1, 0 /* min */)
		maxReplicas = envIntOrDefault("CHAOS_MAX_REPLICAS", replicas+2, replicas /* min */)
		numKeys = envIntOrDefault("CHAOS_NUM_KEYS", 100000, 1 /* min */)
		dataSize = envIntOrDefault("CHAOS_DATA_SIZE", 3, 1 /* min */)
		targetShards = envOrDefault("CHAOS_TARGET_SHARDS", "random")
		mode = envOneOf("CHAOS_MODE", "random", []string{"random", "sequential"})
		recoveryTimeout = envDurationOrDefault("CHAOS_RECOVERY_TIMEOUT", calcTimeout(shards, replicas))
		tolerationSec = envIntOrDefault("CHAOS_TOLERATION_SECONDS", 0, 0 /* min */)
		seed = envInt64OrDefault("CHAOS_SEED", time.Now().UnixNano())
		scenarios = filterScenarios(allScenarios, envOrDefault("CHAOS_SCENARIOS", ""))
		cpuPressure = envBool("CHAOS_CPU_PRESSURE", false)
		cpuMin = envFloat64OrDefault("CHAOS_CPU_MIN", 0.3, 0.1)
		cpuMax = envFloat64OrDefault("CHAOS_CPU_MAX", 1.0, cpuMin)
		writeRPS = envIntOrDefault("CHAOS_WRITE_RPS", 20, 0 /* min */)
		nodeTimeoutMin = envIntOrDefault("CHAOS_NODE_TIMEOUT_MIN_MS", 5000, 1 /* min */)
		nodeTimeoutMax = envIntOrDefault("CHAOS_NODE_TIMEOUT_MAX_MS", 15000, nodeTimeoutMin /* min */)
		compoundDelay = envDurationOrDefault("CHAOS_COMPOUND_MAX_DELAY", 0)

		// The scale scenarios move within these ranges, so the starting counts
		// have to be inside them.
		if shards < minShards || shards > maxShards {
			Fail(fmt.Sprintf("CHAOS_SHARDS=%d must be within [%d, %d]", shards, minShards, maxShards))
		}
		if replicas > maxReplicas {
			Fail(fmt.Sprintf("CHAOS_REPLICAS=%d must be <= CHAOS_MAX_REPLICAS=%d", replicas, maxReplicas))
		}
		if cpuPressure {
			workerNodes = getWorkerNodes()
		}

		rnd = rand.New(rand.NewSource(seed))

		// A randomized cluster-node-timeout varies how quickly nodes declare
		// each other failed, exercising failover and recovery timing across
		// runs. Drawn from rnd so a given CHAOS_SEED reproduces the value.
		nodeTimeoutMs = nodeTimeoutMin
		if nodeTimeoutMax > nodeTimeoutMin {
			nodeTimeoutMs = nodeTimeoutMin + rnd.Intn(nodeTimeoutMax-nodeTimeoutMin+1)
		}

		// Log configuration
		_, _ = fmt.Fprintf(GinkgoWriter, "=== Chaos Test Configuration ===\n")
		_, _ = fmt.Fprintf(GinkgoWriter, "  WorkloadType:     %s\n", workloadType)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Shards:           %d (min=%d, max=%d)\n", shards, minShards, maxShards)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Replicas:         %d (max=%d)\n", replicas, maxReplicas)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Persistence:      %v\n", persistence)
		_, _ = fmt.Fprintf(GinkgoWriter, "  AOF:              %v\n", aof)
		_, _ = fmt.Fprintf(GinkgoWriter, "  NumKeys:          %d\n", numKeys)
		_, _ = fmt.Fprintf(GinkgoWriter, "  DataSize:         %d\n", dataSize)
		_, _ = fmt.Fprintf(GinkgoWriter, "  TargetShards:     %s\n", targetShards)
		_, _ = fmt.Fprintf(GinkgoWriter, "  RecoveryTimeout:  %s\n", recoveryTimeout)
		_, _ = fmt.Fprintf(GinkgoWriter, "  NodeTimeout:      %dms (range=[%d, %d])\n", nodeTimeoutMs, nodeTimeoutMin, nodeTimeoutMax)
		if compoundDelay > 0 {
			_, _ = fmt.Fprintf(GinkgoWriter, "  CompoundMaxDelay: %s (all compounds)\n", compoundDelay)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "  CompoundMaxDelay: per compound (default %s)\n", defaultCompoundDelay)
		}
		if tolerationSec > 0 {
			_, _ = fmt.Fprintf(GinkgoWriter, "  Tolerations:      not-ready=%ds, unreachable=%ds (default: 300s)\n", tolerationSec, tolerationSec)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "  Tolerations:      not set (default: 300s, no evictions will be triggered)\n")
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Seed:             %d\n", seed)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Mode:             %s\n", mode)
		_, _ = fmt.Fprintf(GinkgoWriter, "  CpuPressure:      %v (min=%.2f, max=%.2f)\n", cpuPressure, cpuMin, cpuMax)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Scenarios:\n")
		enabledNames := make(map[string]bool)
		for _, s := range scenarios {
			enabledNames[s.Name] = true
		}
		allNames := make(map[string]bool)
		for _, s := range allScenarios {
			allNames[s.Name] = true
			status := "enabled"
			if !enabledNames[s.Name] {
				status = "disabled"
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "    - %-30s [%s]\n", s.Name, status)
		}
		for _, s := range scenarios {
			if !allNames[s.Name] {
				_, _ = fmt.Fprintf(GinkgoWriter, "    - %-30s [enabled]\n", s.Name)
			}
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "================================\n")

		// Create a cluster
		By("creating ValkeyCluster for chaos testing")
		manifest := fmt.Sprintf(`apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: %s
  namespace: default
spec:
  shards: %d
  replicas: %d
  workloadType: %s
  config:
    enable-debug-command: local
    cluster-node-timeout: "%d"
`, clusterName, shards, replicas, workloadType, nodeTimeoutMs)
		if aof {
			manifest += "    appendonly: \"yes\"\n"
		}

		if tolerationSec > 0 {
			manifest += fmt.Sprintf(`  scheduling:
    tolerations:
    - key: node.kubernetes.io/not-ready
      operator: Exists
      effect: NoExecute
      tolerationSeconds: %d
    - key: node.kubernetes.io/unreachable
      operator: Exists
      effect: NoExecute
      tolerationSeconds: %d
`, tolerationSec, tolerationSec)
		}

		if persistence {
			if workloadType != "StatefulSet" {
				Fail("CHAOS_PERSISTENCE=true requires CHAOS_WORKLOAD_TYPE=StatefulSet")
			}
			manifest += `  persistence:
    size: 1Gi
    reclaimPolicy: Delete
`
		}

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create chaos ValkeyCluster")

		By("waiting for cluster to become Ready")
		Eventually(func(g Gomega) {
			cr, err := utils.GetValkeyClusterStatus(clusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady))
			g.Expect(cr.Status.ReadyShards).To(Equal(int32(shards)))
			err = verifyClusterHealth(clusterName, "default", shards, replicas)
			g.Expect(err).NotTo(HaveOccurred())
		}, recoveryTimeout, 5*time.Second).Should(Succeed())
		expectNoControllerRestarts("while the cluster was created")

		By("seeding test data")
		seededKeys, err = startBackgroundClient(clusterName, "default", numKeys, dataSize, writeRPS)
		Expect(err).NotTo(HaveOccurred(), "Failed to seed test data")
		_, _ = fmt.Fprintf(GinkgoWriter, "  Seeded keys:      %d\n", seededKeys)
	})

	AfterEach(func() {
		// Always remove CPU pressure to avoid leaving nodes throttled
		if cpuPressure {
			unthrottleWorkerNodes(workerNodes)
		}

		if CurrentSpecReport().Failed() {
			// Dump keyspace counts
			if total, perShard, err := getTotalKeyCount(clusterName, "default"); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "KEY COUNT at failure: total=%d, per-shard=%v\n", total, perShard)
			}

			// Dump cluster-specific state
			By("collecting chaos cluster debug info")
			cmd := exec.Command("kubectl", "get", "pods", "-n", "default", "-l",
				fmt.Sprintf("valkey.io/cluster=%s", clusterName), "-o", "wide")
			if output, err := utils.Run(cmd); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Cluster pods:\n%s\n", output)
			}
			cmd = exec.Command("kubectl", "get", "valkeycluster", clusterName, "-n", "default", "-o", "yaml")
			if output, err := utils.Run(cmd); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "ValkeyCluster status:\n%s\n", output)
			}
			cmd = exec.Command("kubectl", "get", "valkeynodes", "-n", "default", "-l",
				fmt.Sprintf("valkey.io/cluster=%s", clusterName), "-o", "wide")
			if output, err := utils.Run(cmd); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "ValkeyNodes:\n%s\n", output)
			}

			// Collect logs and CLUSTER NODES from all valkey node pods
			cmd = exec.Command("kubectl", "get", "pods", "-n", "default", "-l",
				fmt.Sprintf("valkey.io/cluster=%s", clusterName),
				"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			if podList, err := utils.Run(cmd); err == nil {
				for _, pod := range utils.GetNonEmptyLines(podList) {
					cmd = exec.Command("kubectl", "exec", pod, "-n", "default", "-c", "server", "--",
						"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli CLUSTER NODES")
					if output, err := utils.Run(cmd); err == nil {
						_, _ = fmt.Fprintf(GinkgoWriter, "CLUSTER NODES from %s:\n%s\n", pod, output)
					}
					cmd = exec.Command("kubectl", "logs", pod, "-n", "default", "-c", "server", "--tail=100")
					if logs, err := utils.Run(cmd); err == nil {
						_, _ = fmt.Fprintf(GinkgoWriter, "Logs for %s:\n%s\n", pod, logs)
					}
				}
			}

			// Controller logs and K8s events
			utils.CollectDebugInfo(namespace)
			logPreviousControllerLogs()
		}
	})

	AfterAll(func() {
		stopBackgroundClient("default")
		By("cleaning up chaos cluster")
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "-n", "default", "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		// With persistence, ValkeyNodes carry a finalizer that only the operator
		// removes. Wait for them while it runs, or undeploy hangs on the CRD.
		selector := "valkey.io/cluster=" + clusterName
		cmd = exec.Command("kubectl", "wait", "--for=delete", "valkeynodes", "-l", selector,
			"-n", "default", "--timeout=2m")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "get", "valkeynodes", "-l", selector, "-n", "default", "-o", "name")
		out, _ := utils.Run(cmd)
		stuck := utils.GetNonEmptyLines(out)
		for _, node := range stuck {
			_, _ = fmt.Fprintf(GinkgoWriter, "  WARNING: %s still exists, removing its finalizers\n", node)
			cmd = exec.Command("kubectl", "patch", node, "-n", "default", "--type=merge",
				"-p", `{"metadata":{"finalizers":null}}`)
			_, _ = utils.Run(cmd)
		}
		// The operator was running during the wait, so a leftover finalizer is its bug.
		if len(stuck) > 0 {
			Fail(fmt.Sprintf("ValkeyNodes kept their finalizers after 2m: %v", stuck))
		}
	})

	It("runs fault injection until failure", func() {
		passedCount := make(map[string]int)
		skippedCount := make(map[string]int)
		// printStats prints per-scenario counts every 10th iteration.
		printStats := func(iteration int) {
			if len(scenarios) < 2 || iteration%10 != 0 {
				return
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Scenario Statistics (after %d iterations) ===\n", iteration)
			for _, s := range scenarios {
				line := fmt.Sprintf("  %-30s %d passed", s.Name, passedCount[s.Name])
				if n := skippedCount[s.Name]; n > 0 {
					line += fmt.Sprintf(", %d skipped", n)
				}
				_, _ = fmt.Fprintln(GinkgoWriter, line)
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "=================================================\n")
		}

		// expectWriterProgress fails when the writer is gone or its writes do not
		// increase during the check (a --restart=Never writer is not replaced).
		// Call it only while the seeded keys exist: SET XX cannot write lost keys.
		expectWriterProgress := func(iteration int) {
			if writeRPS == 0 {
				return
			}
			By(fmt.Sprintf("Iteration %d: verifying background writer still writing", iteration))
			// Baseline is 0 if a fresh writer has not logged yet.
			_, baseline, _, _ := backgroundClientWrites("default")
			Eventually(func(g Gomega) {
				phase, writes, found, err := backgroundClientWrites("default")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Running"), "writer pod is %s", phase)
				g.Expect(found).To(BeTrue(), "writer logged no stats line yet")
				g.Expect(writes).To(BeNumerically(">", baseline), "writer made no progress (writes=%d)", writes)
			}, 30*time.Second, 2*time.Second).Should(Succeed(), "background writer stopped in iteration %d", iteration)
		}

		for iteration := 1; ; iteration++ {
			var scenario Scenario
			if mode == "sequential" {
				scenario = scenarios[(iteration-1)%len(scenarios)]
			} else {
				scenario = scenarios[rnd.Intn(len(scenarios))]
			}

			var targetShardsForIteration []int
			switch {
			case targetShards == "random":
				count := rnd.Intn(shards) + 1
				targetShardsForIteration = rnd.Perm(shards)[:count]
			case targetShards == "all":
				targetShardsForIteration = make([]int, shards)
				for i := range shards {
					targetShardsForIteration[i] = i
				}
			case strings.HasPrefix(targetShards, "random"):
				count, err := strconv.Atoi(strings.TrimPrefix(targetShards, "random"))
				if err != nil || count < 1 {
					Fail(fmt.Sprintf("CHAOS_TARGET_SHARDS=%q: expected 'randomN' where N >= 1", targetShards))
				}
				count = min(count, shards)
				targetShardsForIteration = rnd.Perm(shards)[:count]
			default:
				for _, s := range strings.Split(targetShards, ",") {
					v, err := strconv.Atoi(strings.TrimSpace(s))
					if err != nil {
						Fail(fmt.Sprintf("CHAOS_TARGET_SHARDS=%q: %q is not a valid shard index", targetShards, s))
					}
					if v < 0 || v >= shards {
						Fail(fmt.Sprintf("CHAOS_TARGET_SHARDS=%q: index %d is outside [0, %d)", targetShards, v, shards))
					}
					if slices.Contains(targetShardsForIteration, v) {
						Fail(fmt.Sprintf("CHAOS_TARGET_SHARDS=%q: index %d is repeated", targetShards, v))
					}
					targetShardsForIteration = append(targetShardsForIteration, v)
				}
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- Iteration %d: scenario=%s ---\n",
				iteration, scenario.Name)

			logClusterState(clusterName, "default", "before")

			// Log per-shard key counts before scenario
			if _, perShard, err := getTotalKeyCount(clusterName, "default"); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "  KEY COUNT before: %v\n", perShard)
			}

			// Mark iteration start in all valkey node logs
			logMsg := fmt.Sprintf("CHAOS-TEST: iteration %d scenario=%s", iteration, scenario.Name)
			cmd := exec.Command("kubectl", "get", "pods", "-n", "default", "-l",
				fmt.Sprintf("valkey.io/cluster=%s", clusterName),
				"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			if podList, err := utils.Run(cmd); err == nil {
				for _, pod := range utils.GetNonEmptyLines(podList) {
					cmd = exec.Command("kubectl", "exec", pod, "-n", "default", "-c", "server", "--",
						"sh", "-c",
						`unset VALKEYCLI_AUTH REDISCLI_AUTH; exec valkey-cli EVAL "return server.log(server.LOG_WARNING, ARGV[1])" 0 "$1"`,
						"sh", logMsg)
					_, _ = utils.Run(cmd)
				}
			}

			ctx := &ChaosContext{
				ClusterName:    clusterName,
				Namespace:      "default",
				WorkloadType:   workloadType,
				TargetShards:   targetShardsForIteration,
				Shards:         shards,
				MinShards:      minShards,
				MaxShards:      maxShards,
				Replicas:       replicas,
				MaxReplicas:    maxReplicas,
				LowestReplicas: replicas,
				TolerationSec:  tolerationSec,
				Rand:           rnd,

				CompoundMaxDelay: compoundDelay,
			}

			// Apply random CPU pressure to Kind worker nodes
			if cpuPressure {
				unthrottleWorkerNodes(workerNodes)
				throttledNodes = throttleRandomWorkerNodes(rnd, workerNodes, cpuMin, cpuMax)
				if len(throttledNodes) > 0 {
					_, _ = fmt.Fprintf(GinkgoWriter, "  CPU pressure on %v\n", throttledNodes)
				}
			}

			err := scenario.Inject(ctx)
			if err != nil {
				if strings.Contains(err.Error(), "skip:") {
					_, _ = fmt.Fprintf(GinkgoWriter, "  Skipped: %s\n", err)
					unthrottleWorkerNodes(throttledNodes)
					skippedCount[scenario.Name]++
					printStats(iteration)
					continue
				}
				Fail(fmt.Sprintf("Iteration %d: scenario %s failed to inject: %v", iteration, scenario.Name, err))
			}

			// Update shards/replicas in case a scale scenario changed them
			shards = ctx.Shards
			replicas = ctx.Replicas

			// Recalculate timeout based on current cluster size (may have scaled)
			recoveryTimeout = envDurationOrDefault("CHAOS_RECOVERY_TIMEOUT", calcTimeout(shards, replicas))

			// Wait until CR is Ready, all pods are Running, and cluster health is ok.
			By(fmt.Sprintf("Iteration %d: waiting for cluster recovery", iteration))
			recoveryStart := time.Now()
			lastStatus := recoveryStart
			Eventually(func(g Gomega) {
				cr, err := utils.GetValkeyClusterStatus(clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				if time.Since(lastStatus) >= time.Minute {
					remaining := recoveryTimeout - time.Since(recoveryStart)
					_, _ = fmt.Fprintf(GinkgoWriter, "    recovery status: state=%s reason=%s readyShards=%d/%d (timeout in %s)\n",
						cr.Status.State, cr.Status.Reason, cr.Status.ReadyShards, shards, remaining.Truncate(time.Second))
					lastStatus = time.Now()
				}
				g.Expect(cr.Status.State).To(Equal(valkeyiov1alpha1.ClusterStateReady),
					fmt.Sprintf("cluster state: %s, reason: %s", cr.Status.State, cr.Status.Reason))
				g.Expect(cr.Status.ReadyShards).To(Equal(int32(shards)))
				err = verifyK8sResources(clusterName, "default", workloadType, shards, replicas)
				g.Expect(err).NotTo(HaveOccurred(), "K8s resources not ready: %v", err)
				err = verifyClusterHealth(clusterName, "default", shards, replicas)
				g.Expect(err).NotTo(HaveOccurred(), "cluster health: %v", err)
				err = verifyClusterConverged(clusterName, "default")
				g.Expect(err).NotTo(HaveOccurred(), "cluster convergence: %v", err)
			}, recoveryTimeout, 5*time.Second).Should(Succeed(),
				fmt.Sprintf("Iteration %d: cluster did not recover after %s (scenario=%s, shards=%v, seed=%d)",
					iteration, recoveryTimeout, scenario.Name, targetShardsForIteration, seed))

			// Remove CPU pressure after recovery
			unthrottleWorkerNodes(throttledNodes)

			// Log cluster state after recovery
			logClusterState(clusterName, "default", "after")
			expectNoControllerRestarts(fmt.Sprintf("in iteration %d", iteration))

			if !scenario.losesData(ctx.LowestReplicas, aof) {
				By(fmt.Sprintf("Iteration %d: verifying test data integrity", iteration))
				Eventually(func() error {
					return verifyTestData(clusterName, "default", seededKeys)
				}, 60*time.Second, 2*time.Second).Should(Succeed(),
					fmt.Sprintf("Iteration %d: data integrity check failed (seed=%d)", iteration, seed))
				expectWriterProgress(iteration)
			} else {
				By(fmt.Sprintf("Iteration %d: checking for data loss (scenario may lose data)", iteration))
				if err := verifyTestData(clusterName, "default", seededKeys); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "  WARNING: data lost (expected): %s\n", err)
					By(fmt.Sprintf("Iteration %d: re-seeding test data after data-loss scenario", iteration))
					Eventually(func() error {
						return flushAll(clusterName, "default")
					}, 60*time.Second, 2*time.Second).Should(Succeed(), "Failed to flush data")
					seededKeys, err = startBackgroundClient(clusterName, "default", numKeys, dataSize, writeRPS)
					Expect(err).NotTo(HaveOccurred(), "Failed to re-seed test data")
					_, _ = fmt.Fprintf(GinkgoWriter, "  Seeded keys:      %d\n", seededKeys)
				}
				expectWriterProgress(iteration)
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "  Iteration %d: PASSED\n", iteration)
			passedCount[scenario.Name]++
			printStats(iteration)
		}
	})
})

// Fault scenario implementations

// deletePrimaryPod force deletes the primary pod of every targeted shard.
func deletePrimaryPod(ctx *ChaosContext) error {
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting primary pod: %s\n", pod)
		pods = append(pods, pod)
	}
	for _, pod := range pods {
		if err := deletePodByName(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// deleteReplicaPod force deletes one replica pod of every targeted shard.
func deleteReplicaPod(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("deleting replica pod for shard %d: %w", shard, err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting replica pod: %s\n", pod)
		pods = append(pods, pod)
	}
	for _, pod := range pods {
		if err := deletePodByName(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// deletePrimaryPodGraceful deletes the primary pod of every targeted shard with
// its grace period, as kubectl delete pod does by default.
func deletePrimaryPodGraceful(ctx *ChaosContext) error {
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Gracefully deleting primary pod: %s\n", pod)
		pods = append(pods, pod)
	}
	return deletePodsGracefully(pods, ctx.Namespace)
}

// deleteReplicaPodGraceful deletes one replica pod of every targeted shard with
// its grace period, as kubectl delete pod does by default.
func deleteReplicaPodGraceful(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("deleting replica pod for shard %d: %w", shard, err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Gracefully deleting replica pod: %s\n", pod)
		pods = append(pods, pod)
	}
	return deletePodsGracefully(pods, ctx.Namespace)
}

// evictPrimaryPod evicts the primary pod of every targeted shard.
func evictPrimaryPod(ctx *ChaosContext) error {
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		pods = append(pods, pod)
	}
	return evictPods(pods, ctx.Namespace)
}

// evictReplicaPod evicts one replica pod of every targeted shard.
func evictReplicaPod(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("evicting replica pod for shard %d: %w", shard, err)
		}
		pods = append(pods, pod)
	}
	return evictPods(pods, ctx.Namespace)
}

// evictPods evicts the pods one at a time, as kubectl drain does, waiting for
// each to be gone. An eviction the PodDisruptionBudget refuses is retried, and
// the pod is left running if it is still refused after two minutes.
func evictPods(pods []string, namespace string) error {
	for _, pod := range pods {
		uid, err := getPodUID(pod, namespace)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Evicting pod: %s\n", pod)
		deadline := time.Now().Add(2 * time.Minute)
		for err = evictPod(pod, namespace); errors.Is(err, errEvictionBlocked); err = evictPod(pod, namespace) {
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Second)
		}
		if errors.Is(err, errEvictionBlocked) {
			_, _ = fmt.Fprintf(GinkgoWriter, "    eviction of %s still blocked by the PodDisruptionBudget, leaving it\n", pod)
			continue
		}
		if err != nil {
			return err
		}
		// The StatefulSet recreates the pod under the same name, so wait for the UID to change.
		Eventually(func() (string, error) {
			return getPodUID(pod, namespace)
		}, 2*time.Minute, time.Second).ShouldNot(Equal(uid), "evicted pod %s was not deleted", pod)
	}
	return nil
}

// crashPrimary abnormally crashes the server process of every targeted shard's
// primary. The container restarts in place with the same node ID, so recovery
// runs without a graceful handoff: the shard's replica must self-promote (or the
// operator take over), then the restarted node rejoins as a replica.
func crashPrimary(ctx *ChaosContext) error {
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Crashing primary server: %s\n", pod)
		pods = append(pods, pod)
	}
	crashedAt := time.Now()
	for _, pod := range pods {
		if err := crashServer(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	Eventually(func(g Gomega) {
		for _, pod := range pods {
			g.Expect(serverRestartedSince(pod, ctx.Namespace, crashedAt)).To(Succeed())
		}
	}).WithTimeout(serverRestartTimeout).WithPolling(2 * time.Second).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "  Server containers restarted and ready after %s\n", time.Since(crashedAt).Round(time.Second))
	return nil
}

// crashReplica abnormally crashes the server process of one replica in every
// targeted shard. The container restarts in place and resyncs from its primary.
func crashReplica(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("crashing replica server for shard %d: %w", shard, err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Crashing replica server: %s\n", pod)
		pods = append(pods, pod)
	}
	crashedAt := time.Now()
	for _, pod := range pods {
		if err := crashServer(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	Eventually(func(g Gomega) {
		for _, pod := range pods {
			g.Expect(serverRestartedSince(pod, ctx.Namespace, crashedAt)).To(Succeed())
		}
	}).WithTimeout(serverRestartTimeout).WithPolling(2 * time.Second).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "  Server containers restarted and ready after %s\n", time.Since(crashedAt).Round(time.Second))
	return nil
}

// deleteShardPods force deletes every pod of every targeted shard, primary included.
func deleteShardPods(ctx *ChaosContext) error {
	_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting all pods for shards %v\n", ctx.TargetShards)

	var pods []string
	for _, shard := range ctx.TargetShards {
		cmd := exec.Command("kubectl", "get", "pods", "-n", ctx.Namespace,
			"-l", fmt.Sprintf("valkey.io/cluster=%s,valkey.io/shard-index=%d", ctx.ClusterName, shard),
			"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		output, err := utils.Run(cmd)
		if err != nil {
			return err
		}
		pods = append(pods, utils.GetNonEmptyLines(output)...)
	}

	for _, pod := range pods {
		if err := deletePodByName(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// deletePrimaryWorkload deletes the StatefulSet or Deployment owning each targeted primary.
func deletePrimaryWorkload(ctx *ChaosContext) error {
	var workloads []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		workload, err := getWorkloadForPod(pod, ctx.Namespace, ctx.WorkloadType)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting primary %s: %s\n", ctx.WorkloadType, workload)
		workloads = append(workloads, workload)
	}
	for _, workload := range workloads {
		if err := deleteWorkload(workload, ctx.Namespace, ctx.WorkloadType); err != nil {
			return err
		}
	}
	return nil
}

// deleteReplicaWorkload deletes the StatefulSet or Deployment owning one replica per targeted shard.
func deleteReplicaWorkload(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	var workloads []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("deleting replica workload for shard %d: %w", shard, err)
		}
		workload, err := getWorkloadForPod(pod, ctx.Namespace, ctx.WorkloadType)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting replica %s: %s\n", ctx.WorkloadType, workload)
		workloads = append(workloads, workload)
	}
	for _, workload := range workloads {
		if err := deleteWorkload(workload, ctx.Namespace, ctx.WorkloadType); err != nil {
			return err
		}
	}
	return nil
}

// networkPartitionPrimary isolates the worker nodes hosting the targeted primaries, then heals them.
func networkPartitionPrimary(ctx *ChaosContext) error {
	// Without tolerations: 3-5s, below the 5-15s default node timeout, so no failover.
	// With tolerations: extends up to eviction threshold + 20s to also test pod rescheduling.
	maxDuration := 5 * time.Second
	if ctx.TolerationSec > 0 {
		evictionThreshold := 40*time.Second + time.Duration(ctx.TolerationSec)*time.Second
		maxDuration = evictionThreshold + 20*time.Second
	}
	duration := randomDuration(ctx.Rand, 3*time.Second, maxDuration)
	var nodes []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		nodeName, err := getPodNodeName(pod, ctx.Namespace)
		if err != nil {
			return err
		}
		if slices.Contains(nodes, nodeName) {
			continue
		}
		if isProtectedNode(nodeName, ctx.Namespace) {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"  Skipping node %s (shard %d): hosts the controller-manager or background writer\n", nodeName, shard)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Will partition node %s (primary pod: %s, shard %d)\n", nodeName, pod, shard)
		nodes = append(nodes, nodeName)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("skip: all targeted primaries are on the controller-manager or background writer node")
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "  Partitioning %d node(s) for %s\n", len(nodes), duration.Truncate(time.Millisecond))
	var partitioned []string
	for _, nodeName := range nodes {
		if err := partitionWorkerNode(nodeName); err != nil {
			// Include nodeName: partitionWorkerNode may have applied some rules.
			_ = healWorkerNodes(append(partitioned, nodeName))
			return err
		}
		partitioned = append(partitioned, nodeName)
	}
	time.Sleep(duration)
	return healWorkerNodes(partitioned)
}

// networkPartitionReplica isolates the worker nodes hosting the targeted replicas, then heals them.
func networkPartitionReplica(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	// Without tolerations: 3-5s, below the 5-15s default node timeout, so no failover.
	// With tolerations: extends up to eviction threshold + 20s to also test pod rescheduling.
	maxDuration := 5 * time.Second
	if ctx.TolerationSec > 0 {
		evictionThreshold := 40*time.Second + time.Duration(ctx.TolerationSec)*time.Second
		maxDuration = evictionThreshold + 20*time.Second
	}
	duration := randomDuration(ctx.Rand, 3*time.Second, maxDuration)
	var nodes []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("partitioning replica node for shard %d: %w", shard, err)
		}
		nodeName, err := getPodNodeName(pod, ctx.Namespace)
		if err != nil {
			return err
		}
		if slices.Contains(nodes, nodeName) {
			continue
		}
		if isProtectedNode(nodeName, ctx.Namespace) {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"  Skipping node %s (shard %d): hosts the controller-manager or background writer\n", nodeName, shard)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Will partition node %s (replica pod: %s, shard %d)\n", nodeName, pod, shard)
		nodes = append(nodes, nodeName)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("skip: all targeted replicas are on the controller-manager or background writer node")
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "  Partitioning %d node(s) for %s\n", len(nodes), duration.Truncate(time.Millisecond))
	var partitioned []string
	for _, nodeName := range nodes {
		if err := partitionWorkerNode(nodeName); err != nil {
			// Include nodeName: partitionWorkerNode may have applied some rules.
			_ = healWorkerNodes(append(partitioned, nodeName))
			return err
		}
		partitioned = append(partitioned, nodeName)
	}
	time.Sleep(duration)
	return healWorkerNodes(partitioned)
}

// healWorkerNodes heals every given worker node, continuing past failures so a
// single error cannot leave a node partitioned. Returns the first error.
func healWorkerNodes(workers []string) error {
	var firstErr error
	for _, worker := range workers {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Healing worker node %s\n", worker)
		if err := healWorkerNode(worker); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// pausePrimaryContainer freezes the server container of every targeted primary, then resumes it.
func pausePrimaryContainer(ctx *ChaosContext) error {
	// 1-5s stays below the 5-15s default node timeout: a blip, not a failover.
	duration := randomDuration(ctx.Rand, 1*time.Second, 5*time.Second)
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Pausing primary container in pod: %s for %s\n",
			pod, duration.Truncate(time.Millisecond))
		pods = append(pods, pod)
	}
	var paused []string
	for _, pod := range pods {
		if err := pauseContainer(pod, ctx.Namespace); err != nil {
			_ = unpauseContainers(paused, ctx.Namespace)
			return err
		}
		paused = append(paused, pod)
	}
	time.Sleep(duration)
	return unpauseContainers(paused, ctx.Namespace)
}

// pauseReplicaContainer freezes the server container of one replica per targeted shard, then resumes it.
func pauseReplicaContainer(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	// 1-5s stays below the 5-15s default node timeout: a blip, not a failover.
	duration := randomDuration(ctx.Rand, 1*time.Second, 5*time.Second)
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("pausing replica container for shard %d: %w", shard, err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Pausing replica container in pod: %s for %s\n",
			pod, duration.Truncate(time.Millisecond))
		pods = append(pods, pod)
	}
	var paused []string
	for _, pod := range pods {
		if err := pauseContainer(pod, ctx.Namespace); err != nil {
			_ = unpauseContainers(paused, ctx.Namespace)
			return err
		}
		paused = append(paused, pod)
	}
	time.Sleep(duration)
	return unpauseContainers(paused, ctx.Namespace)
}

// pauseWorkerNode freezes the worker nodes hosting the targeted primaries, then resumes them.
func pauseWorkerNode(ctx *ChaosContext) error {
	// Eviction threshold: 40s (node-monitor-grace) + tolerationSeconds.
	// Range spans below and above threshold to cover both eviction and non-eviction cases.
	evictionThreshold := 40*time.Second + time.Duration(ctx.TolerationSec)*time.Second
	duration := randomDuration(ctx.Rand, 3*time.Second, evictionThreshold+30*time.Second)
	var nodes []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		nodeName, err := getPodNodeName(pod, ctx.Namespace)
		if err != nil {
			return err
		}
		if slices.Contains(nodes, nodeName) {
			continue
		}
		if isProtectedNode(nodeName, ctx.Namespace) {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"  Skipping node %s (shard %d): hosts the controller-manager or background writer\n", nodeName, shard)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Will pause Kind node %s (primary pod: %s, shard %d)\n", nodeName, pod, shard)
		nodes = append(nodes, nodeName)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("skip: all targeted primaries are on the controller-manager or background writer node")
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "  Pausing %d node(s) for %s\n", len(nodes), duration.Truncate(time.Second))
	var paused []string
	for _, nodeName := range nodes {
		cmd := exec.Command("docker", "pause", nodeName)
		if _, err := utils.Run(cmd); err != nil {
			_ = unpauseWorkerNodes(paused)
			return err
		}
		paused = append(paused, nodeName)
	}
	time.Sleep(duration)
	return unpauseWorkerNodes(paused)
}

// unpauseWorkerNodes unpauses every given worker node, continuing past failures
// so a single error cannot leave the remaining nodes frozen. Returns the first error.
func unpauseWorkerNodes(workers []string) error {
	var firstErr error
	for _, worker := range workers {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Unpausing Kind node %s\n", worker)
		if err := unpauseWorkerNode(worker); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// stopWorkerNode stops one Kind node (hosting the first targeted shard's primary)
// and records it on ctx.StoppedNodes without restarting it; start-worker-node does
// that. Pair them as the ends of a compound, e.g.
// stop-worker-node+scale-shards+start-worker-node, to run scenarios while a node is
// down. Only one node is stopped, to keep quorum. Unlike pause (a transient stall,
// same instance resumes), a stop reboots the node: containers get new IPs and Valkey
// reloads cold. Alone it leaves the node down and the recovery check fails.
func stopWorkerNode(ctx *ChaosContext) error {
	if len(ctx.TargetShards) == 0 {
		return fmt.Errorf("skip: no target shards")
	}
	pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, ctx.TargetShards[0])
	if err != nil {
		return err
	}
	nodeName, err := getPodNodeName(pod, ctx.Namespace)
	if err != nil {
		return err
	}
	if slices.Contains(ctx.StoppedNodes, nodeName) {
		return fmt.Errorf("skip: node %s already stopped", nodeName)
	}
	// Stopping the operator's or writer's node fails the restart or writer check.
	if isProtectedNode(nodeName, ctx.Namespace) {
		return fmt.Errorf("skip: node %s hosts the controller-manager or background writer", nodeName)
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "  Stopping Kind node %s (primary pod: %s, shard %d)\n", nodeName, pod, ctx.TargetShards[0])
	// -t 0: kill at once so the kubelet stops heartbeating immediately.
	if _, err := utils.Run(exec.Command("docker", "stop", "-t", "0", nodeName)); err != nil {
		return err
	}
	ctx.StoppedNodes = append(ctx.StoppedNodes, nodeName)
	return nil
}

// startWorkerNode restarts the nodes stop-worker-node stopped (ctx.StoppedNodes)
// and clears the list. No-op when no node is stopped.
func startWorkerNode(ctx *ChaosContext) error {
	if len(ctx.StoppedNodes) == 0 {
		return fmt.Errorf("skip: no stopped worker node to start")
	}
	err := startWorkerNodes(ctx.StoppedNodes)
	ctx.StoppedNodes = nil
	return err
}

// startWorkerNodes starts every given worker node, continuing past failures so
// one error cannot leave the rest stopped. Returns the first error.
func startWorkerNodes(workers []string) error {
	var firstErr error
	for _, worker := range workers {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Starting Kind node %s\n", worker)
		if err := startNodeContainer(worker); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// partitionNode network-isolates one Kind node (hosting the first targeted
// shard's primary) and records it on ctx.PartitionedNodes without healing it;
// heal-worker-node does that. Pair them as the ends of a compound, e.g.
// partition-worker-node+scale-shards+heal-worker-node, to run scenarios while a
// node is cut off. Unlike stop, the node and its server keep running: the same
// instance rejoins on heal, demoting itself if its slots were failed over. Skips
// the controller and writer nodes. Alone it leaves the node partitioned and the
// recovery check fails.
func partitionNode(ctx *ChaosContext) error {
	if len(ctx.TargetShards) == 0 {
		return fmt.Errorf("skip: no target shards")
	}
	pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, ctx.TargetShards[0])
	if err != nil {
		return err
	}
	nodeName, err := getPodNodeName(pod, ctx.Namespace)
	if err != nil {
		return err
	}
	if slices.Contains(ctx.PartitionedNodes, nodeName) {
		return fmt.Errorf("skip: node %s already partitioned", nodeName)
	}
	if isProtectedNode(nodeName, ctx.Namespace) {
		return fmt.Errorf("skip: node %s hosts the controller-manager or background writer", nodeName)
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "  Partitioning Kind node %s (primary pod: %s, shard %d)\n", nodeName, pod, ctx.TargetShards[0])
	if err := partitionWorkerNode(nodeName); err != nil {
		_ = healWorkerNode(nodeName)
		return err
	}
	ctx.PartitionedNodes = append(ctx.PartitionedNodes, nodeName)
	return nil
}

// healNode heals the nodes partition-worker-node isolated (ctx.PartitionedNodes)
// and clears the list. No-op when no node is partitioned.
func healNode(ctx *ChaosContext) error {
	if len(ctx.PartitionedNodes) == 0 {
		return fmt.Errorf("skip: no partitioned worker node to heal")
	}
	err := healWorkerNodes(ctx.PartitionedNodes)
	ctx.PartitionedNodes = nil
	return err
}

// scaleShards patches spec.shards to a new count within the configured range.
func scaleShards(ctx *ChaosContext) error {
	if ctx.MinShards == ctx.MaxShards {
		return fmt.Errorf("skip: shard range is fixed at %d", ctx.MinShards)
	}
	// Pick a random shard count in [MinShards, MaxShards], excluding current.
	newShards := ctx.MinShards + ctx.Rand.Intn(ctx.MaxShards-ctx.MinShards+1)
	if newShards == ctx.Shards {
		// Ensure we actually change something, staying within the range.
		if newShards < ctx.MaxShards {
			newShards++
		} else {
			newShards--
		}
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "  Scaling shards from %d to %d\n", ctx.Shards, newShards)
	cmd := exec.Command("kubectl", "patch", "valkeycluster", ctx.ClusterName,
		"-n", ctx.Namespace, "--type=merge",
		"-p", fmt.Sprintf(`{"spec":{"shards":%d}}`, newShards))
	if _, err := utils.Run(cmd); err != nil {
		return err
	}
	ctx.Shards = newShards
	return nil
}

// scaleReplicas patches spec.replicas to a new count within the configured range.
func scaleReplicas(ctx *ChaosContext) error {
	if ctx.MaxReplicas == 0 {
		return fmt.Errorf("skip: replica range is fixed at 0")
	}
	// Pick a random replica count in [0, MaxReplicas], excluding current.
	newReplicas := ctx.Rand.Intn(ctx.MaxReplicas)
	if newReplicas >= ctx.Replicas {
		newReplicas++
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "  Scaling replicas from %d to %d\n", ctx.Replicas, newReplicas)
	cmd := exec.Command("kubectl", "patch", "valkeycluster", ctx.ClusterName,
		"-n", ctx.Namespace, "--type=merge",
		"-p", fmt.Sprintf(`{"spec":{"replicas":%d}}`, newReplicas))
	if _, err := utils.Run(cmd); err != nil {
		return err
	}
	ctx.Replicas = newReplicas
	ctx.LowestReplicas = min(ctx.LowestReplicas, newReplicas)
	return nil
}

// deleteRecreateCluster deletes the ValkeyCluster, waits for its pods to go, then recreates it from the captured spec.
func deleteRecreateCluster(ctx *ChaosContext) error {
	// Capture the current spec before deleting. Fetch the typed object so the
	// spec round-trips as valid JSON for the manifest below.
	cr, err := utils.GetValkeyClusterStatus(ctx.ClusterName)
	if err != nil {
		return fmt.Errorf("failed to get ValkeyCluster spec: %w", err)
	}
	spec, err := json.Marshal(cr.Spec)
	if err != nil {
		return fmt.Errorf("failed to marshal ValkeyCluster spec: %w", err)
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting ValkeyCluster %s and waiting for removal\n", ctx.ClusterName)
	cmd := exec.Command("kubectl", "delete", "valkeycluster", ctx.ClusterName,
		"-n", ctx.Namespace, "--wait=true", "--timeout=120s")
	if _, err := utils.Run(cmd); err != nil {
		return fmt.Errorf("failed to delete ValkeyCluster: %w", err)
	}

	// Wait for all pods to be gone to ensure the new cluster won't have
	// GetClusterState accidentally connect to a still-terminating old pod.
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", "-l",
			fmt.Sprintf("valkey.io/cluster=%s", ctx.ClusterName),
			"-n", ctx.Namespace, "-o", "jsonpath={.items[*].metadata.name}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "pods still exist: %s", output)
	}, 120*time.Second, 2*time.Second).Should(Succeed())

	_, _ = fmt.Fprintf(GinkgoWriter, "  Recreating ValkeyCluster %s with captured spec\n", ctx.ClusterName)
	manifest := fmt.Sprintf(`{"apiVersion":"valkey.io/v1alpha1","kind":"ValkeyCluster","metadata":{"name":"%s","namespace":"%s"},"spec":%s}`,
		ctx.ClusterName, ctx.Namespace, spec)

	cmd = exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if _, err := utils.Run(cmd); err != nil {
		return fmt.Errorf("failed to recreate ValkeyCluster: %w", err)
	}
	return nil
}

// rollingUpdate toggles a restart-requiring config value and waits for pods to be replaced.
func rollingUpdate(ctx *ChaosContext) error {
	// Toggle io-threads between 1 and 2 to trigger a restart-requiring config change.
	cmd := exec.Command("kubectl", "get", "valkeycluster", ctx.ClusterName,
		"-n", ctx.Namespace, "-o", "jsonpath={.spec.config.io-threads}")
	output, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("reading current io-threads: %w", err)
	}
	current := strings.TrimSpace(output)
	next := "2"
	if current == "2" {
		next = "1"
	}

	// Capture current pod UIDs to detect restarts.
	cmd = exec.Command("kubectl", "get", "pods", "-l",
		fmt.Sprintf("valkey.io/cluster=%s", ctx.ClusterName),
		"-n", ctx.Namespace, "-o", "jsonpath={range .items[*]}{.metadata.uid}{\"\\n\"}{end}")
	uidsBefore, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("reading pod UIDs before the rolling update: %w", err)
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "  Patching config io-threads=%s (was %q)\n", next, current)
	cmd = exec.Command("kubectl", "patch", "valkeycluster", ctx.ClusterName,
		"-n", ctx.Namespace, "--type=merge",
		"-p", fmt.Sprintf(`{"spec":{"config":{"io-threads":"%s"}}}`, next))
	if _, err := utils.Run(cmd); err != nil {
		return err
	}

	// Wait for at least one pod to be replaced (new UID).
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", "-l",
			fmt.Sprintf("valkey.io/cluster=%s", ctx.ClusterName),
			"-n", ctx.Namespace, "-o", "jsonpath={range .items[*]}{.metadata.uid}{\"\\n\"}{end}")
		uidsAfter, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(uidsAfter).NotTo(Equal(uidsBefore), "no pods restarted after config change")
	}, 60*time.Second, 2*time.Second).Should(Succeed())

	return nil
}

// deleteControllerPod force deletes the operator pod and waits for its replacement to be Ready.
func deleteControllerPod(_ *ChaosContext) error {
	cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
		"-n", namespace, "-o", "jsonpath={.items[0].metadata.name}")
	podName, err := utils.Run(cmd)
	if err != nil {
		return err
	}
	podName = strings.TrimSpace(podName)
	_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting controller pod: %s\n", podName)
	cmd = exec.Command("kubectl", "delete", "pod", podName, "-n", namespace, "--grace-period=0", "--force")
	if _, err := utils.Run(cmd); err != nil {
		return err
	}
	// Wait for the new controller pod to become Ready.
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
			"-n", namespace, "-o", "jsonpath={.items[0].status.conditions[?(@.type==\"Ready\")].status}")
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("True"))
	}, 60*time.Second, 2*time.Second).Should(Succeed())
	return nil
}

//----------------------------------------------------------

// expectNoControllerRestarts fails when an operator manager container has
// restarted. A replaced operator pod starts again from zero, so
// delete-controller-pod does not count.
func expectNoControllerRestarts(when string) {
	restarts, err := controllerRestarts()
	Expect(err).NotTo(HaveOccurred(), "Failed to get the operator restart count")
	for pod, n := range restarts {
		Expect(n).To(BeZero(), "operator container in pod %s restarted %s", pod, when)
	}
}

// logPreviousControllerLogs logs the previous run of every operator container
// that has restarted.
func logPreviousControllerLogs() {
	restarts, err := controllerRestarts()
	if err != nil {
		return
	}
	for pod, n := range restarts {
		if n == 0 {
			continue
		}
		if logs, err := getPreviousManagerLogs(pod); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Previous controller logs of %s (%d restarts):\n%s\n", pod, n, logs)
		}
	}
}

// logClusterState logs pod placement and CLUSTER NODES with a label (e.g. "before" or "after").
func logClusterState(clusterName, namespace, label string) {
	if output, err := getPodsWide(clusterName, namespace); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "  PODS %s:\n%s\n", label, output)
	}
	if output, err := getClusterNodesOutput(clusterName, namespace); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "  CLUSTER NODES %s:\n%s", label, output)
	}
}

// randomDuration returns a random duration between min and max.
func randomDuration(rnd *rand.Rand, min, max time.Duration) time.Duration {
	if max <= min {
		return max
	}
	return min + time.Duration(rnd.Int63n(int64(max-min)))
}

// getControllerNodeName returns the node hosting the controller-manager pod.
func getControllerNodeName() string {
	cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
		"-n", namespace, "-o", "jsonpath={.items[0].spec.nodeName}")
	out, err := utils.Run(cmd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// isProtectedNode reports whether nodeName hosts the controller-manager or the
// background writer, which node faults skip: losing either fails the restart or
// writer check. The writer starts on the operator's node, but the operator can
// move (delete-controller-pod), so both are looked up live.
func isProtectedNode(nodeName, namespace string) bool {
	return nodeName == getControllerNodeName() || nodeName == getBackgroundClientNodeName(namespace)
}

// Helper functions for configuration parsing

// envOrDefault returns the env var value, or defaultVal when unset.
func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// envBool returns the env var as a bool, or defaultVal when unset. Invalid values fail the suite.
func envBool(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		Fail(fmt.Sprintf("%s=%q is not a valid boolean", key, v))
	}
	return b
}

// envOneOf returns the env var value, which must be one of valid, or defaultVal when unset.
func envOneOf(key, defaultVal string, valid []string) string {
	v := envOrDefault(key, defaultVal)
	for _, opt := range valid {
		if v == opt {
			return v
		}
	}
	Fail(fmt.Sprintf("%s=%q is invalid, must be one of %v", key, v, valid))
	return ""
}

// envIntOrDefault returns the env var as an int, or defaultVal when unset. Invalid or too small values fail the suite.
func envIntOrDefault(key string, defaultVal int, minVal ...int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		Fail(fmt.Sprintf("%s=%q is not a valid integer", key, v))
	}
	if len(minVal) > 0 && i < minVal[0] {
		Fail(fmt.Sprintf("%s=%d must be >= %d", key, i, minVal[0]))
	}
	return i
}

// envInt64OrDefault returns the env var as an int64, or defaultVal when unset.
func envInt64OrDefault(key string, defaultVal int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		Fail(fmt.Sprintf("%s=%q is not a valid integer", key, v))
	}
	return i
}

const (
	chaosRecoveryPerPod      = 30 * time.Second
	chaosRecoveryConvergence = 60 * time.Second
	chaosRecoveryMinTimeout  = 5 * time.Minute
)

// calcTimeout returns a recovery timeout scaled to the cluster size: a per-pod
// roll cost plus a fixed convergence tail, floored at chaosRecoveryMinTimeout.
func calcTimeout(shards, replicas int) time.Duration {
	pods := shards * (replicas + 1)
	t := time.Duration(pods)*chaosRecoveryPerPod + chaosRecoveryConvergence
	if t < chaosRecoveryMinTimeout {
		t = chaosRecoveryMinTimeout
	}
	return t
}

// envDurationOrDefault returns the env var as a duration, or defaultVal when unset.
func envDurationOrDefault(key string, defaultVal time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		Fail(fmt.Sprintf("%s=%q is not a valid duration (e.g. 30s, 5m)", key, v))
	}
	return d
}

// filterScenarios resolves CHAOS_SCENARIOS into the scenarios to run. An empty
// filter selects everything except DisabledByDefault, plus defaultCompounds.
// Names joined by + become a single compound scenario. Unknown names fail the
// suite.
func filterScenarios(all []Scenario, filter string) []Scenario {
	if filter == "" {
		var result []Scenario
		for _, s := range all {
			if !s.DisabledByDefault {
				result = append(result, s)
			}
		}
		for _, c := range defaultCompounds {
			result = append(result, compoundScenario(c.name, compoundMaxDelay(c.name)))
		}
		return result
	}
	var result []Scenario
	for _, name := range strings.Split(filter, ",") {
		name = strings.TrimSpace(name)
		if strings.Contains(name, "+") {
			result = append(result, compoundScenario(name, compoundMaxDelay(name)))
			continue
		}
		found := false
		for _, s := range all {
			if s.Name == name {
				result = append(result, s)
				found = true
				break
			}
		}
		if !found {
			Fail(fmt.Sprintf("CHAOS_SCENARIOS contains unknown scenario: %q", name))
		}
	}
	return result
}

// compoundScenario builds the compound scenario for names joined by +, such as
// "scale-shards+delete-primary-pod", with the longest random delay between its
// steps. Unknown names fail the suite.
func compoundScenario(name string, maxDelay time.Duration) Scenario {
	parts := strings.Split(name, "+")
	group := make([]string, len(parts))
	for i, p := range parts {
		group[i] = strings.TrimSpace(p)
		if scenarioByName(group[i]) == nil {
			Fail(fmt.Sprintf("compound scenario %q contains unknown scenario: %q", name, group[i]))
		}
	}
	return Scenario{
		Name:   name,
		Steps:  group,
		Inject: makeCompoundInject(group, maxDelay),
	}
}

// compoundMaxDelay returns the longest random delay between the steps of the
// named compound: its delay in defaultCompounds, or defaultCompoundDelay.
func compoundMaxDelay(name string) time.Duration {
	for _, c := range defaultCompounds {
		if c.name == name && c.maxDelay > 0 {
			return c.maxDelay
		}
	}
	return defaultCompoundDelay
}

// scenarioByName looks up a scenario in allScenarios, returning nil when absent.
func scenarioByName(name string) *Scenario {
	for i := range allScenarios {
		if allScenarios[i].Name == name {
			return &allScenarios[i]
		}
	}
	return nil
}

// makeCompoundInject builds an Inject that runs each named scenario in turn with
// a random delay below maxDelay between them, or below ctx.CompoundMaxDelay when
// set, so their faults overlap. Sub-scenarios that skip are logged and do not
// abort the group.
func makeCompoundInject(group []string, maxDelay time.Duration) func(*ChaosContext) error {
	return func(ctx *ChaosContext) error {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Compound test: %s\n", strings.Join(group, " + "))
		if ctx.CompoundMaxDelay > 0 {
			maxDelay = ctx.CompoundMaxDelay
		}
		for i, n := range group {
			if i > 0 {
				var delay time.Duration
				if maxSec := int(maxDelay / time.Second); maxSec > 0 {
					delay = time.Duration(ctx.Rand.Intn(maxSec)) * time.Second
				}
				_, _ = fmt.Fprintf(GinkgoWriter, "    delay %s before %s\n", delay, n)
				time.Sleep(delay)
				// Shows how far the earlier steps had got when this one starts.
				logClusterState(ctx.ClusterName, ctx.Namespace, "before "+n)
			}
			s := scenarioByName(n)
			if s == nil {
				return fmt.Errorf("unknown scenario in compound: %s", n)
			}
			if !s.IgnoresTargets {
				// An earlier scale-shards step may have removed targeted shards.
				targets := make([]int, 0, len(ctx.TargetShards))
				for _, shard := range ctx.TargetShards {
					if shard < ctx.Shards {
						targets = append(targets, shard)
					}
				}
				if len(targets) == 0 {
					_, _ = fmt.Fprintf(GinkgoWriter, "    %s skipped: no targeted shard left after scaling to %d shards\n",
						n, ctx.Shards)
					continue
				}
				ctx.TargetShards = targets
			}
			if err := s.Inject(ctx); err != nil {
				if strings.Contains(err.Error(), "skip:") {
					_, _ = fmt.Fprintf(GinkgoWriter, "    %s skipped: %s\n", n, err)
					continue
				}
				return err
			}
		}
		return nil
	}
}

// envFloat64OrDefault returns the env var as a float64, or defaultVal when unset.
func envFloat64OrDefault(key string, defaultVal float64, minVal float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		Fail(fmt.Sprintf("%s=%q is not a valid float", key, v))
	}
	if f < minVal {
		Fail(fmt.Sprintf("%s=%.2f must be >= %.2f", key, f, minVal))
	}
	return f
}
