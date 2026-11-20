# Architecture

This document describes the internal implementation of the valkey-operator for developers contributing to the codebase.

For the user-facing API see [valkeycluster.md](valkeycluster.md). For the ValkeyNode CRD design rationale see [valkeynode-design.md](valkeynode-design.md). For status conditions and events see [status-conditions.md](status-conditions.md).

## Controllers

### ValkeyClusterReconciler

`internal/controller/valkeycluster_controller.go` — owns top-level orchestration. Each reconcile loop:

1. Upserts a headless Service and PodDisruptionBudget
2. Reconciles ACL users (writes the `valkey-<cluster>-acl` and `valkey-<cluster>-system-passwords` Secrets, with type `valkey.io/acl`)
3. Upserts the `valkey-<cluster>` ConfigMap containing the health-check scripts, the init container script and the primary handoff script. The server config is not in it: each node's config lives in its own ConfigMap (see [Config revisions](#config-revisions))
4. Connects to live pods via `internal/valkey.GetClusterState` (CLUSTER INFO / CLUSTER NODES) to build a `ClusterState`
5. Creates/updates ValkeyNode CRs — one per (shard, node-index) pair, named `<cluster>-<N>-<M>`. Each desired spec carries `spec.configRevision`. Updates are one-at-a-time, replicas-before-primary within each shard (the snapshot identifies the actual primary, which may differ from node-index 0 after a failover)
6. Issues CLUSTER MEET, CLUSTER ADDSLOTSRANGE, CLUSTER REPLICATE in phases
7. Handles scale-in (drains slots via CLUSTER MIGRATESLOTS, deletes excess ValkeyNodes) and scale-out (rebalances slots via `internal/valkey.PlanRebalanceMove`)

### ValkeyNodeReconciler

`internal/controller/valkeynode_controller.go` — manages the workload for a single node:

1. Writes the node's config ConfigMap `valkey-<node>`: the pinned file for `spec.configRevision` when it is missing, and `live.conf`
2. Ensures a PVC (if persistence is configured)
3. Ensures a StatefulSet or Deployment (determined by `spec.workloadType`, immutable)
4. Deletes config revision keys that nothing uses any more
5. Updates `status.ready`, `status.podIP`, `status.role`, and `status.observedGeneration`
6. Once the pod is ready, applies live config with CONFIG SET and records it in `status.appliedLiveConfig`

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

- It triggers, it never writes. The ValkeyNode controller stays the sole writer of `status.role`, so a false positive costs one extra reconcile and nothing else.
- Triggers travel in-process over a channel (`source.Channel`), so a tick where every role matches performs no API operations at all.
- It runs only on the elected leader
- A Valkey instance that fails to answer is backed off exponentially (up to a minute) rather than dialled every tick.
- The Pod watch and the 30s backstop are both retained: the watch is faster for pod recreation, and the backstop is what makes a wedged poller a latency regression rather than an availability bug.

Each tick gets one client per node from the `ClientProvider` in `internal/controller/clients.go`, which takes it from the pool in `internal/valkey/pool.go`. The pool keeps each client open across ticks and reconciles, so a tick normally opens no connections.

## Key packages

- `internal/valkey/` — pure Valkey protocol layer: `ClusterState` / `NodeState` types, CLUSTER NODES parsing, slot range arithmetic, slot migration and rebalancing logic
- `internal/controller/config.go` — renders server config from `spec.config` and the base config, embeds `scripts/` (liveness/readiness probe scripts and `init.sh`) into the scripts ConfigMap
- `internal/controller/config_pinned.go`: renders the pinned file and `live.conf`, names a revision, and decides which revision a node references (`pinnedConfigRef`)
- `internal/controller/config_revision.go`: the parent sets `spec.configRevision` here, reading the live revision from the node's ConfigMap
- `internal/controller/node_config.go`: the node controller writes `valkey-<node>` here and deletes unused revision keys
- `internal/controller/users.go` — manages the system passwords Secret and the ACL Secret built from the user-defined password Secrets
- `internal/controller/secret_migration.go` — moves Secrets that earlier releases created under `internal-` names to their `valkey-` names
- `internal/controller/failover.go` — proactive failover logic: before rolling a primary, promotes a healthy replica via CLUSTER FAILOVER TAKEOVER
- `internal/controller/valkeynode_resources.go` — builds the StatefulSet/Deployment spec (containers, volumes, probes, affinity)
- `internal/controller/status.go` — condition helpers (`setCondition`)

## Naming convention

ValkeyNode names encode position: `<cluster>-<shardIndex>-<nodeIndex>`. Node-index 0 is the *initial* primary; 1+ are replicas. After a failover, Valkey may promote a replica — the labels are not updated; the live role is always read from CLUSTER NODES. Labels `valkey.io/shard-index` and `valkey.io/node-index` are used by the reconciler to determine slot assignment vs. replication.

Every other child of a ValkeyCluster takes the `valkey-` prefix, so it cannot collide with an unrelated object in the namespace: the headless Service, PodDisruptionBudget and scripts ConfigMap are `valkey-<cluster>`, each node's StatefulSet or Deployment is `valkey-<cluster>-<shardIndex>-<nodeIndex>`, and the Secrets are `valkey-<cluster>-<purpose>` (`acl`, `system-passwords`, `tls-trust`), with no kind suffix. A Secret the user creates, such as a user password Secret, does not take the prefix. See [Operator-managed Secrets](valkeycluster.md#operator-managed-secrets).

