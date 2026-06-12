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

	valkeyiov1alpha1 "valkey.io/valkey-operator/api/v1alpha1"
	"valkey.io/valkey-operator/test/utils"
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
	{Name: "rolling-update", Inject: rollingUpdate},
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
	)

	BeforeAll(func() {
		// Parse configuration from environment
		workloadType = envOneOf("CHAOS_WORKLOAD_TYPE", "StatefulSet", []string{"StatefulSet", "Deployment"})
		persistence = envBool("CHAOS_PERSISTENCE", false)
		shards = envIntOrDefault("CHAOS_SHARDS", 3, 1 /* min */)
		minShards = envIntOrDefault("CHAOS_MIN_SHARDS", shards, 1 /* min */)
		maxShards = envIntOrDefault("CHAOS_MAX_SHARDS", shards+3, minShards /* min */)
		replicas = envIntOrDefault("CHAOS_REPLICAS", 1, 0 /* min */)
		numKeys = envIntOrDefault("CHAOS_NUM_KEYS", 100000, 0 /* min */)
		dataSize = envIntOrDefault("CHAOS_DATA_SIZE", 3, 1 /* min */)
		targetShards = envOrDefault("CHAOS_TARGET_SHARDS", "random")
		mode = envOneOf("CHAOS_MODE", "random", []string{"random", "sequential"})
		// Scale timeout with cluster size: 15s per pod, minimum 5 minutes.
		defaultTimeout := time.Duration(shards*(replicas+1)) * 15 * time.Second
		if defaultTimeout < 5*time.Minute {
			defaultTimeout = 5 * time.Minute
		}
		recoveryTimeout = envDurationOrDefault("CHAOS_RECOVERY_TIMEOUT", defaultTimeout)
		tolerationSec = envIntOrDefault("CHAOS_TOLERATION_SECONDS", 0, 0 /* min */)
		seed = envInt64OrDefault("CHAOS_SEED", time.Now().UnixNano())
		scenarios = filterScenarios(allScenarios, envOrDefault("CHAOS_SCENARIOS", ""))
		cpuPressure = envBool("CHAOS_CPU_PRESSURE", false)
		cpuMin = envFloat64OrDefault("CHAOS_CPU_MIN", 0.3, 0.1)
		cpuMax = envFloat64OrDefault("CHAOS_CPU_MAX", 1.0, cpuMin)
		if cpuPressure {
			workerNodes = utils.GetWorkerNodes()
		}

		rnd = rand.New(rand.NewSource(seed))

		// Log configuration
		_, _ = fmt.Fprintf(GinkgoWriter, "=== Chaos Test Configuration ===\n")
		_, _ = fmt.Fprintf(GinkgoWriter, "  WorkloadType:     %s\n", workloadType)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Persistence:      %v\n", persistence)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Shards:           %d (min=%d, max=%d)\n", shards, minShards, maxShards)
		_, _ = fmt.Fprintf(GinkgoWriter, "  Replicas:         %d\n", replicas)
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
		for _, s := range allScenarios {
			status := "enabled"
			if !enabledNames[s.Name] {
				status = "disabled"
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "    - %-30s [%s]\n", s.Name, status)
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
			err = utils.VerifyClusterHealth(clusterName, "default", shards, replicas)
			g.Expect(err).NotTo(HaveOccurred())
		}, recoveryTimeout, 5*time.Second).Should(Succeed())

		By("seeding test data")
		seededKeys, err = utils.SeedTestData(clusterName, "default", numKeys, dataSize, seed)
		Expect(err).NotTo(HaveOccurred(), "Failed to seed test data")
		_, _ = fmt.Fprintf(GinkgoWriter, "  Seeded keys:      %d\n", seededKeys)
	})

	AfterEach(func() {
		// Always remove CPU pressure to avoid leaving nodes throttled
		if cpuPressure {
			utils.UnthrottleNodes(workerNodes)
		}

		if CurrentSpecReport().Failed() {
			// Dump keyspace counts
			if total, perShard, err := utils.GetTotalKeyCount(clusterName, "default"); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "KEY COUNT at failure: total=%d, per-shard=%v\n", total, perShard)
			}

			// Dump cluster-specific state
			By("collecting chaos cluster debug info")
			cmd := exec.Command("kubectl", "get", "pods", "-l",
				fmt.Sprintf("valkey.io/cluster=%s", clusterName), "-o", "wide")
			if output, err := utils.Run(cmd); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Cluster pods:\n%s\n", output)
			}
			cmd = exec.Command("kubectl", "get", "valkeycluster", clusterName, "-o", "yaml")
			if output, err := utils.Run(cmd); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "ValkeyCluster status:\n%s\n", output)
			}
			cmd = exec.Command("kubectl", "get", "valkeynodes", "-l",
				fmt.Sprintf("valkey.io/cluster=%s", clusterName), "-o", "wide")
			if output, err := utils.Run(cmd); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "ValkeyNodes:\n%s\n", output)
			}

			// Collect logs and CLUSTER NODES from all valkey node pods
			cmd = exec.Command("kubectl", "get", "pods", "-l",
				fmt.Sprintf("valkey.io/cluster=%s", clusterName),
				"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			if podList, err := utils.Run(cmd); err == nil {
				for _, pod := range utils.GetNonEmptyLines(podList) {
					cmd = exec.Command("kubectl", "exec", pod, "-c", "server", "--",
						"valkey-cli", "CLUSTER", "NODES")
					if output, err := utils.Run(cmd); err == nil {
						_, _ = fmt.Fprintf(GinkgoWriter, "CLUSTER NODES from %s:\n%s\n", pod, output)
					}
					cmd = exec.Command("kubectl", "logs", pod, "-c", "server", "--tail=100")
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
		By("cleaning up chaos cluster")
		cmd := exec.Command("kubectl", "delete", "valkeycluster", clusterName, "--ignore-not-found=true")
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
			switch targetShards {
			case "random":
				count := rnd.Intn(shards) + 1
				targetShardsForIteration = rnd.Perm(shards)[:count]
			case "all":
				targetShardsForIteration = make([]int, shards)
				for i := range shards {
					targetShardsForIteration[i] = i
				}
			default:
				for _, s := range strings.Split(targetShards, ",") {
					if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
						targetShardsForIteration = append(targetShardsForIteration, v)
					}
				}
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- Iteration %d: scenario=%s ---\n",
				iteration, scenario.Name)

			logClusterState(clusterName, "default", "before")

			// Log per-shard key counts before scenario
			if _, perShard, err := utils.GetTotalKeyCount(clusterName, "default"); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "  KEY COUNT before: %v\n", perShard)
			}

			// Mark iteration start in all valkey node logs
			logMsg := fmt.Sprintf("CHAOS-TEST: iteration %d scenario=%s", iteration, scenario.Name)
			cmd := exec.Command("kubectl", "get", "pods", "-l",
				fmt.Sprintf("valkey.io/cluster=%s", clusterName),
				"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			if podList, err := utils.Run(cmd); err == nil {
				for _, pod := range utils.GetNonEmptyLines(podList) {
					cmd = exec.Command("kubectl", "exec", pod, "-c", "server", "--",
						"valkey-cli", "EVAL", fmt.Sprintf("return server.log(server.LOG_WARNING, '%s')", logMsg), "0")
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
				TolerationSec: tolerationSec,
				Rand:          rnd,
			}

			// Apply random CPU pressure to Kind worker nodes
			if cpuPressure {
				utils.UnthrottleNodes(workerNodes)
				throttledNodes = utils.ThrottleRandomWorkerNodes(rnd, workerNodes, cpuMin, cpuMax)
				if len(throttledNodes) > 0 {
					_, _ = fmt.Fprintf(GinkgoWriter, "  CPU pressure on %v\n", throttledNodes)
				}
			}

			err := scenario.Inject(ctx)
			if err != nil {
				if strings.Contains(err.Error(), "skip:") {
					_, _ = fmt.Fprintf(GinkgoWriter, "  Skipped: %s\n", err)
					utils.UnthrottleNodes(throttledNodes)
					continue
				}
				Fail(fmt.Sprintf("Iteration %d: scenario %s failed to inject: %v", iteration, scenario.Name, err))
			}

			// Update shards/replicas in case a scale scenario changed them
			shards = ctx.Shards
			replicas = ctx.Replicas

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
				err = utils.VerifyK8sResources(clusterName, "default", workloadType, shards, replicas)
				g.Expect(err).NotTo(HaveOccurred(), "K8s resources not ready: %v", err)
				err = utils.VerifyClusterHealth(clusterName, "default", shards, replicas)
				g.Expect(err).NotTo(HaveOccurred(), "cluster health: %v", err)
			}, recoveryTimeout, 5*time.Second).Should(Succeed(),
				fmt.Sprintf("Iteration %d: cluster did not recover after %s (scenario=%s, shards=%v, seed=%d)",
					iteration, recoveryTimeout, scenario.Name, targetShardsForIteration, seed))

			// Remove CPU pressure after recovery
			utils.UnthrottleNodes(throttledNodes)

			// Log cluster state after recovery
			logClusterState(clusterName, "default", "after")

			if !scenario.losesData(replicas) {
				By(fmt.Sprintf("Iteration %d: verifying test data integrity", iteration))
				Eventually(func() error {
					return utils.VerifyTestData(clusterName, "default", seededKeys)
				}, 60*time.Second).Should(Succeed(),
					fmt.Sprintf("Iteration %d: data integrity check failed (seed=%d)", iteration, seed))
			} else {
				By(fmt.Sprintf("Iteration %d: re-seeding test data after data-loss scenario", iteration))
				err := utils.FlushAll(clusterName, "default")
				Expect(err).NotTo(HaveOccurred(), "Failed to flush data")
				seededKeys, err = utils.SeedTestData(clusterName, "default", numKeys, dataSize, seed)
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

func deletePrimaryPod(ctx *ChaosContext) error {
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := utils.GetShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting primary pod: %s (shard %d)\n", pod, shard)
		pods = append(pods, pod)
	}
	for _, pod := range pods {
		if err := utils.DeletePod(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	return nil
}

func deleteReplicaPod(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := utils.GetShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("skip: %w", err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting replica pod: %s (shard %d)\n", pod, shard)
		pods = append(pods, pod)
	}
	for _, pod := range pods {
		if err := utils.DeletePod(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	return nil
}

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
		for _, pod := range utils.GetNonEmptyLines(output) {
			pods = append(pods, pod)
		}
	}

	for _, pod := range pods {
		if err := utils.DeletePod(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	return nil
}

func deletePrimaryWorkload(ctx *ChaosContext) error {
	var workloads []string
	for _, shard := range ctx.TargetShards {
		pod, err := utils.GetShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		workload, err := utils.GetWorkloadForPod(pod, ctx.Namespace, ctx.WorkloadType)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting primary %s: %s (shard %d)\n", ctx.WorkloadType, workload, shard)
		workloads = append(workloads, workload)
	}
	for _, workload := range workloads {
		if err := utils.DeleteWorkload(workload, ctx.Namespace, ctx.WorkloadType); err != nil {
			return err
		}
	}
	return nil
}

func deleteReplicaWorkload(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	var workloads []string
	for _, shard := range ctx.TargetShards {
		pod, err := utils.GetShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("skip: %w", err)
		}
		workload, err := utils.GetWorkloadForPod(pod, ctx.Namespace, ctx.WorkloadType)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting replica %s: %s (shard %d)\n", ctx.WorkloadType, workload, shard)
		workloads = append(workloads, workload)
	}
	for _, workload := range workloads {
		if err := utils.DeleteWorkload(workload, ctx.Namespace, ctx.WorkloadType); err != nil {
			return err
		}
	}
	return nil
}

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
		pod, err := utils.GetShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		nodeName, err := utils.GetPodNodeName(pod, ctx.Namespace)
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
	for _, nodeName := range nodes {
		if err := utils.PartitionNode(nodeName); err != nil {
			return err
		}
	}
	time.Sleep(duration)
	for _, nodeName := range nodes {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Healing node %s\n", nodeName)
		if err := utils.HealNode(nodeName); err != nil {
			return err
		}
	}
	return nil
}

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
		pod, err := utils.GetShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("skip: %w", err)
		}
		nodeName, err := utils.GetPodNodeName(pod, ctx.Namespace)
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
	for _, nodeName := range nodes {
		if err := utils.PartitionNode(nodeName); err != nil {
			return err
		}
	}
	time.Sleep(duration)
	for _, nodeName := range nodes {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Healing node %s\n", nodeName)
		if err := utils.HealNode(nodeName); err != nil {
			return err
		}
	}
	return nil
}

