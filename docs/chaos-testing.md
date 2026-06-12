# Chaos Testing

The chaos test suite continuously injects faults into a running ValkeyCluster and verifies that the operator recovers the cluster to a healthy state without losing data.

## Prerequisites

- Kind
- Docker
- `kubectl` configured to the target cluster

## Running

```bash
make test-chaos
```

This creates a Kind cluster, builds and deploys the operator, then runs the chaos test loop until a failure occurs or it is interrupted.

To save the output to a log file (useful for post-mortem analysis):

```bash
make test-chaos 2>&1 | tee --ignore-interrupts chaos.log
```

The `--ignore-interrupts` flag ensures the log is fully written even if you Ctrl+C the test.

## How It Works

1. Creates a ValkeyCluster with the configured number of shards and replicas
2. Waits for the cluster to become Ready and healthy
3. Seeds test data across all shards
4. Loops indefinitely:
   - Picks a scenario (random or sequential)
   - Logs cluster state and per-shard key counts
   - Injects the fault
   - Waits for recovery (Ready state + cluster health)
   - Verifies data integrity (unless the scenario is expected to lose data)
5. On failure, collects controller logs, pod states, and CLUSTER NODES output

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `KIND_WORKERS` | `2` | Number of Kind worker nodes to create |
| `CHAOS_SHARDS` | `3` | Number of shards to create |
| `CHAOS_MIN_SHARDS` | value of `CHAOS_SHARDS` | Minimum shards for scale scenarios |
| `CHAOS_MAX_SHARDS` | `CHAOS_SHARDS + 3` | Maximum shards for scale scenarios |
| `CHAOS_REPLICAS` | `1` | Replicas per shard |
| `CHAOS_WORKLOAD_TYPE` | `StatefulSet` | `StatefulSet` or `Deployment` |
| `CHAOS_PERSISTENCE` | `false` | Enable persistence (requires StatefulSet) |
| `CHAOS_SCENARIOS` | all except disabled | Comma-separated list of scenarios to run |
| `CHAOS_SEED` | current time | Random seed for reproducibility |
| `CHAOS_MODE` | `random` | `random` or `sequential` scenario selection |
| `CHAOS_TARGET_SHARDS` | `random` | Shards to target: `random` (1 to N shards each iteration), `all`, or comma-separated indices (e.g. `0,2`) |
| `CHAOS_RECOVERY_TIMEOUT` | `15s × pods` (min 5m) | Max time to wait for cluster recovery |
| `CHAOS_TOLERATION_SECONDS` | `0` | Pod toleration seconds for not-ready/unreachable (0 = not set) |
| `CHAOS_NUM_KEYS` | `100000` | Number of keys to seed |
| `CHAOS_DATA_SIZE` | `3` | Size of each key's value in bytes |
| `CHAOS_CPU_PRESSURE` | `false` | Throttle Kind worker node CPUs each iteration |
| `CHAOS_CPU_MIN` | `0.3` | Minimum CPU limit when throttling |
| `CHAOS_CPU_MAX` | `1.0` | Maximum CPU limit when throttling |

## Scenarios

| Scenario | Description |
|----------|-------------|
| `delete-primary-pod` | Deletes the primary pod of targeted shards |
| `delete-replica-pod` | Deletes a replica pod of targeted shards |
| `delete-shard-pods` | Deletes all pods in targeted shards |
| `delete-primary-workload` | Deletes the primary's Deployment/StatefulSet |
| `delete-replica-workload` | Deletes a replica's Deployment/StatefulSet |
| `pause-primary-container` | Pauses the primary container |
| `pause-replica-container` | Pauses a replica container |
| `scale-shards` | Scales shards up or down randomly |
| `scale-replicas` | Scales replicas up or down randomly |
| `rolling-update` | Changes cluster config to trigger a rolling update |
| `delete-recreate-cluster` | Deletes and recreates the ValkeyCluster |
| `delete-controller-pod` | Kills the operator controller pod |
| `pause-worker-node` | Pauses the Kind worker node hosting the primary (disabled by default) |
| `network-partition-primary` | Isolates the primary from the cluster (disabled by default) |
| `network-partition-replica` | Isolates a replica from the cluster (disabled by default) |

Scenarios that target replicas are skipped when `CHAOS_REPLICAS=0`.

