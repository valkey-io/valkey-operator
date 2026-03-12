# ValkeyNode CRD - Design Proposal

**Date:** 2026-02-13
**Status:** Draft
**Authors:** jdheyburn

---

## Table of contents

- [Overview](#overview)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Resource Hierarchy](#resource-hierarchy)
- [Spec](#spec)
  - [Design Philosophy](#design-philosophy)
  - [Specification](#specification)
- [Status](#status)
  - [Conditions](#conditions)
  - [`kubectl get valkeynode`](#kubectl-get-valkeynode)
- [Reconciliation Logic](#reconciliation-logic)
- [Naming Conventions](#naming-conventions)
- [Lifecycle and Deletion Behaviour](#lifecycle-and-deletion-behaviour)
- [Interaction Contract with Parent Controllers](#interaction-contract-with-parent-controllers)
- [References](#references)

---

## Overview

This document proposes a ValkeyNode CRD which will be used to abstract away the concrete implementation of how the Valkey server workloads are provisioned to Kubernetes.

We wish to support both Deployments and StatefulSets, depending on the user requirements. Having an abstraction on top of these primitives will provide a common interface for the ValkeyCluster to abstract away this detail. We can use this opportunity to provide other abstractions that do not necessarily concern the parent controllers, such as management of volumes.

Additionally, singleton Deployments and StatefulSet can create pods with long, verbose names. A StatefulSet named `valkey-cache-0-0` (shard 0, pod index 0) will create a pod named `valkey-cache-0-0-0`, with the last 0 being the StatefulSet ordinal index. Conversely a Deployment named `valkey-cache-0-0` will create a pod named `valkey-cache-0-0-xyz-abc`. Abstracting this detail into a ValkeyNode such as `valkey-cache-0-0` can help users administer Valkey.

When we build out additional Valkey CRDs, they too would need to interface with Valkey servers, and delegating ownership to a ValkeyNode controller will simplify the parent controllers (at the trade off of additional complexity now).

ValkeyNode would be a CRD that is internal to the operator. It is not expected for the user to create ValkeyNodes themselves.

A proof of concept was created in [this branch](https://github.com/valkey-io/valkey-operator/compare/main...jdheyburn:valkey-operator:feat/valkeynode-crd).

## Goals

- Separation of concerns
  - ValkeyNode handles infrastructure
  - Parent controllers handle topology
- Configuration-driven behaviour
  - ValkeyNode behaviour is determined by config fields
  - Not coupled to parent CRD type
  - Generic and reusable

## Non-Goals

- Primary election / promotion decisions
- Failover orchestration
- Cluster slot management
- Multi-pod workloads (ValkeyNode always manages a single pod)

## Resource hierarchy

```
User Creates:
├─ ValkeyCluster
│ └─ Creates: ValkeyNode per pod
│ └─ Creates: Deployment/StatefulSet + PVC
```

## Spec

> N.B. Spec is open for feedback and opinions, and is not final. My intention is to iterate on this as we identify other use cases for it.
>
> Given it is an internal CRD, we can be forgiving to breaking changes

ValkeyNode is the operator-managed abstraction for single-pod deployments. **Users never create these directly** - they're created by parent controllers (ValkeyCluster).

### Design Philosophy

**Deliberately "dumb"** - ValkeyNode reconciles infrastructure without making topology decisions:

- Creates singleton Deployment or StatefulSet depending on what the user has requested
- Creates PVC (if persistence enabled)
- Configures Valkey based on spec
- Updates status with observed state

**Does NOT:**

- Decide which node should be primary
- Initiate failovers
- Manage cluster slot assignment
- Create other ValkeyNodes

### Specification

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyNode
metadata:
  name: cache-0-0
  namespace: default
  ownerReferences: # Set by parent controller
    - apiVersion: valkey.io/v1alpha1
      kind: ValkeyCluster
      name: cache
      controller: true
spec:
  # Pod management strategy
  # +kubebuilder:validation:Enum=Deployment;StatefulSet
  # +kubebuilder:default=StatefulSet
  workloadType: StatefulSet

  # Pod template specification
  podTemplate:
    metadata:
      labels: {}
      annotations: {}
    spec:
      nodeSelector: {}
      affinity: {}
      tolerations: []
      securityContext: {}
      containers:
        - name: valkey
          image: valkey/valkey:8.0
          resources: {}
          env: []

  # Persistent volume configuration
  persistence:
    # +kubebuilder:validation:Enum=disabled;enabled;external
    # +kubebuilder:default=disabled
    # disabled: No PVC, ephemeral storage (default)
    # enabled: Operator creates and manages PVC
    # external: User provides existing PVC via existingClaim
    mode: enabled
    # Required when mode=enabled
    size: 10Gi
    # +optional
    storageClassName: gp3

  # Valkey-specific configuration
  config:
    # maxmemory: 2gb

  # ... other fields to be added where appropriate

status:
  conditions: []
  podName: valkey-cache-0-0-0
  podIP: 10.0.1.5
  pvcName: valkey-cache-0-0-data

  # Observed replication state (queried via INFO replication)
  observedRole: primary # "primary" or "replica"
  observedReplicaOf: ""

  ready: true
```

### Status

#### Conditions

As an initial proposal for the conditions to be created:

| Condition Type   | Meaning                                                         |
| ---------------- | --------------------------------------------------------------- |
| `Ready`          | Pod is running and Valkey is responding to health checks        |
| `Progressing`    | Workload is being created or updated                            |
| `ResourcesReady` | All managed resources (Deployment/StatefulSet, PVC) are healthy |

#### `kubectl get valkeynode`

Exposing discovered attributes in the status enables users to view and filter key information about the pod.

Default view:

```text
kubectl get valkeynode
NAME        READY   ROLE      POD-IP       REPLICA-OF     AGE
cache-0-0   True    primary   10.0.1.5                    5m
cache-0-1   True    replica   10.0.1.6     cache-0-0      5m
cache-1-0   True    primary   10.0.1.7                    5m
cache-1-1   True    replica   10.0.1.8     cache-1-0      5m
cache-2-0   True    primary   10.0.1.9                    4m
cache-2-1   True    replica   10.0.1.10    cache-2-0      4m
```

Wide view (`-o wide`):

```text
kubectl get valkeynode -o wide
NAME        READY   ROLE      POD-IP       WORKLOAD       AGE   POD-NAME                REPLICA-OF
cache-0-0   True    primary   10.0.1.5     StatefulSet    5m    valkey-cache-0-0-0
cache-0-1   True    replica   10.0.1.6     StatefulSet    5m    valkey-cache-0-1-0      cache-0-0
cache-1-0   True    primary   10.0.1.7     StatefulSet    5m    valkey-cache-1-0-0
cache-1-1   True    replica   10.0.1.8     StatefulSet    5m    valkey-cache-1-1-0      cache-1-0
cache-2-0   True    primary   10.0.1.9     StatefulSet    4m    valkey-cache-2-0-0
cache-2-1   True    replica   10.0.1.10    StatefulSet    4m    valkey-cache-2-1-0      cache-2-0
```

#### Observed statuses

We can add fields that would be beneficial for the user to query and filter on.

- `observedRole` and `observedReplicaOf` are queried from `INFO replication`
- During failovers, status may temporarily differ from desired state until reconciliation

This can then plug into other interfaces as they can query the K8s API for the state of the Cluster and its ValkeyNodes.

## Reconciliation Logic

1. Ensure PVC exists (if persistence enabled)
1. Ensure Deployment/StatefulSet exists with `replicas: 1`
1. Wait for pod readiness
1. Query Valkey via `INFO replication` for observed role
1. Update status fields

## Naming Conventions

The controller will create child resources (i.e. Kubernetes primitives) with `valkey-` prepended to the ValkeyNode name. This is done so that even in primitive views (e.g. `kubectl get pods`) it is easily determined that the resources are related to Valkey.

| Resource               | Naming Pattern                  | Example                                                 |
| ---------------------- | ------------------------------- | ------------------------------------------------------- |
| Deployment/StatefulSet | `valkey-<valkeynode-name>`      | `valkey-cache-0-0`                                      |
| PVC                    | `valkey-<valkeynode-name>-data` | `valkey-cache-0-0-data`                                 |
| Pod                    | Managed by workload controller  | `valkey-cache-0-0-0` (sts) / `valkey-cache-0-0-xyz-abc` |

## Lifecycle and Deletion Behaviour

When a ValkeyNode is deleted:

- **Workload (Deployment/StatefulSet)**: Cascading delete via owner references — the workload and its pods are removed automatically.
- **PVC**: PVCs are **not** deleted when the ValkeyNode or its pod is deleted. This mirrors StatefulSet semantics where volumes persist beyond the lifecycle of the pod, allowing data to survive restarts, rescheduling, and scaling events. PVCs must be explicitly deleted by the user or parent controller if storage reclamation is desired. Users can disable this if they choose to.
- **Orphaned ValkeyNodes**: If a parent controller (e.g. ValkeyCluster) is deleted, ValkeyNodes are garbage collected via owner references and removed. PVCs will still be retained.

## Interaction Contract with Parent Controllers

ValkeyNode is **generic and unaware of its parent type** — it behaves identically whether owned by a ValkeyCluster, Valkey, or any future CRD. All topology awareness lives in the parent controller.

### What parents set on the spec

Parent controllers create ValkeyNode resources and populate the following depending on what mode it is being created in.

- `workloadType` — Deployment or StatefulSet, based on user preference
- `podTemplate` — container image, resources, scheduling constraints
- `persistence` — storage mode and size
- `config` — all Valkey configuration values the node needs (e.g. `cluster-enabled`, `maxmemory`). ValkeyNode applies these as provided; it does not inject its own defaults or interpret the values.

### What parents read from status

Parent controllers watch ValkeyNode status for:

- `ready` — whether the pod is running and Valkey is responding
- `observedRole` / `observedReplicaOf` — current replication topology as observed from `INFO replication`
- `podIP` — the pod's current IP for connecting to the Valkey process
- `podName` — the name of the managed pod

### Valkey command execution

Parent controllers are responsible for executing Valkey commands (`REPLICAOF`, `CLUSTER MEET`, `CLUSTER ADDSLOTS`, `FAILOVER`, etc.) directly against the pod using `status.podIP`. ValkeyNode does **not** run topology or replication commands — it only observes the resulting state via `INFO replication` and reports it in status.

This keeps ValkeyNode deliberately "dumb" and avoids coupling it to any specific topology model.

### Change discovery

Future extensibility includes the ability for parent controllers to set up **watches** on ValkeyNode resources.

For example, when a ValkeyNode's status changes (e.g. pod IP changes, `observedRole` flips during a failover, or `ready` transitions), the watch triggers re-reconciliation of the parent controller, allowing it to react accordingly.

## References

- [Initial CRD Design Strategy](https://github.com/jdheyburn/valkey-operator/blob/f042e6f63d206e67f1680fe9d66e6022254590db/docs/plans/2026-02-04-valkeynode-mvp-design.md)
- [ValkeyNode POC](https://github.com/valkey-io/valkey-operator/compare/main...jdheyburn:valkey-operator:feat/valkeynode-crd)
