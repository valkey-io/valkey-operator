# Valkey Design

`Valkey` is the user-facing CRD for Valkey outside cluster mode. It describes one primary, optional replicas, and in future an optional failover authority. The data pods run on [ValkeyNode](valkeynode-design.md), the same internal primitive `ValkeyCluster` uses.

The full design, including replication and `ValkeySentinel`, was settled in [discussion #387](https://github.com/valkey-io/valkey-operator/discussions/387), which grew out of [issue #198](https://github.com/valkey-io/valkey-operator/issues/198). This document records what is built today and the decisions behind it. Read #387 for the parts not built yet.

## Goals

- One CRD that grows from a lone primary to a replicated, Sentinel-monitored instance without a migration.
- Reuse the `ValkeyCluster` building blocks for the data plane: ACL and system users, the server ConfigMap, the headless Service, the PDB, and `ValkeyNode`.
- Keep `ValkeyNode` unchanged.
- Let each CRD's API evolve on its own, while the controller logic behind them stays shared.

## Non-Goals

- Sharding or slot management. That is `ValkeyCluster`, or a future `ValkeyCell` that creates N `Valkey`s.
- Migrating between a `ValkeyCluster` and a `Valkey`.
- Client-side proxying or role-selector Services.

## Delivery

The work ships in the phases laid out in #387. Phase 1 is split into slices.

| Slice | Scope | Status |
| --- | --- | --- |
| CRD | `Valkey` types, CEL validation, `spec.replicas` pinned to 0 ([#390](https://github.com/valkey-io/valkey-operator/pull/390)) | In review |
| Standalone controller | One primary through one `ValkeyNode`, children, status | This document |
| Replication | `spec.replicas > 0`, `REPLICAOF` wiring, per-node governing Services, boot-time discovery init container | Not started |
| Sentinel | `spec.failover`, `ValkeySentinel`, monitor registration, planned failover | Not started |

`spec.replicas` is pinned to 0 by CEL until replication lands. Relaxing a rule later is backwards compatible. Adding one later would not be.

## API

`ValkeySpec` mirrors the `ValkeyClusterSpec` fields that apply to a single shard: image, resources, scheduling, exporter, workload type, persistence, users, containers, config, networking, PDB, pod security context and grace period. Fields shared with `ValkeyCluster` carry the same CEL rules, including persistence immutability and immutable `workloadType`.

Networking, TLS, scheduling and PDB config are copied per CRD rather than shared. `ValkeyCluster`'s versions carry cluster-only fields such as discovery and node or zone spread. A shared type would let a field added for one topology leak into another's schema. Generic leaf types such as `PersistenceSpec`, `ExporterSpec`, `UserAclSpec` and `TLSCertificates` are reused as they are.

`spec.config` rejects every `cluster-` key and `replicaof`/`slaveof`. A `Valkey` always runs with `cluster-enabled no`, and the operator owns replication topology.

TLS is server-only. `ValkeyTLSSpec` has no client-auth block, so the node runs with optional client certificates.

### Example

A minimal instance is in [`config/samples/v1alpha1_valkey.yaml`](../config/samples/v1alpha1_valkey.yaml). A fuller one:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cache-users
stringData:
  alicepw: change-me
---
apiVersion: valkey.io/v1alpha1
kind: Valkey
metadata:
  name: cache
spec:
  users:
    - name: alice
      enabled: true
      passwordSecret:
        name: cache-users
        keys: [alicepw]
      commands:
        allow: ["@read", "@write", "@connection"]
      keys:
        readWrite: ["app:*"]
  config:
    maxmemory: 100mb
    maxmemory-policy: allkeys-lru
  persistence:
    size: 1Gi
  networking:
    tls:
      certificates:
        server:
          secretName: cache-tls
```

## Reusing ValkeyCluster logic

The rule is: types copied, logic shared.

The `ValkeyCluster` child resource builders were extracted into free functions in `internal/controller`. Each takes the owner object, the name, the namespace and the labels instead of a `*ValkeyCluster`. Both controllers call the same implementation. The `ValkeyCluster` methods remain as thin wrappers with their original signatures.

| Shared function | Builds |
| --- | --- |
| `upsertHeadlessService` | Headless Service over the pods labelled `valkey.io/cluster=<name>` |
| `reconcilePDB` | `maxUnavailable: 1` budget, or deletes it when disabled |
| `reconcileUsersAcl` | ACL Secret, system password Secret, and the revision user |
| `upsertServerConfigMap` | `valkey.conf` plus the probe scripts |
| `buildServerConfigFrom` | Renders `valkey.conf` from a base config and the user config |
| `setConditionOn` | Sets a condition on any conditions slice |

`resourceClient` bundles the client, API reader, scheme and event recorder that these functions need. Passing the client to free functions follows the `controllerutil` style.

The aclfile is still assembled in exactly one function, `reconcileUsersAcl`. The revision user it appends last is what lets a node report `ACLApplied`. A second assembly path that skipped it would leave every node reporting `ACLApplied=False`.

Status derivation is not shared. The status structs and state vocabulary differ per kind, so each controller keeps its own `updateStatus`. The state priority is the same in both: `Degraded`, then `Ready`, then `Progressing`, then `Failed`.

## Standalone server config

`getBaseConfig` is cluster-shaped. It sets `cluster-enabled yes` and `shutdown-on-sigterm failover`. A standalone `Valkey` renders with `getStandaloneBaseConfig` instead, which keeps the managed directives (`aclfile`, `dir`, TLS) and drops the cluster ones. A standalone primary has no replica to hand off to, so `shutdown-on-sigterm` stays at the server default.

The node controller hashes the node spec into the pod template to trigger rolls. For a `Valkey`-owned node it still hashes with the cluster base config. That has no effect, because the hash only has to change when the node spec changes. The config the server actually loads comes from the parent's ConfigMap.

The node controller already supports standalone servers. It ignores `WorkloadRevision` and applies template changes immediately. It reads the role from `INFO replication` when cluster mode is off.

## Reconcile flow

The controller runs these steps in order on every reconcile:

1. Fetch the `Valkey`. Return without action if it is gone or being deleted.
2. Upsert the headless Service.
3. Reconcile the PDB. At `spec.replicas: 0` this deletes any existing budget.
4. Reconcile the ACL. This creates the system password Secret if missing, then writes the internal ACL Secret.
5. Upsert the server ConfigMap with the standalone `valkey.conf`.
6. Create or update the single `ValkeyNode`.
7. Derive conditions from the node's status, then patch the `Valkey` status.

A failure in steps 2 to 6 sets `Ready=False` with that step's reason, writes status, and returns the error so the request is retried with backoff. The controller owns every child kind, so a change to any child, including the node's status, triggers a new reconcile. No periodic requeue is needed.

## Child resources and naming

| Resource | Name | Notes |
| --- | --- | --- |
| Headless Service | `valkey-<name>` | Port 6379 |
| Server ConfigMap | `valkey-<name>` | `valkey.conf` and probe scripts |
| ACL Secret | `internal-<name>-acl` | aclfile mounted by the node |
| System password Secret | `internal-<name>-system-passwords` | `_operator`, `_exporter` and the other system users |
| PodDisruptionBudget | `valkey-<name>` | Not created while `spec.replicas` is 0 |
| ValkeyNode | `<name>-0-0` | Shard 0, node 0 |

`metadata.name` is limited to 37 characters. The longest derived name, `internal-<name>-system-passwords`, must fit a 63 character DNS label.

### Known collision

The names above come from the same helpers `ValkeyCluster` uses, and both kinds label their pods `valkey.io/cluster=<name>`. A `Valkey` and a `ValkeyCluster` with the same name in one namespace collide on every child. The second object fails `SetControllerReference` and stays at `Ready=False`. The selectors also overlap. Nothing guards against this yet. Changing the names after release would orphan existing children and roll every pod, so the fix should land before release.

## PodDisruptionBudget

A single pod gets no budget. `maxUnavailable: 1` would allow evicting the only pod, and `maxUnavailable: 0` would block node drains permanently. The controller deletes any existing budget while `spec.replicas` is 0, whatever the mode. With replicas, `Managed` renders `maxUnavailable: 1` over the instance's pods, and `Disabled` creates none.

## Status

| Field | Meaning |
| --- | --- |
| `state` | `Initializing`, `Reconciling`, `Ready`, `Degraded` or `Failed` |
| `reason`, `message` | Taken from the condition that decided `state` |
| `primary` | The `ValkeyNode` serving as primary |
| `replicas`, `readyReplicas` | Replica counts, excluding the primary. Always 0 today |
| `observedGeneration` | Last generation the controller reconciled |

Conditions follow [status-conditions.md](status-conditions.md).

| Condition | True when |
| --- | --- |
| `Ready` | The node reports ready. Reason `InstanceHealthy` |
| `Progressing` | Waiting for the node to become ready |
| `Degraded` | The node failed to apply the ACL. Reason `ACLApplyFailed` |

A child resource error sets `Ready=False` with the existing reason for that resource, such as `ServiceError` or `ConfigMapError`, and the reconcile is retried.

```
$ kubectl get valkey
NAME         STATE   REASON            PRIMARY          AGE
standalone   Ready   InstanceHealthy   standalone-0-0   2m
```

## Lifecycle and deletion

Every child carries an owner reference to the `Valkey`, so deletion cascades through garbage collection. There is no finalizer today. PVCs are retained, following `ValkeyNode`. The controller does nothing while the object is being deleted, so it never recreates children that the garbage collector is removing.

Sentinel mode adds a `valkey.io/sentinel-deregister` finalizer, because monitor registration lives in the Sentinel pods and outlives the `Valkey`. See #387.

## Known gaps

- The name collision with `ValkeyCluster` described above.
- No Prometheus metrics for `Valkey`.
- No viewer, editor or admin RBAC roles for `valkeys`.
- No e2e test.

## Future work

The next slices are specified in #387:

- **Replication.** `1 + spec.replicas` nodes, per-node governing Services for stable pod DNS, `REPLICAOF` wiring, and a boot-time discovery init container present on every node from the first release. `workloadType: Deployment` is rejected above `replicas: 0`.
- **Failover.** `spec.failover.mode` declares who promotes: `None`, `Sentinel`, or a reserved `Operator`. A `ValkeySentinel` selects instances by label and also requires `mode: Sentinel`, so both sides agree.
- **Planned operations.** `SENTINEL FAILOVER` before rolling the primary, with `OnDelete` rollouts gated on replication and quorum health.

#387 names the replicated data nodes `<name>-<index>`, without the shard dimension. The standalone controller uses `<name>-0-0`. The two must be reconciled before release, because renaming a `ValkeyNode` later replaces its workload.