Scenarios marked "disabled by default" are excluded unless explicitly listed in `CHAOS_SCENARIOS`.
They require `CHAOS_TOLERATION_SECONDS` to be set for meaningful eviction testing.

### Compound Scenarios

Use the `+` separator in `CHAOS_SCENARIOS` to inject multiple faults back-to-back within a single iteration.
Each sub-scenario runs sequentially with a random 0-9s delay between them, testing that the operator handles overlapping faults mid-reconciliation.

```bash
# Scale shards while killing a primary during the operation
CHAOS_SCENARIOS=scale-shards+delete-primary-pod make test-chaos

# Multiple compounds, randomly selected each iteration
CHAOS_SCENARIOS=scale-shards+delete-primary-pod,scale-replicas+delete-replica-pod make test-chaos

# Triple fault: scale + kill primary + kill replica
CHAOS_SCENARIOS=scale-shards+delete-primary-pod+delete-replica-pod make test-chaos
```

Compound scenarios are always marked as potentially losing data; the test logs a warning showing how many keys were lost and re-seeds after each iteration.

## Reading the Output

Each iteration logs:

```
--- Iteration 5: scenario=rolling-update ---
  PODS before: ...
  CLUSTER NODES before: ...
  KEY COUNT before: ...

  STEP: Iteration 5: waiting for cluster recovery

  PODS after: ...
  CLUSTER NODES after: ...
  Iteration 5: PASSED
```

- **PODS before/after** — pod placement showing node, IP, and readiness.
- **CLUSTER NODES before/after** — full cluster topology showing roles, slots, and node IDs.
- **KEY COUNT before** — per-shard key distribution before fault injection. All shards should have keys. If a shard shows 0, data was already lost before this iteration.

On failure, the test collects extensive debug information:

- **Keyspace counts** — total and per-shard key counts at the time of failure
- **Pod listing** — all cluster pods with node placement and readiness
- **ValkeyCluster CR status** — full YAML of the ValkeyCluster resource
- **ValkeyNodes** — all ValkeyNode resources for the cluster
- **CLUSTER NODES** — topology output from every pod
- **Server logs** — last 100 lines from each pod's server container
- **Controller logs** — operator controller-manager logs and pod description
- **Kubernetes events** — events in the operator namespace

## Examples

```bash
# Run all default scenarios with Deployment workload type
CHAOS_WORKLOAD_TYPE=Deployment make test-chaos

# Stress test with CPU pressure on worker nodes
CHAOS_CPU_PRESSURE=true make test-chaos

# Test network partitions with pod evictions
CHAOS_SCENARIOS=network-partition-primary,pause-worker-node \
  CHAOS_TOLERATION_SECONDS=10 make test-chaos

# Large cluster with more workers
CHAOS_SHARDS=7 CHAOS_REPLICAS=2 KIND_WORKERS=3 make test-chaos

# Test scaling shards and replicas together
CHAOS_SHARDS=5 CHAOS_REPLICAS=2 CHAOS_SCENARIOS=scale-shards,scale-replicas make test-chaos

# Run scenarios in sequence instead of random
CHAOS_SCENARIOS=delete-primary-pod,scale-shards CHAOS_MODE=sequential make test-chaos

# Kill primaries on specific shards (triggers quorum loss + TAKEOVER)
CHAOS_SHARDS=3 CHAOS_REPLICAS=1 CHAOS_TARGET_SHARDS=0,1 \
  CHAOS_SCENARIOS=delete-primary-pod make test-chaos

# Delete all pods in all shards
CHAOS_TARGET_SHARDS=all CHAOS_SCENARIOS=delete-shard-pods make test-chaos
```

## Running Multiple Tests in Parallel

The chaos test uses a dedicated Kind cluster (`valkey-operator-test-chaos`) that is separate from the e2e test cluster.
To run two chaos tests simultaneously with different configurations, use separate Kind clusters and kubeconfigs:

```bash
# Terminal 1: default config
make test-chaos 2>&1 | tee --ignore-interrupts chaos-run1.log

# Terminal 2: different cluster name and kubeconfig to avoid conflicts
KIND_CLUSTER_CHAOS=chaos-2 KUBECONFIG=/tmp/chaos-2.conf \
  make test-chaos 2>&1 | tee --ignore-interrupts chaos-run2.log
```

Each run creates its own Kind cluster, so they don't interfere with each other.