func pausePrimaryContainer(ctx *ChaosContext) error {
	// 1-5s covers both non-failover (<2s timeout) and failover (>2s) cases
	duration := randomDuration(ctx.Rand, 1*time.Second, 5*time.Second)
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := utils.GetShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Pausing primary container in pod: %s (shard %d) for %s\n", pod, shard, duration.Truncate(time.Millisecond))
		pods = append(pods, pod)
	}
	for _, pod := range pods {
		if err := utils.PauseContainer(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	time.Sleep(duration)
	for _, pod := range pods {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Unpausing primary container in pod: %s\n", pod)
		if err := utils.UnpauseContainer(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	return nil
}

func pauseReplicaContainer(ctx *ChaosContext) error {
	if ctx.Replicas == 0 {
		return fmt.Errorf("skip: no replicas configured")
	}
	// 1-5s covers both non-failover (<2s timeout) and failover (>2s) cases
	duration := randomDuration(ctx.Rand, 1*time.Second, 5*time.Second)
	var pods []string
	for _, shard := range ctx.TargetShards {
		pod, err := utils.GetShardReplicaPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return fmt.Errorf("skip: %w", err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Pausing replica container in pod: %s (shard %d) for %s\n", pod, shard, duration.Truncate(time.Millisecond))
		pods = append(pods, pod)
	}
	for _, pod := range pods {
		if err := utils.PauseContainer(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	time.Sleep(duration)
	for _, pod := range pods {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Unpausing replica container in pod: %s\n", pod)
		if err := utils.UnpauseContainer(pod, ctx.Namespace); err != nil {
			return err
		}
	}
	return nil
}

func pauseWorkerNode(ctx *ChaosContext) error {
	// Eviction threshold: 40s (node-monitor-grace) + tolerationSeconds.
	// Range spans below and above threshold to cover both eviction and non-eviction cases.
	evictionThreshold := 40*time.Second + time.Duration(ctx.TolerationSec)*time.Second
	duration := randomDuration(ctx.Rand, 3*time.Second, evictionThreshold+30*time.Second)
	var paused []string
	for _, shard := range ctx.TargetShards {
		pod, err := utils.GetShardPrimaryPod(ctx.ClusterName, ctx.Namespace, shard)
		if err != nil {
			return err
		}
		nodeName, err := utils.GetPodNodeName(pod, ctx.Namespace)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "  Pausing Kind node %s (primary pod: %s, shard %d) for %s\n", nodeName, pod, shard, duration.Truncate(time.Second))
		logIfControllerNode(nodeName)
		cmd := exec.Command("docker", "pause", nodeName)
		if _, err := utils.Run(cmd); err != nil {
			return err
		}
		paused = append(paused, nodeName)
	}
	time.Sleep(duration)
	for _, nodeName := range paused {
		_, _ = fmt.Fprintf(GinkgoWriter, "  Unpausing Kind node %s\n", nodeName)
		cmd := exec.Command("docker", "unpause", nodeName)
		if _, err := utils.Run(cmd); err != nil {
			return err
		}
	}
	return nil
}

func scaleShards(ctx *ChaosContext) error {
	newShards := ctx.MinShards + ctx.Rand.Intn(ctx.MaxShards-ctx.MinShards+1)
	if newShards == ctx.Shards {
		// Ensure we actually change something
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

func deleteRecreateCluster(ctx *ChaosContext) error {
	// Capture the current spec before deleting
	cmd := exec.Command("kubectl", "get", "valkeycluster", ctx.ClusterName,
		"-n", ctx.Namespace, "-o", "jsonpath={.spec}")
	spec, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to get ValkeyCluster spec: %w", err)
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "  Deleting ValkeyCluster %s and waiting for removal\n", ctx.ClusterName)
	cmd = exec.Command("kubectl", "delete", "valkeycluster", ctx.ClusterName,
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
	if output, err := utils.GetPodsWide(clusterName, namespace); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "  PODS %s:\n%s\n", label, output)
	}
	if output, err := utils.GetClusterNodes(clusterName, namespace); err == nil {
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

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

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

func envOneOfOrInt(key, defaultVal string, valid []string) string {
	v := envOrDefault(key, defaultVal)
	for _, opt := range valid {
		if v == opt {
			return v
		}
	}
	if _, err := strconv.Atoi(v); err == nil {
		return v
	}
	Fail(fmt.Sprintf("%s=%q is invalid, must be one of %v or an integer", key, v, valid))
	return ""
}

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
	requested := make(map[string]bool)
	for _, name := range strings.Split(filter, ",") {
		requested[strings.TrimSpace(name)] = true
	}
	var result []Scenario
	for _, s := range all {
		if requested[s.Name] {
			result = append(result, s)
			delete(requested, s.Name)
		}
	}
	for name := range requested {
		Fail(fmt.Sprintf("CHAOS_SCENARIOS contains unknown scenario: %q", name))
	}
	return result
}

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
