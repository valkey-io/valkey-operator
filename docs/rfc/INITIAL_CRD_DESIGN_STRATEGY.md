> Represents the CRD design I submitted for review in this PR: TODO


# Valkey Operator CRD Architecture - Design Proposal

**Date:** 2026-01-22
**Status:** Draft
**Authors:** jdheyburn

---

## Table of contents

- [Overview](#overview)
- [Design Goals and Principles](#design-goals-and-principles)
- [CRD Architecture Overview](#crd-architecture-overview)
- [ValkeyNode CRD (Internal)](#valkeynode-crd-internal)
- [Valkey CRD (Standalone/Replicated)](#valkey-crd-standalonereplicated)
- [ValkeyPool CRD (Client-Side Sharding)](#valkeypool-crd-client-side-sharding)
- [ValkeyCluster CRD (Server-Side Sharding)](#valkeycluster-crd-server-side-sharding)
- [Sentinel CRD (HA Failover)](#sentinel-crd-ha-failover)
- [Common Configuration Patterns](#common-configuration-patterns)
- [Key Design Decisions](#key-design-decisions)
- [Design Rationale and Rejected Alternatives](#design-rationale-and-rejected-alternatives)
- [Migration Guide](#migration-guide)
- [Future Enhancements](#future-enhancements)
- [References](#references)
- [Appendices](#appendices)
- [Questions](#questions)

---

## Overview

This document aims to outline the design of CRDs that would be required for the valkey-operator. For the 0.1.0 release we have suggested a minimal ValkeyCluster that is open to breaking changes. This document proposes additional CRDs to meet the requirements outlined in the references.

### Valkey deployment methods

Valkey is popular because of the breadth and depth of the data types and workloads it support, which then open it up to several methods of deployment.

- Standalone
    - no replication, just one primary instance
- HA replication,
    - one primary and one or more replicas
- Sentinel-backed HA replication
    - one primary and one or more replicas
    - a separate process (Valkey Sentinel) that has watchdog, healthchecker, and primary discovery capabilities
- Cluster
    - Server-side sharding
    - Horizontally scalable
    - Independent of above deployment methods

### Five-CRD architecture

| CRD | Purpose | User-Facing |
|-----|---------|-------------|
| **ValkeyNode** | Single-pod abstraction | ❌ No (operator-managed) |
| **Valkey** | Standalone/replicated instance | ✅ Yes |
| **ValkeyPool** | Multiple independent instances | ✅ Yes |
| **ValkeyCluster** | Sharded cluster mode | ✅ Yes |
| **Sentinel** | HA monitoring and failover | ✅ Yes |

## Design Goals and Principles

### Primary Goals

1. **User-first approach**: Deploy functional, highly available Valkey clusters with minimal configuration
2. **Kubernetes-native**: Follow established Kubernetes conventions for consistency
3. **Production focus**: Prioritise cluster and replication modes for production workloads
4. **Upgrade paths**: Enable seamless progression from standalone → replicated → HA → sharded

### Design principles

TODO comment to ask if there are any others that should be included

- Progressive enhancement
    - Start simple and upgrade as required
    - No forced migrations between CRD types
        - i.e. `ValkeyStandalone` -> `ValkeyReplicated` -> `ValkeySentinel`
- Explicit over implicit
    - Failover modes explicitly declared
    - No external dependencies
    - Self-documenting specs
- Separation of concerns
    - ValkeyNode handles infrastructure
    - Parent controllers handle topology
    - Sentinel handles failover
- Nested sub-problems
    - Related fields are grouped (`persistence.rdb`, `tls.clusterBus`)
    - Better organisation and extensibility
- Configuration-driven behaviour
    - ValkeyNode behaviour is determined by config fields
    - Not coupled to parent CRD type
    - Generic and reusable

## CRD Architecture Overview

### Resource Hierarchy

```
User Creates:
├─ ValkeyPool
│  └─ Creates: Multiple Valkey resources
│     └─ Creates: ValkeyNode per pod
│        └─ Creates: Deployment/StatefulSet + Service + PVC
│
├─ Valkey
│  └─ Creates: ValkeyNode per pod
│     └─ Creates: Deployment/StatefulSet + Service + PVC
│
├─ ValkeyCluster
│  └─ Creates: ValkeyNode per pod
│     └─ Creates: Deployment/StatefulSet + Service + PVC
│
└─ Sentinel
   └─ Creates: ValkeyNode per Sentinel pod
      └─ Creates: Deployment/StatefulSet + Service
```

### Controller Responsibilities

| Controller        | Responsibilities                                                                                                               |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| **ValkeyNode**    | Create/manage Deployment or StatefulSet (replicas=1), Service, PVC; Configure Valkey/Sentinel; Update status                   |
| **Valkey**        | Create ValkeyNodes based on replicas; Configure replication topology; Register with Sentinel; Handle operator-managed failover |
| **ValkeyPool**    | Create child Valkey resources; Inject AZ affinity; Handle shard scaling; Aggregate status                                      |
| **ValkeyCluster** | Create ValkeyNodes for shards; Assign hash slots; Initialize cluster; Handle slot migration; Inject AZ affinity                |
| **Sentinel**      | Create ValkeyNodes for Sentinels; Watch monitoredInstances; Configure Sentinel monitoring; Handle Sentinel scaling             |
## ValkeyNode CRD (Internal)

ValkeyNode is the operator-managed abstraction for single-pod deployments. **Users never create these directly** - they're created by parent controllers (Valkey, ValkeyCluster, Sentinel).

### Design Philosophy

**Deliberately "dumb"** - ValkeyNode reconciles infrastructure without making topology decisions:

- Creates singleton Deployment or StatefulSet depending on what the user has requested
- Creates Service (stable DNS per pod)
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
  name: myvalkey-0
  namespace: default
  ownerReferences:  # Set by parent controller
    - apiVersion: valkey.io/v1alpha1
      kind: Valkey
      name: myvalkey
      controller: true
  # Pod management strategy
  # +kubebuilder:validation:Enum=deployment;statefulset
  # +kubebuilder:default=statefulset
  podManagementType: statefulset

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
    enabled: true
    size: 10Gi
    storageClassName: gp3

  # Service configuration
  service:
    type: ClusterIP
    annotations: {}

  # Valkey-specific configuration (only when type=valkey)
  valkeyConfig:
    # Cluster configuration
    cluster:
      enabled: false
      slots: []  # e.g., ["0-5460"]    
    # ... other configurations

status:
  conditions: []
  podName: myvalkey-0-xyz
  podIP: 10.0.1.5
  serviceName: myvalkey-0
  serviceIP: 10.0.1.10
  pvcName: myvalkey-0-data
  managedResourceName: myvalkey-0-sts
  managedResourceKind: StatefulSet

  # Observed replication state (queried via INFO replication)
  observedRole: primary  # "primary" or "replica"
  observedReplicaOf: ""

  ready: true
```

### Replication and Failover Semantics

TODO these semantics needs to be finalised

**Status tracks observed reality:**

- `observedRole` and `observedReplicaOf` queried from `INFO replication`
- During failovers, status may temporarily differ until reconciliation
#### Sentinel-Managed Failover

1. Sentinel detects failure and promotes a replica
2. Sentinel reconfigures Valkey directly (`REPLICAOF` commands)
3. ValkeyNode controllers observe change (query `INFO replication`)
4. Parent Valkey controller detects topology change

#### Operator-Managed Failover

1. Valkey controller detects failure
2. Valkey controller selects replica to promote
3. Valkey controller updates ValkeyNode specs
4. Valkey (or ValkeyNode?) controller run `REPLICAOF` / `FAILOVER` commands
5. ValkeyNode controller update status

## Valkey CRD (Standalone/Replicated)

Valkey manages standalone and replicated instances with a natural upgrade path from simple to complex deployments.

Given the similarities between ValkeyStandalone, ValkeyReplicated, ValkeySentinel, combining these into one CRD will simplify the CRDs in consideration for the end user. The trade-off for this is complexity in the code base, and a few implicit assumptions abstracted out by `spec.replicas`.

### Specification

```yaml
apiVersion: valkey.io/v1alpha1
kind: Valkey
metadata:
  name: myvalkey
spec:
  # Valkey version/image
  image: valkey/valkey:8.0

  # Total number of pods (Kubernetes-native semantic)
  # 0 = suspended (no pods, PVCs retained)
  # 1 = standalone (1 primary, no replication)
  # ≥2 = replication (1 primary + N replicas where N = replicas - 1)
  # +kubebuilder:validation:Minimum=0
  # +kubebuilder:default=1
  replicas: 1

  # Reference to Sentinel for HA failover (optional)
  # If set: Sentinel manages failover
  # If unset: Operator manages failover
  sentinel:
    name: my-sentinel

    # Per-instance Sentinel config (overrides Sentinel defaults)
    config:
      downAfterMilliseconds: "10000"
      failoverTimeout: "180000"
      parallelSyncs: "1"

  # Resource requirements
  resources:
    requests:
      memory: "256Mi"
      cpu: "100m"
    limits:
      memory: "1Gi"
      cpu: "500m"

  # Persistent storage configuration
  persistence:
    enabled: true
    size: 10Gi
    storageClassName: gp3

    # Valkey RDB snapshots
    rdb:
      enabled: true
      savePolicy:
        - seconds: 900
          changes: 1
        - seconds: 300
          changes: 10

    # Valkey AOF (append-only file)
    aof:
      enabled: true
      fsync: "everysec"  # always | everysec | no

  # TLS configuration
  tls:
    enabled: false
    certificateRef:
      name: valkey-tls
    certKey: tls.crt
    keyKey: tls.key
    caKey: ca.crt
    clientAuth: require  # none | request | require

  # Authentication
  auth:
    # Legacy password mode
    password:
      secret:
        name: valkey-password
        key: password

    # Modern ACL mode
    acl:
      enabled: true
      users:
        - name: default
          passwordSecret:
            name: valkey-default-pw
            key: password
          permissions: "+@all ~*"
        - name: readonly
          passwordSecret:
            name: valkey-readonly-pw
            key: password
          permissions: "+@read ~*"

  # Pod scheduling
  nodeSelector: {}
  affinity: {}
  tolerations: []

  # Pod management strategy
  podManagementType: statefulset

  # Metrics exporter sidecar
  exporter:
    enabled: true
    image: oliver006/redis_exporter:latest
    resources: {}

  # Custom Valkey configuration
  config:
    maxmemory: "512mb"
    maxmemory-policy: "allkeys-lru"

status:
  state: Ready  # Initializing | Ready | Degraded | Suspended | Failed

  # Current topology
  primary: myvalkey-0
  replicas:
    - myvalkey-1
    - myvalkey-2

  readyReplicas: 3

  # Sentinel association
  sentinelManaged: true
  sentinelName: my-sentinel

  conditions: []

  # ValkeyNode references
  nodes:
    - name: myvalkey-0
      role: primary
      ready: true
    - name: myvalkey-1
      role: replica
      ready: true
    - name: myvalkey-2
      role: replica
      ready: true
```

### Replicas Semantics (Kubernetes-Native)

Following standard Kubernetes convention where `spec.replicas` = total number of pods:

| spec.replicas | Behavior | Pods Created | Use Case |
|---------------|----------|--------------|----------|
| 0 | Suspended | 0 (PVCs retained) | Cost savings, temporary pause |
| 1 | Standalone | 1 primary | Development, testing, simple cache |
| 2 | Replication | 1 primary + 1 replica | Basic HA |
| 3 | Replication | 1 primary + 2 replicas | Production HA |

**Pod naming:** `<valkey-name>-<index>` (e.g., `myvalkey-0`, `myvalkey-1`)

- Pod 0 is always the initial primary
    - and desired primary if AZ awareness is enabled
- Pods 1..N are replicas

### Upgrade Path Examples

#### Standalone → Replication

```yaml
# Start with standalone
spec:
  replicas: 1

# Scale to replication
spec:
  replicas: 3  # 1 primary + 2 replicas
```

#### Replication → Sentinel-Managed

```yaml
# Operator-managed replication
spec:
  replicas: 3

# Add Sentinel for HA
spec:
  replicas: 3
  sentinel:
    name: production-sentinel
```

#### Suspend for Cost Savings

```yaml
# Running instance
spec:
  replicas: 3

# Suspend
spec:
  replicas: 0  # All pods removed, PVCs retained

# Resume
spec:
  replicas: 3  # Pods recreate with same data
```

### Validation Rules

```go
func (v *Valkey) ValidateCreate() error {
  // Sentinel requires replication
  if v.Spec.Sentinel != nil && v.Spec.Replicas < 2 {
    return errors.New("sentinel requires replicas >= 2 (primary + replica)")
  }
  return nil
}
```

## ValkeyPool CRD (Client-Side Sharding)

ValkeyPool manages multiple independent Valkey instances (shards) for horizontal scaling with client-side sharding.

The ValkeyPool controller will orchestrate scaling events, pod lifecycling, and topology requirements for the pool.

### Specification

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyPool
metadata:
  name: mypool
spec:
  # Number of independent Valkey instances (shards)
  # +kubebuilder:validation:Minimum=0
  # +kubebuilder:default=3
  shards: 3

  # Template for Valkey instances (all shards share this config)
  template:
    image: valkey/valkey:8.0

    replicas: 3  # Each shard: 1 primary + 2 replicas

    sentinel:
      name: pool-sentinel
      config:
        downAfterMilliseconds: "30000"

    persistence:
      enabled: true
      size: 10Gi
      storageClassName: gp3
      rdb:
        enabled: true
      aof:
        enabled: true
        fsync: "everysec"

    resources:
      requests:
        memory: "512Mi"
        cpu: "200m"
      limits:
        memory: "2Gi"
        cpu: "1000m"

    nodeSelector: {}
    affinity: {}
    tolerations: []

    podManagementType: statefulset

    exporter:
      enabled: true

    tls:
      enabled: false

    config:
      maxmemory: "1gb"
      maxmemory-policy: "allkeys-lru"

  # Availability zone distribution for primaries
  azDistribution:
    # List of availability zones (round-robin placement)
    zones:
      - us-east-1a
      - us-east-1b
      - us-east-1c

    # Node label key for zone selection
    nodeLabel: "topology.kubernetes.io/zone"

    # Replica placement strategy
    replicaStrategy: anti-affinity  # or spread

status:
  state: Ready  # Initializing | Ready | Degraded | Failed

  totalShards: 3
  readyShards: 3

  # Per-shard summary
  shards:
    - name: mypool-0
      zone: us-east-1a
      state: Ready
      readyReplicas: 3
    - name: mypool-1
      zone: us-east-1b
      state: Ready
      readyReplicas: 3
    - name: mypool-2
      zone: us-east-1c
      state: Ready
      readyReplicas: 3

  conditions: []
```

### Child Resource Naming

ValkeyPool creates child Valkey resources with predictable names:
- Pattern: `<pool-name>-<shard-index>`
- Examples: `mypool-0`, `mypool-1`, `mypool-2`

Each child Valkey resource:
- Has `ownerReferences` pointing to ValkeyPool
- Inherits template spec from ValkeyPool
- Gets AZ-specific affinity injected based on `azDistribution`

### AZ Distribution Semantics

Allow the user to define what AZs the master of each shard should be located in. If omitted, then sensible defaults are applied (soft anti-affinity).

**Primary placement (round-robin):**
```
Shard 0 primary → zones[0 % len(zones)] = zones[0] = us-east-1a
Shard 1 primary → zones[1 % len(zones)] = zones[1] = us-east-1b
Shard 2 primary → zones[2 % len(zones)] = zones[2] = us-east-1c
Shard 3 primary → zones[3 % len(zones)] = zones[0] = us-east-1a
```

**Replica placement (anti-affinity):**
Replicas prefer zones OTHER than their primary's zone:
```
Shard 0: primary in us-east-1a → replicas prefer us-east-1b, us-east-1c
Shard 1: primary in us-east-1b → replicas prefer us-east-1a, us-east-1c
Shard 2: primary in us-east-1c → replicas prefer us-east-1a, us-east-1b
```

**Cost optimisation:** Reduces cross-AZ data transfer costs while maintaining HA.

### Scaling Examples

#### Scale up

```yaml
spec:
  shards: 3  # Currently 3 shards

# Scale to 5 shards
spec:
  shards: 5  # Creates mypool-3 and mypool-4
```

#### Scale down

```yaml
spec:
  shards: 5  # Currently 5 shards

# Scale to 3 shards
spec:
  shards: 3  # Deletes mypool-4, mypool-3 (reverse order)
```

**⚠️ Data loss warning**: Scaling down permanently deletes data. Users must manually migrate keys before scaling down.

### Use Case: Client-Side Sharding

ValkeyPool would provide a means of easily scaling a number of independent instances that share the same configuration. This is useful in larger deployments where Valkey is used as the Sidekiq datastore, which is incompatible with Valkey Cluster.

**ValkeyPool vs ValkeyCluster:**

- **ValkeyPool**: Client-side sharding, independent instances, simpler
- **ValkeyCluster**: Server-side sharding, distributed hash slots, Valkey Cluster protocol

## ValkeyCluster CRD (Server-Side Sharding)

ValkeyCluster manages sharded Valkey Cluster mode with distributed hash slots (server-side sharding).

### Breaking Changes (Pre-0.1.0)

**Important:** Making breaking changes for consistency before first release.

| Old Semantic | New Semantic | Reason |
|--------------|--------------|--------|
| `replicas` = replicas per shard (excluding primary) | `replicas` = **total pods per shard** | Consistent with K8s conventions |

**Migration formula:** `new_replicas = old_replicas + 1`

Rationale: allow shards to be scaled to 0, otherwise we would always have the primary up. Users may have maintenance that they need to perform.

### Specification

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: mycluster
spec:
  # Valkey version
  image: valkey/valkey:8.0

  # Number of primary shards (minimum 3 for Valkey Cluster)
  # +kubebuilder:validation:Minimum=3
  # +kubebuilder:default=3
  shards: 3

  # Total pods per shard (primary + replicas)
  # 0 = suspended (no pods, PVCs retained)
  # 1 = no replication (NOT RECOMMENDED for production)
  # ≥2 = replication (1 primary + N replicas)
  # +kubebuilder:validation:Minimum=0
  # +kubebuilder:default=3
  replicas: 3

  # Resource requirements
  resources:
    requests:
      memory: "512Mi"
      cpu: "200m"
    limits:
      memory: "2Gi"
      cpu: "1000m"

  # Persistent storage
  persistence:
    enabled: true
    size: 20Gi
    storageClassName: gp3
    rdb:
      enabled: true
    aof:
      enabled: false

  # Cluster-specific configuration
  cluster:
    nodeTimeout: "15s"
    replicaReadOnly: true
    allowReadsWhenDown: false
    migrationBarrier: 1

  # AZ distribution
  azDistribution:
    zones:
      - us-east-1a
      - us-east-1b
      - us-east-1c
    nodeLabel: "topology.kubernetes.io/zone"
    replicaStrategy: anti-affinity

  # Pod scheduling
  nodeSelector: {}
  affinity: {}
  tolerations: []

  podManagementType: statefulset

  # Metrics exporter
  exporter:
    enabled: true

  # TLS configuration
  tls:
    enabled: false
    certificateRef:
      name: valkey-cluster-tls

  # Custom configuration
  config:
    cluster-node-timeout: "15000"
    cluster-replica-validity-factor: "10"

status:
  state: Ready  # Initializing | Reconciling | Ready | Degraded | Suspended | Failed
  reason: ClusterHealthy
  message: "All shards healthy, all slots assigned"

  totalShards: 3
  readyShards: 3
  totalReplicas: 9  # shards × replicas
  readyReplicas: 9

  slotsAssigned: 16384
  slotsUnassigned: 0

  shards:
    - shardIndex: 0
      primary: mycluster-0-0
      replicas: [mycluster-0-1, mycluster-0-2]
      slots: "0-5461"
      zone: us-east-1a
      ready: true
    - shardIndex: 1
      primary: mycluster-1-0
      replicas: [mycluster-1-1, mycluster-1-2]
      slots: "5462-10922"
      zone: us-east-1b
      ready: true
    - shardIndex: 2
      primary: mycluster-2-0
      replicas: [mycluster-2-1, mycluster-2-2]
      slots: "10923-16383"
      zone: us-east-1c
      ready: true

  conditions: []
```

### Naming Convention

**ValkeyNode naming:** `<cluster-name>-<shard-index>-<replica-index>`

Examples:
- `mycluster-0-0`: Shard 0, replica index 0 (primary)
- `mycluster-0-1`: Shard 0, replica index 1 (first replica)
- `mycluster-1-0`: Shard 1, replica index 0 (primary)
- `mycluster-2-2`: Shard 2, replica index 2 (second replica)

**Service naming:** `<valkeynode-name>`

### Slot Distribution

Valkey Cluster uses **16384 hash slots** distributed across primaries:

**For 3 shards:**
- Shard 0: slots 0-5461 (5462 slots)
- Shard 1: slots 5462-10922 (5461 slots)
- Shard 2: slots 10923-16383 (5461 slots)

**Formula:**
```go
slotsPerShard := 16384 / shards
for i := 0; i < shards; i++ {
  start := i * slotsPerShard
  end := start + slotsPerShard - 1
  if i == shards-1 {
    end = 16383  // Last shard gets remainder
  }
  assignSlots(shard[i], start, end)
}
```

The controller will manage slot migrations on scaling.

### Validation Rules

```go
func (v *ValkeyCluster) ValidateCreate() error {
  // Valkey Cluster protocol requires minimum 3 primaries
  if v.Spec.Shards < 3 {
    return errors.New("Valkey Cluster requires at least 3 shards")
  }
  return nil
}
```

## Sentinel CRD (HA Failover)

Sentinel provides high-availability monitoring and automatic failover for Valkey replication instances. **Note:** Sentinel is only used with Valkey CRD, not ValkeyCluster (which has built-in cluster failover).

### Specification

```yaml
apiVersion: valkey.io/v1alpha1
kind: Sentinel
metadata:
  name: production-sentinel
spec:
  # Sentinel image
  image: valkey/valkey:8.0

  # Number of Sentinel instances
  # Minimum 3 recommended for proper quorum
  # Odd numbers preferred (3, 5, 7) to avoid split-brain
  # +kubebuilder:validation:Minimum=1
  # +kubebuilder:default=3
  replicas: 3

  # Quorum for failover decisions
  # Number of Sentinels that must agree to initiate failover
  # Defaults to: (replicas / 2) + 1
  # +kubebuilder:validation:Minimum=1
  # quorum: 2

  # Resource requirements
  resources:
    requests:
      memory: "128Mi"
      cpu: "100m"
    limits:
      memory: "256Mi"
      cpu: "200m"

  # Pod scheduling
  nodeSelector: {}
  
  # Default soft anti-affinity provided on spreads
  affinity: {}
  tolerations: []

  # Default Sentinel config (fallback if Valkey doesn't specify on itself)
  defaultConfig:
    downAfterMilliseconds: "30000"
    failoverTimeout: "180000"
    parallelSyncs: "1"

  # TLS configuration
  tls:
    enabled: false
    certificateRef:
      name: sentinel-tls

status:
  state: Ready  # Initializing | Ready | Degraded | Failed

  totalReplicas: 3
  readyReplicas: 3

  # Monitored Valkey instances
  monitoredInstances:
    - name: prod-cache
      namespace: default
      primary: prod-cache-0
      replicas: 4
      monitoring: true
      sentinelConfig:
        downAfterMilliseconds: "5000"
        failoverTimeout: "60000"
        parallelSyncs: "1"
      lastFailover: "2026-01-22T10:30:00Z"

    - name: dev-cache
      namespace: default
      primary: dev-cache-0
      replicas: 1
      monitoring: true
      sentinelConfig:
        downAfterMilliseconds: "30000"
        failoverTimeout: "180000"
        parallelSyncs: "1"

  # Sentinel node details
  sentinels:
    - name: production-sentinel-0
      ready: true
      ip: 10.0.1.10
    - name: production-sentinel-1
      ready: true
      ip: 10.0.1.11
    - name: production-sentinel-2
      ready: true
      ip: 10.0.1.12

  conditions: []
```

### Discovery Mechanism: Push Model

Sentinel discovers which Valkey instances to monitor via **push model**:

1. **Valkey specifies sentinelRef** in its spec
2. **Valkey controller registers with Sentinel**: Updates `Sentinel.status.monitoredInstances`
3. **Sentinel controller configures monitoring**: Runs `SENTINEL MONITOR` commands

### Per-Instance Sentinel Configuration

**Key Design Decision:** Sentinel monitoring config lives in `Valkey.spec.sentinel.config`, not in Sentinel CRD.

**Rationale:**
- Different Valkey instances have different SLA requirements
- One Sentinel can monitor many instances with different configs
- Clear ownership: Valkey spec contains its monitoring requirements
- Flexible: adjust per-instance without affecting others

**Configuration precedence:**
1. `Valkey.spec.sentinel.config` (highest - per-instance override)
2. `Sentinel.spec.defaultConfig` (middle - Sentinel-wide default)
3. Hardcoded Sentinel defaults (lowest - if nothing specified)

### Example: One Sentinel, Multiple Instances

```yaml
---
# Shared Sentinel infrastructure
apiVersion: valkey.io/v1alpha1
kind: Sentinel
metadata:
  name: shared-sentinel
spec:
  replicas: 3
  quorum: 2
  defaultConfig:
    downAfterMilliseconds: "30000"

---
# Production: Fast failover
apiVersion: valkey.io/v1alpha1
kind: Valkey
metadata:
  name: prod-cache
spec:
  replicas: 5
  sentinel:
    name: shared-sentinel
    config:
      downAfterMilliseconds: "5000"  # Override for fast failover

---
# Dev: Uses defaults
apiVersion: valkey.io/v1alpha1
kind: Valkey
metadata:
  name: dev-cache
spec:
  replicas: 2
  sentinel:
    name: shared-sentinel
    # No config override - uses defaultConfig (30000ms)
```

### Validation Rules

```go
func (s *Sentinel) ValidateCreate() error {
  if s.Spec.Replicas < 1 {
    return errors.New("replicas must be >= 1")
  }

  // Warn if even number
  if s.Spec.Replicas % 2 == 0 {
    // Log warning: "Odd numbers (3, 5, 7) recommended"
  }

  if s.Spec.Quorum < 1 || s.Spec.Quorum > int32(s.Spec.Replicas) {
    return errors.New("quorum must be between 1 and replicas")
  }

  return nil
}
```

## Common Configuration Patterns

All user-facing CRDs share similar configuration patterns for consistency.

### Persistence Configuration

```yaml
spec:
  persistence:
    enabled: true
    size: 10Gi
    storageClassName: gp3

    # Valkey RDB snapshots
    rdb:
      enabled: true
      savePolicy:
        - seconds: 900
          changes: 1
        - seconds: 300
          changes: 10
        - seconds: 60
          changes: 10000
      compression: true

    # Valkey AOF (append-only file)
    aof:
      enabled: false
      fsync: everysec  # always | everysec | no
      rewritePercentage: 100
      rewriteMinSize: 64mb
```

**Design decisions:**
- Nested structure for clarity
- Both Valkey and Kubernetes volume config
- Operator validates: warns if RDB/AOF enabled but volume disabled
- PVCs created separately (not StatefulSet volumeClaimTemplate)

### TLS Configuration

```yaml
spec:
  tls:
    enabled: true

    # Secret reference (no cert-manager dependency)
    certificateRef:
      name: valkey-tls

    # Keys within the secret
    certKey: tls.crt
    keyKey: tls.key
    caKey: ca.crt

    # mTLS configuration
    clientAuth: require  # none | request | require

    # Protocol settings
    minVersion: TLS12
    protocols:
      - TLSv1.2
      - TLSv1.3

    # Valkey-specific
    replication: true      # TLS for primary-replica traffic
    clusterBus: true       # TLS for cluster bus (ValkeyCluster only)
```

**Design decisions:**
- Follows prometheus-operator pattern
- Works with any Secret source
- No external dependencies
- Granular control over internal traffic encryption

### Authentication Configuration

```yaml
spec:
  auth:
    # Legacy password mode (requirepass)
    password:
      secret:
        name: valkey-password
        key: password

    # Modern ACL mode
    acl:
      enabled: true
      # +listType=map
      # +listMapKey=name
      users:
        - name: default
          passwordSecret:
            name: valkey-default-pw
            key: password
          permissions: "+@all ~*"

        - name: readonly
          passwordSecret:
            name: valkey-readonly-pw
            key: password
          permissions: "+@read ~*"

        - name: app
          passwordSecret:
            name: app-credentials
            key: valkey-password
          permissions: "+@all -@admin ~app:*"
```

**Design decisions:**
- Both modes can coexist (migration path)
- All passwords via Secret references
- GitOps-friendly with `+listMapKey=name`
- Standard Valkey ACL syntax

### Custom Configuration

```yaml
spec:
  # Untyped config for settings not explicitly modeled
  # +kubebuilder:pruning:PreserveUnknownFields
  config:
    maxmemory: 2gb
    maxmemory-policy: allkeys-lru
    timeout: 300
    tcp-keepalive: 60
    slowlog-log-slower-than: 10000
```

**Merge order (later wins):**
1. Operator defaults
2. Explicitly modeled fields (`persistence.rdb.*`, `tls.*`, `auth.*`)
3. `spec.config` inline map

**Design decisions:**
- Uses `map[string]interface{}`
- Inline only (no ConfigMap references)
- Escape hatch for advanced users

### Common Fields

All user-facing CRDs share these fields:

```yaml
spec:
  # Container
  image: valkey/valkey:8.0

  # Resources
  resources:
    requests:
      memory: "256Mi"
      cpu: "100m"
    limits:
      memory: "1Gi"
      cpu: "500m"

  # Scheduling
  nodeSelector: {}
  affinity: {}
  tolerations: []

  # Pod management
  podManagementType: statefulset  # or deployment

  # Metrics exporter
  exporter:
    enabled: true
    image: oliver006/redis_exporter:latest
    resources: {}
    port: 9121

  # TLS
  tls:
    enabled: false

  # Auth
  auth:
    password: {}
    acl: {}

  # Custom config
  config: {}
```

### Status Patterns

All CRDs follow consistent status patterns:

```yaml
status:
  # High-level state
  state: Ready  # Enum varies by CRD
  reason: "..."
  message: "..."

  # Standard Kubernetes conditions
  # +listType=map
  # +listMapKey=type
  conditions:
    - type: Ready
      status: "True"
      reason: AllNodesHealthy
      message: "..."
      lastTransitionTime: "2026-01-22T10:30:00Z"
      observedGeneration: 3
```

## Key Design Decisions

### Replicas Semantic - Kubernetes-Native

**Decision:** `spec.replicas` = **total number of pods**

**Rationale:**
- Consistent with Kubernetes Deployment/StatefulSet
- Intuitive scaling behavior
- Clear: `replicas: 0` = suspended, `replicas: 1` = standalone
- Eliminates "excluding primary" confusion

**Applied to:**
- `Valkey.spec.replicas` = total pods
- `ValkeyCluster.spec.replicas` = total pods per shard
- `ValkeyPool.spec.template.replicas` = total pods per shard
- `Sentinel.spec.replicas` = total Sentinel pods

**Total pod calculation:**
- **Valkey**: `replicas`
- **ValkeyPool**: `shards × template.replicas`
- **ValkeyCluster**: `shards × replicas`
- **Sentinel**: `replicas`

### ValkeyNode - Generic Pod Abstraction

**Decision:** ValkeyNode is operator-managed, abstracts out the concrete implementation of StatefulSet vs Deployment.

**Rationale:**
- Single abstraction for all pods
- Configuration-driven behaviour
- Deliberately "dumb" - no topology decisions
- Reusable and maintainable
- If we want to directly manage pods, this interface abstracts that too

**Characteristics:**
- Singleton workloads (replicas=1)
    - Maximum scheduling control
- Service per pod (if required)
- PVC managed separately (if required)

### ValkeyNode - Singleton Deployments/StatefulSets

**Decision:** Each ValkeyNode creates one workload with `replicas: 1`.

**Rationale:**
- Maximum scheduling control
- Enables AZ pinning
- Faster rollouts
- No ordered rollout constraints

### Nested Sub-Problems

**Decision:** Use nested config structures, not CamelCase concatenation.

**Examples:**
- `persistence.rdb.enabled`, `persistence.aof.fsync`
- `tls.clusterBus.enabled`, `tls.clientAuth`
- `cluster.enabled`, `cluster.slots`

**Rationale:**
- Kubernetes CRD best practices
- Clear scope and validation
- Better extensibility

### Multiple Focused CRDs

**Decision:** Separate CRDs for different use cases.

**Rationale:**
- Clear intent
- Easier validation
- Better UX
- Evolvable independently

### Sentinel Discovery - Push Model

**Decision:** Valkey explicitly references Sentinel via `spec.sentinel.name`.

**Rationale:**
- Self-documenting
- Explicit opt-in
- Clear ownership
- Supports per-instance config

**Rejected alternatives:**
- Label selector discovery (implicit, ambiguous)
- SentinelMonitor CRD (extra complexity)

### Per-Instance Sentinel Configuration

**Decision:** Sentinel config lives in `Valkey.spec.sentinel.config`.

**Rationale:**
- Different instances have different SLAs
- One Sentinel monitors many instances
- Clear ownership
- Flexible per-instance tuning

**Precedence:**
1. `Valkey.spec.sentinel.config` (per-instance)
2. `Sentinel.spec.defaultConfig` (default)
3. Hardcoded defaults

### Operator Creates PVCs Separately

**Decision:** Operator creates PVC, then references in pod spec.

**Rationale:**
- Avoids StatefulSet volumeClaimTemplate immutability
- Operator can modify PVC specs
- Volume expansion possible

### Service Per Pod

**Decision:** Each ValkeyNode gets its own Service if enabled.

**Rationale:**
- Stable network identity
- Prevents stale replica issues
- Required for `replica-announce-ip`
- Clean lifecycle

### AZ Distribution

**Decision:** Primaries round-robin, replicas anti-affinity from primary.

**Rationale:**
- Reduces cross-AZ costs
- HA: replicas survive primary zone failure
- Deterministic placement

**Implementation:**
- Shard N primary → `zones[N % len(zones)]`
- Replicas prefer different zones

### TLS via Secret References

**Decision:** TLS uses Secret references, no cert-manager dependency.

**Rationale:**
- Follows prometheus-operator pattern
- Works with any Secret source
- No external dependencies
- Portable

### Authentication - Dual Mode

**Decision:** Support password and ACL simultaneously.

**Rationale:**
- Migration path
- Both coexist during transition
- Flexibility

### 14. Custom Configuration - Inline Only

**Decision:** Untyped `config` map for unmapped Valkey settings.

**Rationale:**
- Escape hatch
- Simpler than ConfigMap refs
- GitOps-friendly

---



## Design Rationale and Rejected Alternatives

### Replicas Semantic

**Chosen:** `replicas` = total pods

**Rejected:** `replicas` = replicas excluding primary

**Rationale:**
- Kubernetes-native convention
- More intuitive
- Clear suspend behavior (`replicas: 0`)

### Sentinel Discovery

**Chosen:** Push model (Valkey references Sentinel)

**Rejected:** Pull model (Sentinel selects Valkey via labels)

**Rationale:**
- Self-documenting
- No ambiguity
- Clear ownership
- Supports per-instance config

### Sentinel Configuration Location

**Chosen:** Config in `Valkey.spec.sentinel.config`

**Rejected:** Config in `Sentinel.spec` or separate SentinelMonitor CRD

**Rationale:**
- Different instances need different SLAs
- Single source of truth
- Clear ownership

### ValkeyNode Approach

**Chosen:** Internal CRD with generic abstraction

**Rejected:** Direct pod management or parent-specific pod types

**Rationale:**
- Single abstraction
- Configuration-driven
- Reusable
- Maintainable

### Workload Type

**Chosen:** Singleton Deployment/StatefulSet per pod

**Rejected:** Multi-replica StatefulSet or single large Deployment

**Rationale:**
- Maximum scheduling control
- AZ pinning
- Faster rollouts
- Cleaner lifecycle

### PVC Management

**Chosen:** Operator creates PVCs separately

**Rejected:** StatefulSet volumeClaimTemplate

**Rationale:**
- Mutability
- Operator control
- Volume expansion

### TLS Approach

**Chosen:** Secret references

**Rejected:** cert-manager CRD integration

**Rationale:**
- No dependencies
- Works with any Secret source
- Portable

### CRD Structure

**Chosen:** Multiple focused CRDs

**Rejected:** Single unified CRD with mode field

**Rationale:**
- Clear intent
- Easier validation
- Better UX
- Independent evolution

## Migration Guide

### Upgrade Paths

#### Standalone → Replication

```yaml
# 1. Start with standalone
kind: Valkey
spec:
  replicas: 1

# 2. Add replication
spec:
  replicas: 3
```

#### Replication → Sentinel-Managed

```yaml
# 1. Replication
kind: Valkey
spec:
  replicas: 3

# 2. Create sentinel
kind: Sentinel
metadata:
  name: production-sentinel
spec:
  # ...

# 3. Add Sentinel to Valkey
kind: Valkey
spec:
  replicas: 3
  sentinel:
    name: production-sentinel
```

#### Horizontal Scaling with Pool

```yaml
# 1. Scale horizontally
kind: ValkeyPool
spec:
  # shards: 3 <- old value
  shards: 5
  template:
    replicas: 3
    sentinel:
      name: production-sentinel
```

Data resharding is performed on the client.

Open to future enhancements to support resharding provided by the Operator.

#### Upgrade to Cluster

```yaml
# Data migration required - no automatic upgrade path
# From: Valkey or ValkeyPool
# To: ValkeyCluster

kind: ValkeyCluster
spec:
  shards: 3
  replicas: 3
```

**Note:** ValkeyCluster ↔ Valkey migration requires data migration (architecturally different).

## Future Enhancements

### Enhanced Replica Placement

Support explicit per-replica zone placement:

```yaml
azDistribution:
  primaryZone: us-east-1a
  replicaZones:
    - us-east-1b
    - us-east-1c
```

**Current:** Anti-affinity provides soft preference
**Future:** Required affinity per replica

### Configuration Live Reload

- Detect which configs require pod restart
- Apply mutable configs live via `CONFIG SET`

### Backup/Restore Integration

- ValkeyBackup CRD?
- Cloud storage integration (S3, GCS, Azure)
- Point-in-time recovery

### Advanced Monitoring

- PrometheusRule generation
- Custom dashboards
- SLO/SLI tracking

### Multi-Cluster Federation

- Cross-cluster replication
- Disaster recovery

### External Access

- LoadBalancer/Ingress configuration
- External DNS integration
- Proxy support

### Pod Disruption Budgets

- Automatic PDB creation
- Per-shard and per-cluster/pool availability guarantees
- Safe node drains

### Configuration References

- ConfigMap references for shared config
- External config sources


## References

### External References

- [Valkey RFC #28](https://github.com/valkey-io/valkey-rfc/pull/28) - Operator requirements
- [Valkey Operator Discussion #19](https://github.com/valkey-io/valkey-operator/discussions/19) - Initial design discussion
- [Valkey Kubernetes Topologies](https://gist.githubusercontent.com/jdheyburn/88c5c67625d784d52cb1245be68a7429/raw/2a82b71b0357461721db118aa12bcf8c3cb044ec/VALKEY_KUBERNETES_TOPOLOGIES.md) - Production use cases
- [Kubernetes CRD Design for the Long Haul](https://kccnceu2025.sched.com/) - ClusterAPI maintainers' best practices
- [Simplify Kubernetes Operator Development With a Modular Design Pattern](https://kccnceu2025.sched.com/) - Operator patterns

### Operator References

- [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator) - TLS and selector patterns
- [MongoDB Kubernetes Operator](https://github.com/mongodb/mongodb-kubernetes-operator) - Configuration patterns
- [Elastic Cloud on Kubernetes](https://github.com/elastic/cloud-on-k8s) - Multi-resource architecture
- [SAP Valkey Operator](https://github.com/sap/valkey-operator) - Existing Valkey operator
- [Hyperspike Valkey Operator](https://github.com/hyperspike/valkey-operator) - Alternative implementation

## Appendices

### Use Case Comparison

| Feature               | Valkey               | ValkeyPool           | ValkeyCluster       | Sentinel          |
| --------------------- | -------------------- | -------------------- | ------------------- | ----------------- |
| **Topology**          | Single instance      | Multiple independent | Cluster with slots  | HA monitor        |
| **Sharding**          | None                 | Client-side          | Server-side         | N/A               |
| **HA Failover**       | Operator or Sentinel | Operator or Sentinel | Built-in            | Provides failover |
| **Use Case**          | Simple cache, dev    | Horizontal scaling   | Production sharding | HA for Valkey     |
| **Min Instances**     | 1 (standalone)       | 1 shard              | 3 shards            | 3 recommended     |
| **Client Type**       | Standard             | Standard             | Cluster-aware       | Sentinel-aware    |
| **Complexity**        | Low                  | Medium               | High                | Low               |
| **Cost Optimization** | N/A                  | AZ-aware             | AZ-aware            | N/A               |

### Total Pod Count Examples

#### Valkey

- `replicas: 1` → **1 pod** (standalone)
- `replicas: 3` → **3 pods** (1 primary + 2 replicas)
- `replicas: 5` → **5 pods** (1 primary + 4 replicas)

#### ValkeyPool

- `shards: 3, template.replicas: 3` → **9 pods** (3 shards × 3 pods)
    - 3 masters, each with 2 replicas
- `shards: 6, template.replicas: 2` → **12 pods** (6 shards × 2 pods)
    - 3 masters, each with 1 replica

#### ValkeyCluster

- `shards: 3, replicas: 3` → **9 pods** (3 shards × 3 pods)
    - 3 masters, each with 2 replicas
- `shards: 6, replicas: 2` → **12 pods** (6 shards × 2 pods)
    - 3 masters, each with 1 replica

#### Sentinel

- `replicas: 3` → **3 pods** (3 Sentinels)
- `replicas: 5` → **5 pods** (5 Sentinels)

---

## Questions

### ValkeyNode to support Sentinel

Should ValkeyNode support Sentinel?

We have ValkeyNode to abstract away the concrete implementation of the pod management type (Deployment vs StatefulSet). Would Sentinel ever be managed by a Deployment? JDH comment: I am used to running it as a StatefulSet with 3 replicas. Adding Sentinel to ValkeyNode may make it complex.

### Migrating from Valkey to ValkeyPool

Is this something we want to support? We can include a process for this in the future.

### Replication configuring

- In our helm-chart backed by Sentinel, Valkey pods have an init container to discover the current master from Sentinel, and sets the `replicaof` config to be that master.
- How would we achieve something similar for Valkey with replication managed by Operator?

There are probably a load of edge cases we would need to consider here too.
