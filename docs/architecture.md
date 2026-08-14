# Architecture

This document describes the internal implementation of the valkey-operator for developers contributing to the codebase.

For the user-facing API see [valkeycluster.md](valkeycluster.md). For the ValkeyNode CRD design rationale see [valkeynode-design.md](valkeynode-design.md). For status conditions and events see [status-conditions.md](status-conditions.md).

## Controllers

### ValkeyClusterReconciler

`internal/controller/valkeycluster_controller.go` — owns top-level orchestration. Each reconcile loop:

1. Upserts a headless Service and PodDisruptionBudget
2. Reconciles ACL users (creates an internal Secret with type `valkey.io/acl`)
3. Upserts a ConfigMap containing `valkey.conf` and health-check scripts. A SHA-256 of `valkey.conf` is propagated to each ValkeyNode spec to trigger rolling restarts when config changes
4. Connects to live pods via `internal/valkey.GetClusterState` (CLUSTER INFO / CLUSTER NODES) to build a `ClusterState`
5. Creates/updates ValkeyNode CRs — one per (shard, node-index) pair, named `<cluster>-<N>-<M>`. Updates are one-at-a-time, replicas-before-primary within each shard (the snapshot identifies the actual primary, which may differ from node-index 0 after a failover)
6. Issues CLUSTER MEET, CLUSTER ADDSLOTSRANGE, CLUSTER REPLICATE in phases
7. Handles scale-in (drains slots via CLUSTER MIGRATESLOTS, deletes excess ValkeyNodes) and scale-out (rebalances slots via `internal/valkey.PlanRebalanceMove`)

### ValkeyNodeReconciler

`internal/controller/valkeynode_controller.go` — manages the workload for a single node:

1. Ensures a ConfigMap (skipped if `ServerConfigMapName` is set, i.e. when owned by ValkeyCluster)
2. Ensures a PVC (if persistence is configured)
3. Ensures a StatefulSet or Deployment (determined by `spec.workloadType`, immutable)
4. Updates `status.ready`, `status.podIP`, `status.role`, and `status.observedGeneration`

#### Role resolution

The ValkeyNode controller is the only writer of `status.role`, and always resolves it from the node's own server.

In cluster mode the role comes from **slot ownership** on the `myself` line of CLUSTER NODES: a node owning at least one slot is `primary`, everything else is `replica`. INFO replication is used only in standalone mode, since a restarting cluster replica reports `role:master` (and the `master` flag) for several seconds before replication is re-established, whereas slot ownership stays correct throughout. The tradeoff is that a primary owning no slots — a new shard before slot assignment — reads as `replica` until slots are assigned.

Resolution is event-driven rather than tied to the reconcile cadence:

- a Pod watch: create and delete events pass through, and update events are filtered to readiness, pod-IP and phase transitions
- the RolePoller (below), for role changes Kubernetes never sees

Both are triggers only: the ValkeyNode controller re-reads its own state on every reconcile and never trusts a value handed to it.

The role follows the pod, not the workload: it is cleared when the pod is not ready, kept at its last-known value on a transient read failure, and preserved while the node's StatefulSet/Deployment rolls (`status.ready` still reports the rollout).

### RolePoller

`internal/controller/rolepoller.go` — a manager `Runnable` that samples live topology every `DefaultRolePollInterval` (5s) and pushes a reconcile trigger for any node whose live role differs from its `status.role`. The interval is a constant, not a flag: polling cost is an implementation concern, not an operator-facing knob.

It exists because a failover between two healthy pods moves nothing in Kubernetes: no restart, no readiness flip, no IP change. No watch can fire, so without the poller the only detector is the 30s backstop requeue.

- **It triggers; it never writes.** The ValkeyNode controller stays the sole writer of `status.role`, so a false positive costs one extra reconcile and nothing else.
- Triggers travel in-process over a channel (`source.Channel`), so a tick where every role matches performs no API operations at all.
- It runs only on the elected leader
- A Valkey instance that fails to answer is backed off exponentially (up to a minute) rather than dialled every tick.
- The Pod watch and the 30s backstop are both retained: the watch is faster for pod recreation, and the backstop is what makes a wedged poller a latency regression rather than an availability bug.

Each tick opens one connection per node. That is the cost that sets the interval, and the `scrapeFunc` seam is where pooled clients will replace it.

## Key packages

- `internal/valkey/` — pure Valkey protocol layer: `ClusterState` / `NodeState` types, CLUSTER NODES parsing, slot range arithmetic, slot migration and rebalancing logic
- `internal/controller/config.go` — builds `valkey.conf` from `spec.config`, embeds `scripts/` (liveness/readiness shell scripts) into the ConfigMap
- `internal/controller/users.go` — manages the internal `_operator` system user Secret and user-defined ACL Secrets
- `internal/controller/failover.go` — proactive failover logic: before rolling a primary, promotes a healthy replica via CLUSTER FAILOVER TAKEOVER
- `internal/controller/valkeynode_resources.go` — builds the StatefulSet/Deployment spec (containers, volumes, probes, affinity)
- `internal/controller/status.go` — condition helpers (`setCondition`)

## Naming convention

ValkeyNode names encode position: `<cluster>-<shardIndex>-<nodeIndex>`. Node-index 0 is the *initial* primary; 1+ are replicas. After a failover, Valkey may promote a replica — the labels are not updated; the live role is always read from CLUSTER NODES. Labels `valkey.io/shard-index` and `valkey.io/node-index` are used by the reconciler to determine slot assignment vs. replication.

## Config hash propagation

When `valkey.conf` changes, ValkeyCluster computes a SHA-256 of the rendered `valkey.conf` string and writes it to `ValkeyNode.spec.serverConfigHash`. The ValkeyNode controller stamps this as a pod template annotation, triggering a rolling restart.

## Auto-generated files

The following files are generated by `make manifests` or `make generate` and must never be edited by hand:

- `config/crd/bases/*.yaml`
- `config/rbac/role.yaml`
- `api/v1alpha1/zz_generated.deepcopy.go`
- `PROJECT`

Never remove `// +kubebuilder:scaffold:*` comments — the Kubebuilder CLI injects code at these markers.

## Controller design patterns

- **Idempotent reconciliation** — every reconcile loop must be safe to run multiple times with the same outcome
- **Re-fetch before updates** — always `r.Get` the latest object before calling `r.Update` or `r.Patch` to avoid stale `resourceVersion` conflicts
- **Structured logging** — use `log := logf.FromContext(ctx)` and pass key-value pairs rather than formatting into the message string
- **Owner references** — set via `controllerutil.SetControllerReference` to enable automatic garbage collection of child resources
- **Watch secondary resources** — use `.Owns()` or `.Watches()` in `SetupWithManager` so changes to owned resources trigger reconciliation

## Testing

The unit/integration test suite in `internal/controller/` uses Ginkgo/Gomega against a real API server (envtest, no kubelet). E2e tests in `test/e2e/` run against a Kind cluster. See [developer-guide.md](developer-guide.md) for commands.
