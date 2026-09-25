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

import (
	"fmt"
	"math/rand"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/valkey-io/valkey-operator/test/utils"
)

// DeletePod deletes a pod by name in the given namespace.
func deletePodByName(name, namespace string) error {
	cmd := exec.Command("kubectl", "delete", "pod", name, "-n", namespace, "--grace-period=0", "--force")
	_, err := utils.Run(cmd)
	return err
}

// getPodNameByLabels returns a Ready pod name matching the given labels,
// falling back to any matching pod if none are Ready.
func getPodNameByLabels(namespace string, labels map[string]string) (string, error) {
	selector := labelsToSelector(labels)
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"-l", selector,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[0].ready}{"\n"}{end}`)
	output, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}
	var fallback string
	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
		if len(parts) == 2 && parts[1] == "true" {
			return parts[0], nil
		}
		if fallback == "" {
			fallback = parts[0]
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no pods found with labels %v", labels)
}

// deleteWorkload deletes a StatefulSet or Deployment by name.
func deleteWorkload(name, namespace, kind string) error {
	cmd := exec.Command("kubectl", "delete", strings.ToLower(kind), name, "-n", namespace, "--wait=false")
	_, err := utils.Run(cmd)
	return err
}

// getClusterNodesOutput returns the CLUSTER NODES output from any pod in the cluster.
func getClusterNodesOutput(clusterName, namespace string) (string, error) {
	anyPod, err := getPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return "", err
	}
	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli CLUSTER NODES")
	return utils.Run(cmd)
}

// getPodsWide returns kubectl get pods -o wide output for the cluster's pods.
func getPodsWide(clusterName, namespace string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace, "-l",
		fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "wide", "--no-headers")
	return utils.Run(cmd)
}

// getShardPrimaryPod queries CLUSTER NODES to find the actual primary pod for a shard.
func getShardPrimaryPod(clusterName, namespace string, shardIndex int) (string, error) {
	anyPod, err := getPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get any pod for cluster: %w", err)
	}

	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli CLUSTER NODES")
	output, err := utils.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to run CLUSTER NODES: %w", err)
	}

	primaries := parsePrimariesFromClusterNodes(output)
	if shardIndex >= len(primaries) {
		return "", fmt.Errorf("shard index %d out of range (found %d primaries)", shardIndex, len(primaries))
	}

	ip := primaries[shardIndex]
	return getPodByIP(namespace, ip)
}

// getShardReplicaPod queries CLUSTER NODES to find a replica pod for a shard.
func getShardReplicaPod(clusterName, namespace string, shardIndex int) (string, error) {
	anyPod, err := getPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get any pod for cluster: %w", err)
	}

	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli CLUSTER NODES")
	output, err := utils.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to run CLUSTER NODES: %w", err)
	}

	primaries := parsePrimariesFromClusterNodes(output)
	if shardIndex >= len(primaries) {
		return "", fmt.Errorf("shard index %d out of range", shardIndex)
	}

	primaryIP := primaries[shardIndex]
	replicaIP, err := findReplicaOfPrimary(output, primaryIP)
	if err != nil {
		return "", err
	}

	return getPodByIP(namespace, replicaIP)
}

// getPodByIP finds a pod name by its IP address.
func getPodByIP(namespace, ip string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"--field-selector", fmt.Sprintf("status.podIP=%s", ip),
		"-o", "jsonpath={.items[0].metadata.name}")
	output, err := utils.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to find pod with IP %s: %w", ip, err)
	}
	name := strings.TrimSpace(output)
	if name == "" {
		return "", fmt.Errorf("no pod found with IP %s", ip)
	}
	return name, nil
}

// getWorkloadForPod returns the owning workload name for a pod.
func getWorkloadForPod(podName, namespace, workloadType string) (string, error) {
	jsonpath := "{.metadata.ownerReferences[0].name}"
	if strings.EqualFold(workloadType, "statefulset") {
		cmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace,
			"-o", "jsonpath="+jsonpath)
		output, err := utils.Run(cmd)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(output), nil
	}
	cmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace,
		"-o", "jsonpath="+jsonpath)
	rsName, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}
	cmd = exec.Command("kubectl", "get", "replicaset", strings.TrimSpace(rsName), "-n", namespace,
		"-o", "jsonpath="+jsonpath)
	output, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// verifyClusterHealth checks that all pods report cluster_state:ok,
// the topology has no stale nodes, correct node count, and no shard merges.
func verifyClusterHealth(clusterName, namespace string, shards, replicas int) error {
	expectedNodes := shards * (1 + replicas)
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := utils.Run(cmd)
	if err != nil {
		return err
	}

	pods := utils.GetNonEmptyLines(output)
	if len(pods) != expectedNodes {
		return fmt.Errorf("expected %d pods, got %d", expectedNodes, len(pods))
	}

	for _, pod := range pods {
		cmd = exec.Command("kubectl", "exec", pod, "-n", namespace, "-c", "server", "--",
			"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli CLUSTER INFO")
		info, err := utils.Run(cmd)
		if err != nil {
			return fmt.Errorf("CLUSTER INFO failed on %s: %w", pod, err)
		}
		if !strings.Contains(info, "cluster_state:ok") {
			return fmt.Errorf("pod %s reports cluster_state not ok: %s", pod, info)
		}

		cmd = exec.Command("kubectl", "exec", pod, "-n", namespace, "-c", "server", "--",
			"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli CLUSTER NODES")
		nodes, err := utils.Run(cmd)
		if err != nil {
			return fmt.Errorf("CLUSTER NODES failed on %s: %w", pod, err)
		}
		if strings.HasPrefix(nodes, "ERR ") || strings.HasPrefix(nodes, "AUTH ") {
			return fmt.Errorf("CLUSTER NODES returned an error on %s: %s", pod, nodes)
		}
		if err := verifyClusterNodesOutput(nodes, shards, replicas, pod); err != nil {
			return err
		}
	}
	return nil
}

// verifyClusterNodesOutput checks a single pod's CLUSTER NODES output: no node
// carries a fail or noaddr flag, the node count matches the expected topology,
// and every primary has the expected number of replicas.
func verifyClusterNodesOutput(output string, shards, replicas int, pod string) error {
	expectedNodes := shards * (1 + replicas)

	var healthy int
	replicasOf := map[string]int{}
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		flags := fields[2]
		if strings.Contains(flags, "fail") || strings.Contains(flags, "noaddr") {
			return fmt.Errorf("[%s] stale node in topology: %s (flags=%s)\nCLUSTER NODES:\n%s", pod, fields[1], flags, output)
		}
		healthy++
		if strings.Contains(flags, "slave") {
			primaryId := fields[3]
			replicasOf[primaryId]++
		}
	}
	if healthy != expectedNodes {
		return fmt.Errorf("[%s] expected %d nodes in topology, got %d\nCLUSTER NODES:\n%s",
			pod, expectedNodes, healthy, output)
	}
	for primaryId, count := range replicasOf {
		if count != replicas {
			return fmt.Errorf("[%s] primary %s has %d replicas (expected %d)\nCLUSTER NODES:\n%s",
				pod, primaryId, count, replicas, output)
		}
	}
	return nil
}

// verifyK8sResources verifies the correct number of pods, ValkeyNodes, and workloads exist.
func verifyK8sResources(clusterName, namespace, workloadType string, shards, replicas int) error {
	expectedTotal := shards * (1 + replicas)

	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"--field-selector", "status.phase=Running",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := utils.Run(cmd)
	if err != nil {
		return err
	}
	pods := utils.GetNonEmptyLines(output)
	if len(pods) != expectedTotal {
		return fmt.Errorf("expected %d Running pods, got %d", expectedTotal, len(pods))
	}

	cmd = exec.Command("kubectl", "get", "valkeynodes", "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err = utils.Run(cmd)
	if err != nil {
		return err
	}
	nodes := utils.GetNonEmptyLines(output)
	if len(nodes) != expectedTotal {
		return fmt.Errorf("expected %d ValkeyNodes, got %d", expectedTotal, len(nodes))
	}

	kind := "statefulsets"
	if strings.EqualFold(workloadType, "deployment") {
		kind = "deployments"
	}
	cmd = exec.Command("kubectl", "get", kind, "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err = utils.Run(cmd)
	if err != nil {
		return err
	}
	workloads := utils.GetNonEmptyLines(output)
	if len(workloads) != expectedTotal {
		return fmt.Errorf("expected %d %s, got %d", expectedTotal, kind, len(workloads))
	}

	return nil
}

// flushAll runs FLUSHALL on every primary in the cluster.
func flushAll(clusterName, namespace string) error {
	anyPod, err := getPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return err
	}
	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli CLUSTER NODES")
	output, err := utils.Run(cmd)
	if err != nil {
		return err
	}
	for _, ip := range parsePrimariesFromClusterNodes(output) {
		pod, err := getPodByIP(namespace, ip)
		if err != nil {
			return err
		}
		cmd = exec.Command("kubectl", "exec", pod, "-n", namespace, "-c", "server", "--",
			"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli FLUSHALL")
		if _, err := utils.Run(cmd); err != nil {
			return fmt.Errorf("FLUSHALL failed on %s: %w", pod, err)
		}
	}
	return nil
}

// verifyTestData checks that the total key count across all primaries matches expected.
func verifyTestData(clusterName, namespace string, seededKeys int) error {
	totalKeys, perShard, err := getTotalKeyCount(clusterName, namespace)
	if err != nil {
		return fmt.Errorf("failed to get keyspace info: %w", err)
	}
	if totalKeys != seededKeys {
		return fmt.Errorf(
			"keyspace count mismatch: expected %d keys, INFO keyspace reports %d (per-shard: %v)",
			seededKeys, totalKeys, perShard)
	}
	return nil
}

// getTotalKeyCount sums the key count from INFO keyspace across all primaries.
func getTotalKeyCount(clusterName, namespace string) (int, map[string]int, error) {
	anyPod, err := getPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return 0, nil, err
	}

	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli CLUSTER NODES")
	output, err := utils.Run(cmd)
	if err != nil {
		return 0, nil, err
	}

	primaryIPs := parsePrimariesFromClusterNodes(output)
	total := 0
	perShard := make(map[string]int, len(primaryIPs))
	for _, ip := range primaryIPs {
		pod, err := getPodByIP(namespace, ip)
		if err != nil {
			return 0, nil, err
		}
		cmd = exec.Command("kubectl", "exec", pod, "-n", namespace, "-c", "server", "--",
			"sh", "-c", "unset VALKEYCLI_AUTH REDISCLI_AUTH; valkey-cli INFO keyspace")
		info, err := utils.Run(cmd)
		if err != nil {
			return 0, nil, fmt.Errorf("INFO keyspace failed on %s: %w", pod, err)
		}
		keys := parseKeysFromInfoKeyspace(info)
		perShard[pod] = keys
		total += keys
	}
	return total, perShard, nil
}

// parseKeysFromInfoKeyspace parses INFO keyspace output and sums all db keys.
// Format: db0:keys=123,expires=0,avg_ttl=0
func parseKeysFromInfoKeyspace(info string) int {
	total := 0
	for line := range strings.SplitSeq(info, "\n") {
		if !strings.HasPrefix(line, "db") {
			continue
		}
		_, after, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		for field := range strings.SplitSeq(after, ",") {
			if v, ok := strings.CutPrefix(field, "keys="); ok {
				n, _ := strconv.Atoi(v)
				total += n
			}
		}
	}
	return total
}

// parsePrimariesFromClusterNodes parses CLUSTER NODES output and returns primary IPs
// sorted by their slot ranges (shard 0 = lowest slots, etc.)
func parsePrimariesFromClusterNodes(output string) []string {
	type primaryInfo struct {
		ip        string
		slotStart int
	}
	var primaries []primaryInfo

	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		if !strings.Contains(fields[2], "master") {
			continue
		}
		ip, _, _ := strings.Cut(fields[1], ":")
		slotStart, _ := strconv.Atoi(strings.SplitN(fields[8], "-", 2)[0])
		primaries = append(primaries, primaryInfo{ip: ip, slotStart: slotStart})
	}

	for i := 0; i < len(primaries); i++ {
		for j := i + 1; j < len(primaries); j++ {
			if primaries[j].slotStart < primaries[i].slotStart {
				primaries[i], primaries[j] = primaries[j], primaries[i]
			}
		}
	}

	result := make([]string, len(primaries))
	for i, p := range primaries {
		result[i] = p.ip
	}
	return result
}

// findReplicaOfPrimary finds a replica IP that replicates the primary at the given IP.
func findReplicaOfPrimary(clusterNodesOutput, primaryIP string) (string, error) {
	var primaryNodeID string
	for line := range strings.SplitSeq(clusterNodesOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		ip, _, _ := strings.Cut(fields[1], ":")
		if ip == primaryIP && strings.Contains(fields[2], "master") {
			primaryNodeID = fields[0]
			break
		}
	}
	if primaryNodeID == "" {
		return "", fmt.Errorf("could not find primary node ID for IP %s", primaryIP)
	}

	for line := range strings.SplitSeq(clusterNodesOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if !strings.Contains(fields[2], "slave") {
			continue
		}
		if fields[3] == primaryNodeID {
			ip, _, _ := strings.Cut(fields[1], ":")
			return ip, nil
		}
	}
	return "", fmt.Errorf("no replica found for primary %s (nodeID=%s)", primaryIP, primaryNodeID)
}

// labelsToSelector renders a label map as a kubectl -l selector.
func labelsToSelector(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, ",")
}

// getPodNodeName returns the node name where a pod is running.
func getPodNodeName(podName, namespace string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace,
		"-o", "jsonpath={.spec.nodeName}")
	output, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// partitionRules are the iptables rules partitionWorkerNode adds and healWorkerNode removes.
var partitionRules = [][]string{
	{"INPUT", "-j", "DROP"},
	{"OUTPUT", "-j", "DROP"},
}

// partitionWorkerNode adds iptables DROP rules to isolate a worker node from the
// cluster network. In Kind, worker nodes are docker containers. docker exec still
// works as it uses the Docker daemon socket, not the container's network.
func partitionWorkerNode(worker string) error {
	for _, rule := range partitionRules {
		args := append([]string{"exec", worker, "iptables", "-A"}, rule...)
		cmd := exec.Command("docker", args...)
		if _, err := utils.Run(cmd); err != nil {
			return fmt.Errorf("failed to partition worker node %s: %w", worker, err)
		}
	}
	return nil
}

// healWorkerNode removes the DROP rules added by partitionWorkerNode to restore
// network connectivity. Only those rules are deleted, leaving any other rules on
// the worker node (such as the CNI's) untouched. Rules that are not present are
// skipped, so this is safe to call on a node that was never partitioned. Removal
// continues past failures so one error cannot leave the node partitioned.
func healWorkerNode(worker string) error {
	var firstErr error
	for _, rule := range partitionRules {
		// iptables -C exits non-zero when the rule is absent, so only delete
		// rules we actually added.
		check := exec.Command("docker", append([]string{"exec", worker, "iptables", "-C"}, rule...)...)
		if _, err := utils.Run(check); err != nil {
			continue
		}
		args := append([]string{"exec", worker, "iptables", "-D"}, rule...)
		cmd := exec.Command("docker", args...)
		if _, err := utils.Run(cmd); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("failed to heal worker node %s: %w", worker, err)
		}
	}
	return firstErr
}

// unpauseWorkerNode resumes a paused worker node. Nodes that are not paused are
// skipped, so this is safe to call on a node that was never paused.
func unpauseWorkerNode(worker string) error {
	out, err := utils.Run(exec.Command("docker", "inspect", "-f", "{{.State.Paused}}", worker))
	if err != nil {
		return fmt.Errorf("failed to inspect worker node %s: %w", worker, err)
	}
	if strings.TrimSpace(out) != "true" {
		return nil
	}
	if _, err := utils.Run(exec.Command("docker", "unpause", worker)); err != nil {
		return fmt.Errorf("failed to unpause worker node %s: %w", worker, err)
	}
	return nil
}

// pauseContainer pauses the valkey container in a pod using ctr on the Kind node.
func pauseContainer(podName, namespace string) error {
	containerID, err := getContainerID(podName, namespace)
	if err != nil {
		return err
	}
	nodeName, err := getPodNodeName(podName, namespace)
	if err != nil {
		return err
	}
	cmd := exec.Command("docker", "exec", nodeName, "ctr", "-n", "k8s.io", "task", "pause", containerID)
	_, err = utils.Run(cmd)
	return err
}

// unpauseContainer unpauses a previously paused container.
func unpauseContainer(podName, namespace string) error {
	containerID, err := getContainerID(podName, namespace)
	if err != nil {
		return err
	}
	nodeName, err := getPodNodeName(podName, namespace)
	if err != nil {
		return err
	}
	cmd := exec.Command("docker", "exec", nodeName, "ctr", "-n", "k8s.io", "task", "resume", containerID)
	_, err = utils.Run(cmd)
	return err
}

// unpauseContainers unpauses the server container in every given pod, continuing
// past failures so a single error cannot leave a container paused. Returns the
// first error.
func unpauseContainers(pods []string, namespace string) error {
	var firstErr error
	for _, pod := range pods {
		fmt.Printf("  Unpausing container in pod: %s\n", pod)
		if err := unpauseContainer(pod, namespace); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// getContainerID returns the container ID for the server container in a pod.
// containerID format: containerd://abc123 or docker://abc123
func getContainerID(podName, namespace string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace,
		"-o", "jsonpath={.status.containerStatuses[?(@.name=='server')].containerID}")
	output, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(output)
	if idx := strings.Index(id, "://"); idx >= 0 {
		id = id[idx+3:]
	}
	return id, nil
}

// getWorkerNodes returns the names of all worker nodes in the cluster.
func getWorkerNodes() []string {
	cmd := exec.Command("kubectl", "get", "nodes",
		"--selector=!node-role.kubernetes.io/control-plane",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := utils.Run(cmd)
	if err != nil {
		return nil
	}
	return utils.GetNonEmptyLines(output)
}

// throttleWorkerNodes applies a CPU limit to the given worker nodes.
func throttleWorkerNodes(workers []string, cpus float64) []string {
	var throttled []string
	for _, worker := range workers {
		cmd := exec.Command("docker", "update", "--cpus", fmt.Sprintf("%.2f", cpus), worker)
		if _, err := utils.Run(cmd); err != nil {
			continue
		}
		throttled = append(throttled, worker)
	}
	return throttled
}

var hostCPUs string

// getHostCPUs returns the Docker host's CPU count, caching it after the first
// lookup. Used to restore a throttled worker node to its unthrottled limit.
func getHostCPUs() string {
	if hostCPUs == "" {
		cmd := exec.Command("docker", "info", "--format", "{{.NCPU}}")
		output, err := utils.Run(cmd)
		if err == nil {
			hostCPUs = strings.TrimSpace(output)
		}
	}
	return hostCPUs
}

// unthrottleWorkerNodes removes CPU limits from the given worker nodes.
// docker update --cpus 0 is a no-op; we must set cpus to the host max.
func unthrottleWorkerNodes(workers []string) {
	cpus := getHostCPUs()
	for _, worker := range workers {
		cmd := exec.Command("docker", "update", "--cpus", cpus, worker)
		_, _ = utils.Run(cmd)
	}
}

// throttleRandomWorkerNodes picks a random subset of the given worker nodes and
// applies a random CPU limit per worker node.
func throttleRandomWorkerNodes(rnd *rand.Rand, workers []string, cpuMin, cpuMax float64) []string {
	if len(workers) == 0 {
		return nil
	}
	count := 1 + rnd.Intn(len(workers))
	perm := rnd.Perm(len(workers))
	var throttled []string
	for i := 0; i < count; i++ {
		cpus := cpuMin + rnd.Float64()*(cpuMax-cpuMin)
		if result := throttleWorkerNodes([]string{workers[perm[i]]}, cpus); len(result) > 0 {
			throttled = append(throttled, result...)
		}
	}
	return throttled
}

const (
	// clientImage is the chaos client image built and loaded in BeforeSuite.
	clientImage         = "chaos-client:v0.0.1"
	backgroundClientPod = "chaos-background-client"
)

// startBackgroundClient deploys a pod running a custom Go client that seeds
// all keys and then continuously overwrites them. Waits for seeding to complete.
// Returns the number of keys seeded.
func startBackgroundClient(clusterName, namespace string, numKeys, dataSize, rps int) (int, error) {
	if numKeys < 1 {
		return 0, fmt.Errorf("numKeys must be >= 1, got %d", numKeys)
	}
	stopBackgroundClient(namespace)

	svcHost := fmt.Sprintf("valkey-%s.%s.svc.cluster.local:6379", clusterName, namespace)
	// Never restart: the client exits after seeding when rps is 0, and a seeding
	// failure is meant to be terminal. Restarting would hide the failure in a
	// previous container's log and re-seed behind the suite's back.
	cmd := exec.Command("kubectl", "run", backgroundClientPod,
		"-n", namespace,
		"--image="+clientImage,
		"--restart=Never",
		"--image-pull-policy=Never",
		"--env=VALKEY_ADDR="+svcHost,
		"--env=NUM_KEYS="+strconv.Itoa(numKeys),
		"--env=DATA_SIZE="+strconv.Itoa(dataSize),
		"--env=RPS="+strconv.Itoa(rps),
	)
	if _, err := utils.Run(cmd); err != nil {
		return 0, err
	}

	// Fail fast if the client gets stuck rather than waiting out the suite timeout.
	// Seeding is one unthrottled round-trip per key, measured well above 10k
	// keys/sec on Kind, so 1s per 1000 keys leaves room for a throttled worker.
	timeout := 60*time.Second + time.Duration(numKeys/1000)*time.Second
	deadline := time.Now().Add(timeout)
	var seeded int
	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
		cmd = exec.Command("kubectl", "logs", backgroundClientPod, "-n", namespace)
		output, err := utils.Run(cmd)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(output, "\n") {
			if idx := strings.Index(line, "SEEDED "); idx >= 0 {
				if n, err := fmt.Sscanf(line[idx:], "SEEDED %d", &seeded); n == 1 && err == nil {
					return seeded, nil
				}
			}
			// The client seeds every key or exits, so report its failure here
			// rather than waiting for the deadline.
			if idx := strings.Index(line, "SEED FAILED"); idx >= 0 {
				return 0, fmt.Errorf("background client failed to seed: %s", line[idx:])
			}
		}
	}
	cmd = exec.Command("kubectl", "logs", backgroundClientPod, "-n", namespace, "--tail=50")
	if output, err := utils.Run(cmd); err == nil {
		fmt.Printf("  Background client logs at timeout:\n%s\n", output)
	}
	return 0, fmt.Errorf("background client did not finish seeding within %s", timeout)
}

// stopBackgroundClient prints stats and deletes the background client pod.
func stopBackgroundClient(namespace string) {
	cmd := exec.Command("kubectl", "logs", backgroundClientPod, "-n", namespace, "--tail=20")
	if output, err := utils.Run(cmd); err == nil && output != "" {
		fmt.Printf("  Background client output:\n%s\n", output)
	}
	cmd = exec.Command("kubectl", "delete", "pod", backgroundClientPod,
		"-n", namespace, "--ignore-not-found=true", "--grace-period=0", "--force")
	_, _ = utils.Run(cmd)
}

// verifyClusterConverged checks that all ValkeyNodes have converged: no node is
// still waiting for a pod-template roll, and no node is stuck with ACLApplied=False.
func verifyClusterConverged(clusterName, namespace string) error {
	// No node should be left waiting for a pod-template roll to be authorized.
	// WorkloadRollPending=True is expected staging mid-roll, but once the cluster
	// has recovered every node must have completed or never needed its roll.
	cmd := exec.Command("kubectl", "get", "valkeynodes", "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="WorkloadRollPending")]}{.status}{end}{"\n"}{end}`)
	output, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to get WorkloadRollPending conditions: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if parts[1] == "True" {
			return fmt.Errorf("node %s still has WorkloadRollPending=True", parts[0])
		}
	}

	// No node should have ACLApplied=False (if the condition exists it must be True)
	cmd = exec.Command("kubectl", "get", "valkeynodes", "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="ACLApplied")]}{.status}{end}{"\n"}{end}`)
	output, err = utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to get ACLApplied conditions: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if parts[1] == "False" {
			return fmt.Errorf("node %s has ACLApplied=False", parts[0])
		}
	}

	return nil
}
