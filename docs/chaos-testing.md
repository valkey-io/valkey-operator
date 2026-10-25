# Chaos Testing

The chaos test suite continuously injects faults into a running ValkeyCluster and verifies that the operator recovers the cluster to a healthy state, preserving data for the scenarios that are expected to retain it.

## Prerequisites

- Kind
- Docker
- `kubectl` configured to the target cluster

## Running

```bash
make test-chaos
```

This creates a Kind cluster, builds and deploys the operator, then runs the chaos test loop until a failure occurs, it is interrupted, or the 24 hour test timeout is reached.

To save the output to a log file (useful for post-mortem analysis):

```bash
make test-chaos 2>&1 | tee -i chaos.log
```

The `-i` flag ensures the log is fully written even if you Ctrl+C the test.

## How It Works

1. Creates a ValkeyCluster with the configured number of shards and replicas
2. Waits for the cluster to become Ready and healthy
3. Seeds test data across all shards
4. Loops until a failure, an interrupt, or the 24 hour timeout:
   - Picks a scenario (random or sequential)
   - Logs cluster state and per-shard key counts
   - Injects the fault
   - Waits for recovery (Ready state, K8s resources, cluster health, and convergence)
   - Checks that the operator container has not restarted
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
| `CHAOS_MAX_REPLICAS` | `CHAOS_REPLICAS + 2` | Maximum replicas for scale scenarios |
| `CHAOS_NODE_TIMEOUT_MIN_MS` | `5000` | Lower bound for the randomized `cluster-node-timeout` (ms) |
| `CHAOS_NODE_TIMEOUT_MAX_MS` | `15000` | Upper bound for the randomized `cluster-node-timeout` (ms) |
| `CHAOS_WORKLOAD_TYPE` | `StatefulSet` | `StatefulSet` or `Deployment` |
| `CHAOS_PERSISTENCE` | `false` | Enable persistence (requires StatefulSet) |
| `CHAOS_AOF` | `true` | With persistence, set `appendonly yes` in `spec.config` |
| `CHAOS_SCENARIOS` | all except disabled | Comma-separated list of scenarios to run |
| `CHAOS_SEED` | current time | Random seed for reproducibility |
| `CHAOS_MODE` | `random` | `random` or `sequential` scenario selection |
| `CHAOS_TARGET_SHARDS` | `random` | Shards to target: `random` (1 to N shards each iteration), `randomX` (exactly X random shards), `all`, or comma-separated indices (e.g. `0,2`) |
| `CHAOS_RECOVERY_TIMEOUT` | `15s × pods` (min 5m) | Max time to wait for cluster recovery |
| `CHAOS_TOLERATION_SECONDS` | `0` | Pod toleration seconds for not-ready/unreachable (0 = not set) |
| `CHAOS_NUM_KEYS` | `100000` | Number of keys |
| `CHAOS_DATA_SIZE` | `3` | Size of each key's value in bytes |
| `CHAOS_WRITE_RPS` | `20` | Continuous write rate after seeding (0 = seed only, client exits) |
| `CHAOS_CPU_PRESSURE` | `false` | Throttle Kind worker node CPUs each iteration |
| `CHAOS_CPU_MIN` | `0.3` | Minimum CPU limit when throttling |
| `CHAOS_CPU_MAX` | `1.0` | Maximum CPU limit when throttling |
| `CHAOS_COMPOUND_MAX_DELAY` | per compound, `10s` unless listed | Max random delay between the steps of a compound scenario, for all compounds |

Each run picks a random `cluster-node-timeout` in `[CHAOS_NODE_TIMEOUT_MIN_MS, CHAOS_NODE_TIMEOUT_MAX_MS]` and sets it on the cluster, so failover and recovery timing vary across runs.

## Scenarios

| Scenario | Description |
|----------|-------------|
| `delete-primary-pod` | Force deletes the primary pod of targeted shards |
| `delete-replica-pod` | Force deletes a replica pod of targeted shards |
| `delete-primary-pod-graceful` | Deletes the primary pod of targeted shards with its grace period |
| `delete-replica-pod-graceful` | Deletes a replica pod of targeted shards with its grace period |
| `evict-primary-pod` | Evicts the primary pod of targeted shards, one at a time through the PodDisruptionBudget |
| `evict-replica-pod` | Evicts a replica pod of targeted shards, one at a time through the PodDisruptionBudget |
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
| `stop-worker-node` | Stops (kills) the Kind worker node hosting the primary and leaves it down; pair with `start-worker-node` in a compound (disabled by default) |
| `start-worker-node` | Restarts the node(s) `stop-worker-node` stopped this iteration (disabled by default) |
| `network-partition-primary` | Isolates the primary from the cluster (disabled by default) |
| `network-partition-replica` | Isolates a replica from the cluster (disabled by default) |
| `partition-worker-node` | Network-isolates the Kind worker node hosting the primary and leaves it cut off; pair with `heal-worker-node` in a compound (disabled by default) |
| `heal-worker-node` | Heals the node(s) `partition-worker-node` isolated this iteration (disabled by default) |
| `crash-primary` | Crashes the primary's server process (DEBUG SEGFAULT) (disabled by default) |
| `crash-replica` | Crashes a replica's server process (DEBUG SEGFAULT) (disabled by default) |
| `scale-shards+scale-shards` | Scales shards, then scales them again while the first change is in progress |
| `scale-replicas+delete-primary-pod` | Scales replicas, then deletes the primary pod of targeted shards |
| `scale-shards+delete-controller-pod` | Scales shards, then kills the operator controller pod while the change is in progress |
| `scale-replicas+evict-primary-pod` | Scales replicas, then evicts the primary pod of targeted shards while new replicas sync |

