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

package utils

import (
	"fmt"
	"math/rand"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DeletePod deletes a pod by name in the given namespace.
func DeletePod(name, namespace string) error {
	cmd := exec.Command("kubectl", "delete", "pod", name, "-n", namespace, "--grace-period=0", "--force")
	_, err := Run(cmd)
	return err
}

// GetPodNameByLabels returns a Ready pod name matching the given labels,
// falling back to any matching pod if none are Ready.
func GetPodNameByLabels(namespace string, labels map[string]string) (string, error) {
	selector := labelsToSelector(labels)
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"-l", selector,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[0].ready}{"\n"}{end}`)
	output, err := Run(cmd)
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

// DeleteWorkload deletes a StatefulSet or Deployment by name.
func DeleteWorkload(name, namespace, kind string) error {
	cmd := exec.Command("kubectl", "delete", strings.ToLower(kind), name, "-n", namespace, "--wait=false")
	_, err := Run(cmd)
	return err
}

// GetClusterNodes returns the CLUSTER NODES output from any pod in the cluster.
func GetClusterNodes(clusterName, namespace string) (string, error) {
	anyPod, err := GetPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return "", err
	}
	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"valkey-cli", "CLUSTER", "NODES")
	return Run(cmd)
}

func GetPodsWide(clusterName, namespace string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace, "-l",
		fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "wide", "--no-headers")
	return Run(cmd)
}

// GetShardPrimaryPod queries CLUSTER NODES to find the actual primary pod for a shard.
func GetShardPrimaryPod(clusterName, namespace string, shardIndex int) (string, error) {
	// Get any pod from the cluster to query CLUSTER NODES
	anyPod, err := GetPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get any pod for cluster: %w", err)
	}

	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"valkey-cli", "CLUSTER", "NODES")
	output, err := Run(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to run CLUSTER NODES: %w", err)
	}

	// Parse CLUSTER NODES output to find primaries with slots
	primaries := parsePrimariesFromClusterNodes(output)
	if shardIndex >= len(primaries) {
		return "", fmt.Errorf("shard index %d out of range (found %d primaries)", shardIndex, len(primaries))
	}

	ip := primaries[shardIndex]
	return GetPodByIP(namespace, ip)
}

// GetShardReplicaPod queries CLUSTER NODES to find a replica pod for a shard.
func GetShardReplicaPod(clusterName, namespace string, shardIndex int) (string, error) {
	anyPod, err := GetPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get any pod for cluster: %w", err)
	}

	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"valkey-cli", "CLUSTER", "NODES")
	output, err := Run(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to run CLUSTER NODES: %w", err)
	}

	primaries := parsePrimariesFromClusterNodes(output)
	if shardIndex >= len(primaries) {
		return "", fmt.Errorf("shard index %d out of range", shardIndex)
	}

	// Find the primary's node ID, then find a replica of that primary
	primaryIP := primaries[shardIndex]
	replicaIP, err := findReplicaOfPrimary(output, primaryIP)
	if err != nil {
		return "", err
	}

	return GetPodByIP(namespace, replicaIP)
}

// GetPodByIP finds a pod name by its IP address.
func GetPodByIP(namespace, ip string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"--field-selector", fmt.Sprintf("status.podIP=%s", ip),
		"-o", "jsonpath={.items[0].metadata.name}")
	output, err := Run(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to find pod with IP %s: %w", ip, err)
	}
	name := strings.TrimSpace(output)
	if name == "" {
		return "", fmt.Errorf("no pod found with IP %s", ip)
	}
	return name, nil
}

// GetWorkloadForPod returns the owning workload name and kind for a pod.
func GetWorkloadForPod(podName, namespace, workloadType string) (string, error) {
	// Get the pod's owner reference
	jsonpath := "{.metadata.ownerReferences[0].name}"
	if strings.EqualFold(workloadType, "statefulset") {
		// For StatefulSet, pod owner is the StatefulSet directly
		cmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace,
			"-o", "jsonpath="+jsonpath)
		output, err := Run(cmd)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(output), nil
	}
	// For Deployment, pod owner is a ReplicaSet; get the RS owner
	cmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace,
		"-o", "jsonpath="+jsonpath)
	rsName, err := Run(cmd)
	if err != nil {
		return "", err
	}
	cmd = exec.Command("kubectl", "get", "replicaset", strings.TrimSpace(rsName), "-n", namespace,
		"-o", "jsonpath="+jsonpath)
	output, err := Run(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// VerifyClusterHealth checks that all pods report cluster_state:ok,
// the topology has no stale nodes, correct node count, and no shard merges.
func VerifyClusterHealth(clusterName, namespace string, shards, replicas int) error {
	expectedNodes := shards * (1 + replicas)
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := Run(cmd)
	if err != nil {
		return err
	}

	pods := GetNonEmptyLines(output)
	if len(pods) != expectedNodes {
		return fmt.Errorf("expected %d pods, got %d", expectedNodes, len(pods))
	}

	for _, pod := range pods {
		cmd = exec.Command("kubectl", "exec", pod, "-n", namespace, "-c", "server", "--",
			"valkey-cli", "CLUSTER", "INFO")
		info, err := Run(cmd)
		if err != nil {
			return fmt.Errorf("CLUSTER INFO failed on %s: %w", pod, err)
		}
		if !strings.Contains(info, "cluster_state:ok") {
			return fmt.Errorf("pod %s reports cluster_state not ok: %s", pod, info)
		}

		cmd = exec.Command("kubectl", "exec", pod, "-n", namespace, "-c", "server", "--",
			"valkey-cli", "CLUSTER", "NODES")
		nodes, err := Run(cmd)
		if err != nil {
			return fmt.Errorf("CLUSTER NODES failed on %s: %w", pod, err)
		}
		if err := verifyClusterNodesOutput(nodes, shards, replicas, pod); err != nil {
			return err
		}
	}
	return nil
}

func verifyClusterNodesOutput(output string, shards, replicas int, pod string) error {
	expectedNodes := shards * (1 + replicas)

	var healthy int
	replicasOf := map[string]int{} // primary node ID to replica count
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

// VerifyK8sResources verifies the correct number of pods, ValkeyNodes, and workloads exist.
func VerifyK8sResources(clusterName, namespace, workloadType string, shards, replicas int) error {
	expectedTotal := shards * (1 + replicas)

	// Check pods
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"--field-selector", "status.phase=Running",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := Run(cmd)
	if err != nil {
		return err
	}
	pods := GetNonEmptyLines(output)
	if len(pods) != expectedTotal {
		return fmt.Errorf("expected %d Running pods, got %d", expectedTotal, len(pods))
	}

	// Check ValkeyNodes
	cmd = exec.Command("kubectl", "get", "valkeynodes", "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err = Run(cmd)
	if err != nil {
		return err
	}
	nodes := GetNonEmptyLines(output)
	if len(nodes) != expectedTotal {
		return fmt.Errorf("expected %d ValkeyNodes, got %d", expectedTotal, len(nodes))
	}

	// Check workloads
	kind := "statefulsets"
	if strings.EqualFold(workloadType, "deployment") {
		kind = "deployments"
	}
	cmd = exec.Command("kubectl", "get", kind, "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err = Run(cmd)
	if err != nil {
		return err
	}
	workloads := GetNonEmptyLines(output)
	if len(workloads) != expectedTotal {
		return fmt.Errorf("expected %d %s, got %d", expectedTotal, kind, len(workloads))
	}

	return nil
}

// FlushAll runs FLUSHALL on every primary in the cluster.
func FlushAll(clusterName, namespace string) error {
	anyPod, err := GetPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return err
	}
	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"valkey-cli", "CLUSTER", "NODES")
	output, err := Run(cmd)
	if err != nil {
		return err
	}
	for _, ip := range parsePrimariesFromClusterNodes(output) {
		pod, err := GetPodByIP(namespace, ip)
		if err != nil {
			return err
		}
		cmd = exec.Command("kubectl", "exec", pod, "-n", namespace, "-c", "server", "--",
			"valkey-cli", "FLUSHALL")
		if _, err := Run(cmd); err != nil {
			return fmt.Errorf("FLUSHALL failed on %s: %w", pod, err)
		}
	}
	return nil
}

// VerifyTestData checks that the total key count across all primaries matches expected.
func VerifyTestData(clusterName, namespace string, seededKeys int) error {
	totalKeys, perShard, err := GetTotalKeyCount(clusterName, namespace)
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

// GetTotalKeyCount sums the key count from INFO keyspace across all primaries.
// Returns total keys and per-shard key counts.
func GetTotalKeyCount(clusterName, namespace string) (int, map[string]int, error) {
	anyPod, err := GetPodNameByLabels(namespace, map[string]string{
		"valkey.io/cluster": clusterName,
	})
	if err != nil {
		return 0, nil, err
	}

	cmd := exec.Command("kubectl", "exec", anyPod, "-n", namespace, "-c", "server", "--",
		"valkey-cli", "CLUSTER", "NODES")
	output, err := Run(cmd)
	if err != nil {
		return 0, nil, err
	}

	primaryIPs := parsePrimariesFromClusterNodes(output)
	total := 0
	perShard := make(map[string]int, len(primaryIPs))
	for _, ip := range primaryIPs {
		pod, err := GetPodByIP(namespace, ip)
		if err != nil {
			return 0, nil, err
		}
		cmd = exec.Command("kubectl", "exec", pod, "-n", namespace, "-c", "server", "--",
			"valkey-cli", "INFO", "keyspace")
		info, err := Run(cmd)
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
		// fields[1] is ip:port@cport
		ip, _, _ := strings.Cut(fields[1], ":")
		// Slot info starts at field 8
		slotStart, _ := strconv.Atoi(strings.SplitN(fields[8], "-", 2)[0])
		primaries = append(primaries, primaryInfo{ip: ip, slotStart: slotStart})
	}

	// Sort by slot start to get consistent shard ordering
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
	// First find the node ID of the primary
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

	// Find a replica that references this primary's node ID
	for line := range strings.SplitSeq(clusterNodesOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if !strings.Contains(fields[2], "slave") {
			continue
		}
		// fields[3] is the master node ID this replica follows
		if fields[3] == primaryNodeID {
			ip, _, _ := strings.Cut(fields[1], ":")
			return ip, nil
		}
	}
	return "", fmt.Errorf("no replica found for primary %s (nodeID=%s)", primaryIP, primaryNodeID)
}

func labelsToSelector(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, ",")
}

// GetPodNodeName returns the node name where a pod is running.
func GetPodNodeName(podName, namespace string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace,
		"-o", "jsonpath={.spec.nodeName}")
	output, err := Run(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// PartitionNode adds iptables DROP rules to isolate a node from the cluster network.
func PartitionNode(nodeName string) error {
	// In Kind, nodes are docker containers. We exec into the node container to add iptables rules.
	// Block all network traffic to simulate a fully unreachable node.
	// docker exec still works as it uses the Docker daemon socket, not the container's network.
	rules := [][]string{
		{"iptables", "-A", "INPUT", "-j", "DROP"},
		{"iptables", "-A", "OUTPUT", "-j", "DROP"},
	}
	for _, rule := range rules {
		cmd := exec.Command("docker", append([]string{"exec", nodeName}, rule...)...)
		if _, err := Run(cmd); err != nil {
			return fmt.Errorf("failed to partition node %s: %w", nodeName, err)
		}
	}
	return nil
}

// HealNode removes iptables DROP rules to restore network connectivity.
func HealNode(nodeName string) error {
	cmd := exec.Command("docker", "exec", nodeName, "iptables", "-F")
	_, err := Run(cmd)
	return err
}

// PauseContainer pauses the valkey container in a pod using ctr on the Kind node.
func PauseContainer(podName, namespace string) error {
	containerID, err := GetContainerID(podName, namespace)
	if err != nil {
		return err
	}
	nodeName, err := GetPodNodeName(podName, namespace)
	if err != nil {
		return err
	}
	cmd := exec.Command("docker", "exec", nodeName, "ctr", "-n", "k8s.io", "task", "pause", containerID)
	_, err = Run(cmd)
	return err
}

// UnpauseContainer unpauses a previously paused container.
func UnpauseContainer(podName, namespace string) error {
	containerID, err := GetContainerID(podName, namespace)
	if err != nil {
		return err
	}
	nodeName, err := GetPodNodeName(podName, namespace)
	if err != nil {
		return err
	}
	cmd := exec.Command("docker", "exec", nodeName, "ctr", "-n", "k8s.io", "task", "resume", containerID)
	_, err = Run(cmd)
	return err
}

// GetContainerID returns the docker container ID for the server container in a pod.
func GetContainerID(podName, namespace string) (string, error) {
	cmd := exec.Command("kubectl", "get", "pod", podName, "-n", namespace,
		"-o", "jsonpath={.status.containerStatuses[?(@.name=='server')].containerID}")
	output, err := Run(cmd)
	if err != nil {
		return "", err
	}
	// containerID format: containerd://abc123 or docker://abc123
	id := strings.TrimSpace(output)
	if idx := strings.Index(id, "://"); idx >= 0 {
		id = id[idx+3:]
	}
	return id, nil
}

// GetWorkerNodes returns the names of all worker nodes in the cluster.
func GetWorkerNodes() []string {
	cmd := exec.Command("kubectl", "get", "nodes",
		"--selector=!node-role.kubernetes.io/control-plane",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := Run(cmd)
	if err != nil {
		return nil
	}
	return GetNonEmptyLines(output)
}

// ThrottleNodes applies a CPU limit to the given Docker containers.
func ThrottleNodes(nodes []string, cpus float64) []string {
	var throttled []string
	for _, node := range nodes {
		cmd := exec.Command("docker", "update", "--cpus", fmt.Sprintf("%.2f", cpus), node)
		if _, err := Run(cmd); err != nil {
			continue
		}
		throttled = append(throttled, node)
	}
	return throttled
}

var hostCPUs string

func getHostCPUs() string {
	if hostCPUs == "" {
		cmd := exec.Command("docker", "info", "--format", "{{.NCPU}}")
		output, err := Run(cmd)
		if err == nil {
			hostCPUs = strings.TrimSpace(output)
		}
	}
	return hostCPUs
}

// UnthrottleNodes removes CPU limits from the given Docker containers.
// docker update --cpus 0 is a no-op; we must set cpus to the host max.
func UnthrottleNodes(nodes []string) {
	cpus := getHostCPUs()
	for _, node := range nodes {
		cmd := exec.Command("docker", "update", "--cpus", cpus, node)
		_, _ = Run(cmd)
	}
}

// ThrottleRandomWorkerNodes picks a random subset of the given nodes and applies a random CPU limit per node.
func ThrottleRandomWorkerNodes(rnd *rand.Rand, nodes []string, cpuMin, cpuMax float64) []string {
	if len(nodes) == 0 {
		return nil
	}
	count := 1 + rnd.Intn(len(nodes))
	perm := rnd.Perm(len(nodes))
	var throttled []string
	for i := 0; i < count; i++ {
		cpus := cpuMin + rnd.Float64()*(cpuMax-cpuMin)
		if result := ThrottleNodes([]string{nodes[perm[i]]}, cpus); len(result) > 0 {
			throttled = append(throttled, result...)
		}
	}
	return throttled
}

const backgroundClientPod = "chaos-background-client"

// StartBackgroundClient deploys a pod running a custom Go client that seeds
// all keys and then continuously overwrites them, keeping replication offsets
// active on all shards. Waits for seeding to complete before returning.
// Returns the number of keys seeded.
func StartBackgroundClient(clusterName, namespace string, numKeys, dataSize, rps int) (int, error) {
	// Delete any leftover client pod
	StopBackgroundClient(namespace)

	svcHost := fmt.Sprintf("valkey-%s.%s.svc.cluster.local:6379", clusterName, namespace)
	cmd := exec.Command("kubectl", "run", backgroundClientPod,
		"-n", namespace,
		"--image=chaos-client:v0.0.1",
		"--restart=Always",
		"--image-pull-policy=Never",
		"--env=VALKEY_ADDR="+svcHost,
		"--env=NUM_KEYS="+strconv.Itoa(numKeys),
		"--env=DATA_SIZE="+strconv.Itoa(dataSize),
		"--env=RPS="+strconv.Itoa(rps),
	)
	if _, err := Run(cmd); err != nil {
		return 0, err
	}

	// Wait for "SEEDED N" in logs
	var seeded int
	for attempts := 0; attempts < 240; attempts++ {
		time.Sleep(1 * time.Second)
		cmd = exec.Command("kubectl", "logs", backgroundClientPod, "-n", namespace)
		output, err := Run(cmd)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(output, "\n") {
			if idx := strings.Index(line, "SEEDED "); idx >= 0 {
				if n, err := fmt.Sscanf(line[idx:], "SEEDED %d", &seeded); n == 1 && err == nil {
					return seeded, nil
				}
			}
		}
	}
	// Print pod logs on timeout to help debug
	cmd = exec.Command("kubectl", "logs", backgroundClientPod, "-n", namespace, "--tail=50")
	if output, err := Run(cmd); err == nil {
		fmt.Printf("  Background client logs at timeout:\n%s\n", output)
	}
	cmd = exec.Command("kubectl", "logs", backgroundClientPod, "-n", namespace, "--previous", "--tail=50")
	if output, err := Run(cmd); err == nil && output != "" {
		fmt.Printf("  Background client previous logs:\n%s\n", output)
	}
	return 0, fmt.Errorf("background client did not finish seeding within 240s")
}

// StopBackgroundClient prints stats and deletes the background client pod.
func StopBackgroundClient(namespace string) {
	cmd := exec.Command("kubectl", "logs", backgroundClientPod, "-n", namespace, "--tail=20")
	if output, err := Run(cmd); err == nil && output != "" {
		fmt.Printf("  Background client output:\n%s\n", output)
	}
	cmd = exec.Command("kubectl", "delete", "pod", backgroundClientPod,
		"-n", namespace, "--ignore-not-found=true", "--grace-period=0", "--force")
	_, _ = Run(cmd)
}

// VerifyConfigHashConsistency checks that all ValkeyNodes in the cluster have
// the same spec.serverConfigHash. A mismatch means the rolling update hasn't
// finished — some nodes still have the old config.
func VerifyConfigHashConsistency(clusterName, namespace string) error {
	cmd := exec.Command("kubectl", "get", "valkeynodes", "-n", namespace,
		"-l", fmt.Sprintf("valkey.io/cluster=%s", clusterName),
		"-o", "jsonpath={range .items[*]}{.spec.serverConfigHash}{\"\\n\"}{end}")
	output, err := Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to get config hashes: %w", err)
	}
	var expected string
	for _, hash := range strings.Split(strings.TrimSpace(output), "\n") {
		if hash == "" {
			continue
		}
		if expected == "" {
			expected = hash
		} else if hash != expected {
			return fmt.Errorf("config hash mismatch: expected %s, got %s", expected, hash)
		}
	}
	return nil
}
