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
	ClusterName   string
	Namespace     string
	WorkloadType  string
	TargetShards  []int
	Shards        int
	MinShards     int
	MaxShards     int
	Replicas      int
	MaxReplicas   int
	TolerationSec int
	Rand          *rand.Rand
}

// Scenario defines a named chaos scenario.
type Scenario struct {
	Name                 string
	LosesData            bool // may lose data, skip verification and re-seed
	LosesDataIfNoReplica bool // loses data only when replicas == 0
	DisabledByDefault    bool // excluded unless explicitly listed in CHAOS_SCENARIOS
	Inject               func(ctx *ChaosContext) error
}

// losesData reports whether the scenario is expected to lose data at the given
// replica count. Scenarios that kill a whole shard only lose data when there is
// no replica to fail over to.
func (s Scenario) losesData(replicas int) bool {
	return s.LosesData || (s.LosesDataIfNoReplica && replicas == 0)
}

var allScenarios = []Scenario{
	{Name: "delete-primary-pod", LosesDataIfNoReplica: true, Inject: deletePrimaryPod},
	{Name: "delete-replica-pod", Inject: deleteReplicaPod},
	{Name: "delete-shard-pods", LosesData: true, Inject: deleteShardPods},
	{Name: "delete-primary-workload", LosesDataIfNoReplica: true, Inject: deletePrimaryWorkload},
	{Name: "delete-replica-workload", Inject: deleteReplicaWorkload},
	{Name: "pause-primary-container", Inject: pausePrimaryContainer},
	{Name: "pause-replica-container", Inject: pauseReplicaContainer},
	{Name: "scale-shards", Inject: scaleShards},
	{Name: "scale-replicas", Inject: scaleReplicas},
	{Name: "rolling-update", LosesDataIfNoReplica: true, Inject: rollingUpdate},
	{Name: "delete-recreate-cluster", LosesData: true, Inject: deleteRecreateCluster},
	{Name: "delete-controller-pod", Inject: deleteControllerPod},
	{Name: "pause-worker-node", DisabledByDefault: true, LosesData: true, Inject: pauseWorkerNode},
	{Name: "network-partition-primary", DisabledByDefault: true, LosesDataIfNoReplica: true, Inject: networkPartitionPrimary},
	{Name: "network-partition-replica", DisabledByDefault: true, Inject: networkPartitionReplica},
}

