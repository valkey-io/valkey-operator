# ValkeyNode Design

ValkeyNode is an internal CRD that abstracts the infrastructure used to create a Valkey pod. ValkeyNodes are not intended to be created by users — they are managed exclusively by parent controllers such as ValkeyCluster.

## Goals

- Separation of concerns: ValkeyNode handles infrastructure; parent controllers handle topology
- Configuration-driven behaviour: ValkeyNode behaviour is determined by spec fields, not coupled to any specific parent CRD
- Reusable across future Valkey deployment modes (standalone, replication, etc.)

## Non-Goals

- Primary election or promotion decisions
- Failover orchestration
- Cluster slot management
- Multi-pod workloads (ValkeyNode always manages a single pod)

## Workload Type

Users can define what workload type to use — either a StatefulSet or Deployment. The reason for this is that Valkey means different things to different users. It can be used as a cache or as a source-of-truth data store. If you're using Valkey as a cache and don't care about data durability, a Deployment is preferable. If you need stable storage and identity, a StatefulSet is more appropriate.

Supporting both workload types is why the distinction is abstracted behind a ValkeyNode CRD.

## Singletons

ValkeyNodes map 1:1 to pods. A ValkeyNode deploys its pod as either a StatefulSet or Deployment, but these workloads always have exactly one replica. This enables precise pod scheduling — for example, placing a primary pod in a specific availability zone while avoiding nodes that already host other primaries.

If an entire Valkey Cluster were deployed into a single StatefulSet, all pods in that StatefulSet would share the same spec, making advanced per-pod scheduling impossible.

See also: [ClickHouse singleton StatefulSet pattern](https://youtu.be/QLLKzOEMkUk?si=kLvKw8AMuvgGCwbT)

## Observability

Abstracting ValkeyNode into its own CRD exposes status fields on the resource, which makes diagnosing cluster state faster. For example, a ValkeyCluster with 3 shards produces the following views:

```
$ kubectl get valkeycluster -o wide
NAME             STATE   REASON           READYSHARDS   AGE
cluster-sample   Ready   ClusterHealthy   3             131m

$ kubectl get valkeynode
NAME                 READY   ROLE      POD                           AGE
cluster-sample-0-0   true    primary   valkey-cluster-sample-0-0-0   130m
cluster-sample-0-1   true    replica   valkey-cluster-sample-0-1-0   130m
cluster-sample-1-0   true    primary   valkey-cluster-sample-1-0-0   130m
cluster-sample-1-1   true    replica   valkey-cluster-sample-1-1-0   130m
cluster-sample-2-0   true    primary   valkey-cluster-sample-2-0-0   130m
cluster-sample-2-1   true    replica   valkey-cluster-sample-2-1-0   130m
```

This also makes it straightforward to build admin console tooling that queries the Kubernetes API for cluster state.

## Why Not Direct Pod Management?

The operator could manage pods directly rather than delegating to StatefulSets or Deployments. The reason not to is separation of concerns.

With direct pod management (DPM), if a pod were descheduled, it would be the operator's responsibility to reschedule it. If the operator were unavailable, the cluster would remain degraded. Using a StatefulSet or Deployment delegates pod reconciliation to a separate controller, decoupling cluster health from operator availability.

This design also means the cluster can become less dependent on the operator over time, without breaking changes.

That said, ValkeyNode's abstraction does not preclude direct pod management. A `Direct` WorkloadType could be added in the future to bypass StatefulSet and Deployment entirely — the design accommodates this without structural changes.

## Interaction Contract with Parent Controllers

ValkeyNode is deliberately "dumb" — it reconciles infrastructure without making topology decisions. All cluster-level awareness lives in the parent controller.

**Parents write to spec:**

- `workloadType` — Deployment or StatefulSet
- `podTemplate` — image, resources, scheduling constraints
- `persistence` — storage mode and size
- `config` — all Valkey configuration values (e.g. `cluster-enabled`, `maxmemory`); ValkeyNode applies these as-is without interpretation

**Parents read from status:**

- `ready` — whether the pod is running and Valkey is responding
- `observedRole` / `observedReplicaOf` — replication topology observed from `INFO replication`
- `podIP` — used by the parent to connect and issue Valkey commands
- `podName` — name of the managed pod

Parent controllers are responsible for issuing Valkey commands (`REPLICAOF`, `CLUSTER MEET`, `CLUSTER ADDSLOTS`, `FAILOVER`, etc.) directly against `status.podIP`. ValkeyNode only observes the resulting state and reports it.

## Naming Conventions

Child resources created by ValkeyNode have `valkey-` prepended to the ValkeyNode name. This makes their relationship clear in views like `kubectl get pods`.

| Resource               | Pattern                         | Example                      |
| ---------------------- | ------------------------------- | ---------------------------- |
| Deployment/StatefulSet | `valkey-<name>`                 | `valkey-cluster-sample-0-0`  |
| PVC                    | `valkey-<name>-data`            | `valkey-cluster-sample-0-0-data` |
| Pod                    | Managed by workload controller  | `valkey-cluster-sample-0-0-0` (StatefulSet) |

## Lifecycle and Deletion Behaviour

- **Workload**: Deleted via cascading owner references when the ValkeyNode is deleted.
- **PVC**: PVCs are **not** deleted when a ValkeyNode is deleted. This mirrors StatefulSet semantics — volumes persist beyond pod lifecycle to survive restarts and rescheduling. PVCs must be explicitly deleted if storage reclamation is desired.
- **Orphaned ValkeyNodes**: If a parent controller (e.g. ValkeyCluster) is deleted, its ValkeyNodes are garbage collected via owner references. PVCs are still retained.

## ValkeyCluster Integration

Users define a ValkeyCluster CRD to describe their cluster topology. The ValkeyCluster controller creates ValkeyNodes one by one, configuring each to form the cluster. This frees the ValkeyCluster controller to focus solely on cluster mechanics — slot distribution, replication topology, failover — rather than individual pod lifecycle.

## Future Uses

While ValkeyCluster is the first top-level CRD, Valkey can also be deployed in standalone or replication modes (with or without Sentinel). The underlying pod configuration is similar across all modes, so the ValkeyNode abstraction is designed to be reused by future CRDs without modification.