Scenarios that target replicas are skipped when `CHAOS_REPLICAS=0`.

Scenarios marked "disabled by default" are excluded unless explicitly listed in `CHAOS_SCENARIOS`.
They require `CHAOS_TOLERATION_SECONDS` to be set for meaningful eviction testing.

`stop-worker-node` and `start-worker-node` are a pair: `stop-worker-node` leaves the node
down so other scenarios run while it is gone, and `start-worker-node` restarts it. Use
them as the ends of a compound, e.g.
`stop-worker-node+scale-shards+delete-primary-pod+start-worker-node`. Run on its own,
`stop-worker-node` leaves the node down and the iteration's recovery check fails.

`partition-worker-node` and `heal-worker-node` are the same kind of pair for a network
partition instead of a stop. The difference is what the node does while out: a stopped
node reboots (new pod IPs, cold reload), a partitioned node keeps running and the same
instance rejoins on heal. Both leave the node faulted until the paired step and fail the
recovery check if run without their partner.

All worker node faults skip the nodes hosting the controller-manager and the background
writer, since losing either fails the operator restart or writer check. The writer is
started on the controller-manager's node so that normally only one node is skipped.

### Fault Cleanup

Each scenario reverts its own fault, rolling back the targets it already faulted if
injection fails partway through. The exceptions are `stop-worker-node` and
`partition-worker-node`, whose paired `start-worker-node` and `heal-worker-node` do the
revert.

Worker node faults (stopped nodes, network partitions, node pauses, CPU throttling) are
reverted in `AfterSuite` as well, since worker nodes are docker containers that outlive
the suite and would carry the fault into the next run. Pod faults (container pauses) need
no such cleanup: teardown deletes the ValkeyCluster and its pods go with it.

A run that fails or is interrupted deliberately keeps the Kind cluster for
troubleshooting. Remove it with `kind delete cluster --name valkey-operator-test-chaos`.

### Client

A custom Go client (`test/chaos/client/`) handles seeding and continuous writes.
It is built and loaded into Kind automatically during `BeforeSuite`.

**Seeding:** Writes `CHAOS_NUM_KEYS` keys sequentially, starting at `key:000000000000`,
with a fixed value of `CHAOS_DATA_SIZE` bytes.

**Continuous writes:** After seeding, the client overwrites the same keys at `CHAOS_WRITE_RPS` rate.
This generates replication traffic on all shards, simulating a real workload during fault injection.
Set `CHAOS_WRITE_RPS=0` to disable (client seeds and exits).

**Stats output** (printed at test end and in pod logs):
```text
2026/06/14 15:50:53 writes=5721 missing=0 errors=21 rps=20.0
```
- `writes`: successful SET commands since seeding completed
- `missing`: updates skipped because the key no longer existed (lost data)
- `errors`: failed SET commands (cluster unavailable, CLUSTERDOWN, timeout)
- `rps`: configured target rate

Errors during faults are expected. Sustained errors after recovery indicate an operator bug.

### Compound Scenarios

Use the `+` separator in `CHAOS_SCENARIOS` to inject multiple faults back-to-back within a single iteration.
Each sub-scenario runs sequentially with a random delay between them, testing that the operator handles overlapping faults mid-reconciliation.
`CHAOS_COMPOUND_MAX_DELAY` sets the maximum delay for all compounds.
A scenario can appear more than once.

```bash
# Scale shards while killing a primary during the operation
CHAOS_SCENARIOS=scale-shards+delete-primary-pod make test-chaos

# Multiple compounds, randomly selected each iteration
CHAOS_SCENARIOS=scale-shards+delete-primary-pod,scale-replicas+delete-replica-pod make test-chaos

# Triple fault: scale + kill primary + kill replica
CHAOS_SCENARIOS=scale-shards+delete-primary-pod+delete-replica-pod make test-chaos

# Change the shard count again while the first change is still running
CHAOS_SCENARIOS=scale-shards+scale-shards CHAOS_COMPOUND_MAX_DELAY=60s make test-chaos
```

A compound is expected to lose data when:

- one of its steps loses data on its own, or
- one step kills a primary while another removes its replicas (e.g. `delete-replica-pod`).

Replicas added by `scale-replicas` in the same iteration do not count, since they have not synced yet.
The test then logs how many keys were lost and re-seeds. Otherwise the data is verified, as after a single scenario.

## Reading the Output

Each iteration logs:

```text
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
- **KEY COUNT before** — per-shard key distribution before fault injection. With enough keys (the default 100000) every shard has keys, so a shard showing 0 means data was already lost before this iteration. With a very small `CHAOS_NUM_KEYS`, some shards can legitimately hold none.

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

# Run scenarios while a worker node is down (true node loss, then recovery)
CHAOS_SCENARIOS=stop-worker-node+scale-shards+delete-primary-pod+start-worker-node \
  CHAOS_TOLERATION_SECONDS=10 KIND_WORKERS=4 make test-chaos

# Run scenarios while a worker node is network-partitioned, then heal it
CHAOS_SCENARIOS=partition-worker-node+scale-shards+delete-primary-pod+heal-worker-node \
  CHAOS_TOLERATION_SECONDS=10 KIND_WORKERS=4 make test-chaos

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
make test-chaos 2>&1 | tee -i chaos-run1.log

# Terminal 2: different cluster name and kubeconfig to avoid conflicts
KIND_CLUSTER_CHAOS=chaos-2 KUBECONFIG=/tmp/chaos-2.conf \
  make test-chaos 2>&1 | tee -i chaos-run2.log
```

Each run creates its own Kind cluster, so they don't interfere with each other.