var _ = Describe("ValkeyCluster Chaos", Label("chaos"), Ordered, func() {
	var (
		clusterName     = "chaos-test-cluster"
		workloadType    string
		persistence     bool
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
	)

	BeforeAll(func() {
		// Parse configuration from environment
		workloadType = envOneOf("CHAOS_WORKLOAD_TYPE", "StatefulSet", []string{"StatefulSet", "Deployment"})
		persistence = envBool("CHAOS_PERSISTENCE", false)
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

		// Log configuration
		_, _ = fmt.Fprintf(GinkgoWriter, "=== Chaos Test Configuration ===\n")
		_, _ = fmt.Fprintf(GinkgoWriter, "  WorkloadType:     %s\n", workloadType)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Persistence:      %v\n", persistence)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Shards:           %d (min=%d, max=%d)\n", shards, minShards, maxShards)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Replicas:         %d (max=%d)\n", replicas, maxReplicas)
		_, _ = fmt.Fprintf(GinkgoWriter, "  NumKeys:          %d\n", numKeys)
		_, _ = fmt.Fprintf(GinkgoWriter, "  DataSize:         %d\n", dataSize)
		_, _ = fmt.Fprintf(GinkgoWriter, "  TargetShards:     %s\n", targetShards)
		_, _ = fmt.Fprintf(GinkgoWriter, "  RecoveryTimeout:  %s\n", recoveryTimeout)
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
`, clusterName, shards, replicas, workloadType)

		if tolerationSec > 0 {
			manifest += fmt.Sprintf(`  tolerations:
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
		}
	})

	AfterAll(func() {
		stopBackgroundClient("default")
		By("cleaning up chaos cluster")
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "-n", "default", "--ignore-not-found=true")
		_, _ = utils.Run(cmd)
	})

	It("runs fault injection until failure", func() {
		scenarioCount := make(map[string]int)

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
				ClusterName:   clusterName,
				Namespace:     "default",
				WorkloadType:  workloadType,
				TargetShards:  targetShardsForIteration,
				Shards:        shards,
				MinShards:     minShards,
				MaxShards:     maxShards,
				Replicas:      replicas,
				MaxReplicas:   maxReplicas,
				TolerationSec: tolerationSec,
				Rand:          rnd,
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

			if !scenario.losesData(replicas) {
				By(fmt.Sprintf("Iteration %d: verifying test data integrity", iteration))
				Eventually(func() error {
					return verifyTestData(clusterName, "default", seededKeys)
				}, 60*time.Second).Should(Succeed(),
					fmt.Sprintf("Iteration %d: data integrity check failed (seed=%d)", iteration, seed))
			} else {
				By(fmt.Sprintf("Iteration %d: checking for data loss (scenario may lose data)", iteration))
				if err := verifyTestData(clusterName, "default", seededKeys); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "  WARNING: data lost (expected): %s\n", err)
				}
				By(fmt.Sprintf("Iteration %d: re-seeding test data after data-loss scenario", iteration))
				err := flushAll(clusterName, "default")
				Expect(err).NotTo(HaveOccurred(), "Failed to flush data")
				seededKeys, err = startBackgroundClient(clusterName, "default", numKeys, dataSize, writeRPS)
				Expect(err).NotTo(HaveOccurred(), "Failed to re-seed test data")
				_, _ = fmt.Fprintf(GinkgoWriter, "  Seeded keys:      %d\n", seededKeys)
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "  Iteration %d: PASSED\n", iteration)

			// Print statistics every 10th iteration
			scenarioCount[scenario.Name]++
			if len(scenarios) > 1 && iteration%10 == 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Scenario Statistics (after %d iterations) ===\n", iteration)
				for _, s := range scenarios {
					_, _ = fmt.Fprintf(GinkgoWriter, "  %-30s %d\n", s.Name, scenarioCount[s.Name])
				}
				_, _ = fmt.Fprintf(GinkgoWriter, "=================================================\n")
			}
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
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting primary pod: %s (shard %d)\n", pod, shard)
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
			return fmt.Errorf("skip: %w", err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting replica pod: %s (shard %d)\n", pod, shard)
		pods = append(pods, pod)
	}
	for _, pod := range pods {
		if err := deletePodByName(pod, ctx.Namespace); err != nil {
			return err
		}
	}
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
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting primary %s: %s (shard %d)\n", ctx.WorkloadType, workload, shard)
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
			return fmt.Errorf("skip: %w", err)
		}
		workload, err := getWorkloadForPod(pod, ctx.Namespace, ctx.WorkloadType)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting replica %s: %s (shard %d)\n", ctx.WorkloadType, workload, shard)
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
	// Without tolerations: 3-5s, enough to trigger failover.
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
		_, _ = fmt.Fprintf(GinkgoWriter, "  Will partition node %s (primary pod: %s, shard %d)\n", nodeName, pod, shard)
		logIfControllerNode(nodeName)
		nodes = append(nodes, nodeName)
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
	// Without tolerations: 3-5s, enough to trigger failover.
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
			return fmt.Errorf("skip: %w", err)
		}
		nodeName, err := getPodNodeName(pod, ctx.Namespace)
		if err != nil {
			return err
		}
		if slices.Contains(nodes, nodeName) {
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Will partition node %s (replica pod: %s, shard %d)\n", nodeName, pod, shard)
		logIfControllerNode(nodeName)
		nodes = append(nodes, nodeName)
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
	// 1-5s covers both non-failover (<2s timeout) and failover (>2s) cases
	duration := randomDuration(ctx.Rand, 1*time.Second, 5*time.Second)
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Pausing primary container in pod: %s (shard %d) for %s\n", pod, shard, duration.Truncate(time.Millisecond))
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
	// 1-5s covers both non-failover (<2s timeout) and failover (>2s) cases
	duration := randomDuration(ctx.Rand, 1*time.Second, 5*time.Second)
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := getShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("skip: %w", err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Pausing replica container in pod: %s (shard %d) for %s\n", pod, shard, duration.Truncate(time.Millisecond))
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
		_, _ = fmt.Fprintf(GinkgoWriter, "  Will pause Kind node %s (primary pod: %s, shard %d)\n", nodeName, pod, shard)
		logIfControllerNode(nodeName)
		nodes = append(nodes, nodeName)
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
	output, _ := utils.Run(cmd)
	current := strings.TrimSpace(output)
	next := "2"
	if current == "2" {
		next = "1"
	}

	// Capture current pod UIDs to detect restarts.
	cmd = exec.Command("kubectl", "get", "pods", "-l",
		fmt.Sprintf("valkey.io/cluster=%s", ctx.ClusterName),
		"-n", ctx.Namespace, "-o", "jsonpath={range .items[*]}{.metadata.uid}{\"\\n\"}{end}")
	uidsBefore, _ := utils.Run(cmd)

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

// logIfControllerNode emits a warning if the target node hosts the controller-manager.
func logIfControllerNode(nodeName string) {
	if nodeName == getControllerNodeName() {
		_, _ = fmt.Fprintf(GinkgoWriter, "  WARNING: target node %s hosts the controller-manager; operator will be disrupted\n", nodeName)
	}
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

// calcTimeout returns a recovery timeout scaled to the cluster size: 15s per pod, minimum 5 minutes.
func calcTimeout(shards, replicas int) time.Duration {
	t := time.Duration(shards*(replicas+1)) * 15 * time.Second
	if t < 5*time.Minute {
		t = 5 * time.Minute
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
// filter selects everything except DisabledByDefault. Names joined by + become
// a single compound scenario. Unknown names fail the suite.
func filterScenarios(all []Scenario, filter string) []Scenario {
	if filter == "" {
		var result []Scenario
		for _, s := range all {
			if !s.DisabledByDefault {
				result = append(result, s)
			}
		}
		return result
	}
	var result []Scenario
	for _, name := range strings.Split(filter, ",") {
		name = strings.TrimSpace(name)
		if strings.Contains(name, "+") {
			// Ad-hoc compound: "scale-shards+delete-primary-pod"
			parts := strings.Split(name, "+")
			group := make([]string, len(parts))
			for i, p := range parts {
				group[i] = strings.TrimSpace(p)
				if scenarioByName(group[i]) == nil {
					Fail(fmt.Sprintf("CHAOS_SCENARIOS compound %q contains unknown scenario: %q", name, group[i]))
				}
			}
			result = append(result, Scenario{
				Name:      name,
				LosesData: true, // compound scenarios may lose data due to overlapping faults
				Inject:    makeCompoundInject(group),
			})
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
// a random delay between them, so their faults overlap. Sub-scenarios that skip
// are logged and do not abort the group.
func makeCompoundInject(group []string) func(*ChaosContext) error {
	return func(ctx *ChaosContext) error {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Compound test: %s\n", strings.Join(group, " + "))
		for i, n := range group {
			if i > 0 {
				delay := time.Duration(ctx.Rand.Intn(10)) * time.Second
				_, _ = fmt.Fprintf(GinkgoWriter, "    delay %s before %s\n", delay, n)
				time.Sleep(delay)
			}
			s := scenarioByName(n)
			if s == nil {
				return fmt.Errorf("unknown scenario in compound: %s", n)
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