## Config revisions

A node's server config is split into two files, both in the node's `valkey-<node>` ConfigMap.

The pinned file holds the user's restart-only keys and the base config: everything in `spec.config` except the live-settable allowlist (`liveConfigAllowlist`: `maxmemory`, `maxmemory-policy`, `maxclients`) and keys the image's Valkey version does not support. It ends with `include /config/live.conf`, so a live value overrides any copy in the pinned file. Its revision is the first 12 hex characters of the SHA-256 of its content, and it is stored under the key `valkey-<rev>.conf`.

`live.conf` holds the live-settable keys. It is rendered from `status.appliedLiveConfig`, filtered to allowlisted keys still in `spec.config`. When the status is empty it is rendered from the allowlisted keys in `spec.config`. A restarted pod therefore boots with the values its server last accepted.

### Who writes what

The ValkeyCluster controller sets `spec.configRevision` on each node, and nothing else writes it. It renders the pinned file from the desired node spec. When the node's live workload template references a different revision, it reads that revision's content from `valkey-<node>` and keeps the live revision unless a key outside the allowlist differs (`pinnedConfigRef`). Changing a live value or growing the allowlist therefore rolls nothing. If its cache lacks the live revision's key, the parent requeues until the cache catches up.

The ValkeyNode controller owns `valkey-<node>` through a controller reference and is its only writer. A ConfigMap of that name it does not control, such as the shared ConfigMap of a cluster named `<node>`, is left alone and the node reports `Ready=False` with reason `ConfigMapError`. The ConfigMap carries the node's labels, including `valkey.io/cluster`. Before it builds the workload, the node controller:

- writes `valkey-<rev>.conf` for `spec.configRevision` when the key is missing, and only when the pinned file it renders from its own spec hashes to that revision. Otherwise it sets `Ready=False` with reason `ConfigRevisionMismatch`, writes nothing and leaves the workload alone. The field is required. A node stored before it existed has none until the parent reaches it in the upgrade roll; until then the node controller leaves its ConfigMap and workload alone and reports status as usual.
- never rewrites an existing revision key, and merge-patches the keys it changes, so other revision keys are kept.
- writes `live.conf` when the key is missing or its content differs from the render.
- removes any key that is neither a revision key nor `live.conf`, such as scripts an older operator left under the same name.

The node controller writes `status.appliedLiveConfig` in the same status update that sets `LiveConfigApplied=True`, after CONFIG SET succeeds. When `spec.config` holds no allowlisted key, the update that clears the condition also clears `status.appliedLiveConfig`.

### Pods

The config volume of the pod template mounts the `valkey-<rev>.conf` key for `spec.configRevision`, so a new revision changes the template and rolls the pod. The parent reads the live revision from the same volume item. The template stays a pure function of the node spec, so the ValkeyCluster controller and the ValkeyNode controller compute the same `spec.workloadRevision`, and the roll is authorized one node at a time.

The `valkey-conf` volume is one ConfigMap volume on `valkey-<node>` with two items: `valkey-<rev>.conf` mounted as `/config/valkey.conf` and `live.conf` as `/config/live.conf`. One volume means the kubelet swaps both files in one atomic write. The server command is `valkey-server /config/valkey.conf`. The `scripts` volume mounts only the script keys of `valkey-<cluster>`.

### Cleanup

The node controller deletes a revision key when all of these hold:

- it is not `spec.configRevision`
- the workload's pod template does not reference it
- no pod of the node mounts it, read through the APIReader with terminating pods included
- the workload has finished its roll: its controller has observed the latest generation, and for a StatefulSet `updateRevision` equals `currentRevision`, or for a Deployment every replica is updated

Pods and the workload are read only when the ConfigMap holds more than one revision key. The delete is an optimistic-lock merge patch, so a key written since the read is not dropped. `valkey-<node>` itself is deleted with its ValkeyNode through the owner reference.

During a roll `valkey-<node>` holds two revisions and `live.conf` under the 1 MiB ConfigMap limit, so a `spec.config` near half that size fails mid-roll with `ConfigMapError`.

### Upgrading

Older operators wrote the full `valkey.conf` into `valkey-<cluster>` and stamped a `valkey.io/config-hash` annotation on the pod template. The new template drops that annotation and mounts the node ConfigMap `valkey-<node>`, so existing clusters roll once on upgrade, one node at a time. The `valkey.conf` key an older operator left in `valkey-<cluster>` is no longer updated, and pods that have rolled do not mount it.

## Init container

Every ValkeyNode pod runs a `server-init` init container before the server. It runs `/scripts/init.sh` from the scripts ConfigMap and can write files to the `init-conf` emptyDir, which the server container mounts at `/init`. The script is a no-op today. Because it lives in the ConfigMap, changing it updates the scripts hash on the ConfigMap and never the pod template, so it does not roll pods; a pod runs the new script the next time it starts. An init container does not re-run when the server container restarts in place.

The init container copies the server container's image, pull policy, resources, security context, env and volume mounts after `spec.containers` patches. Matching resources and security context keep the pod's QoS class and pass the same Pod Security admission checks as the server. Matching env and mounts give `init.sh` the same view as the server, including `/data`, TLS certificates and the operator credentials, so a script that needs them does not change the pod template.

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
