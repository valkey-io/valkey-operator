# ValkeyNode MVP Design

**Date:** 2026-02-04
**Status:** Approved
**Authors:** jdheyburn, Claude

---

## Overview

ValkeyNode is an internal CRD that abstracts single-pod Valkey deployments. Parent controllers (Valkey, ValkeyCluster, Sentinel) create ValkeyNodes; users don't create them directly.

### Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Implementation approach | Greenfield | Build independently, migrate ValkeyCluster later |
| Scope | Minimal MVP | Validate core abstraction first |
| Testing | Unit + Integration (envtest) | Good coverage without real cluster |
| Ownership enforcement | None for MVP | Simplifies testing |
| Container config | Image + Resources + Scheduling | Essential for realistic deployments |
| Service type | Headless (clusterIP: None) | DNS-based identity survives pod replacement |
| Status fields | Minimal operational | ready, podName, podIP, serviceName, conditions |

---

## Spec Structure

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyNode
metadata:
  name: myvalkey-0
  namespace: default
spec:
  # Required: Valkey container image
  image: valkey/valkey:8.0

  # Optional: Resource requirements
  resources:
    requests:
      memory: "256Mi"
      cpu: "100m"
    limits:
      memory: "1Gi"
      cpu: "500m"

  # Optional: Scheduling constraints
  nodeSelector: {}
  affinity: {}
  tolerations: []
```

### What the controller creates

1. **StatefulSet** (replicas=1) - singleton workload with stable identity
2. **Headless Service** (`clusterIP: None`) - enables stable DNS

### DNS pattern

- Service DNS: `myvalkey-0.default.svc.cluster.local`
- Pod DNS: `myvalkey-0-0.myvalkey-0.default.svc.cluster.local`

### Not included in MVP

- Deployment option (StatefulSet only)
- Persistence/PVC
- TLS configuration
- Cluster mode configuration
- Valkey config injection

---

## Status Structure

```yaml
status:
  # High-level readiness
  ready: true

  # Pod information (observed from StatefulSet's pod)
  podName: myvalkey-0-0
  podIP: 10.0.1.5

  # Service information
  serviceName: myvalkey-0

  # Standard Kubernetes conditions
  conditions:
    - type: Ready
      status: "True"
      reason: PodRunning
      message: "StatefulSet pod is running and ready"
      lastTransitionTime: "2026-02-04T10:30:00Z"
      observedGeneration: 1

    - type: StatefulSetReady
      status: "True"
      reason: ReplicaAvailable
      message: "StatefulSet has 1/1 ready replicas"
      lastTransitionTime: "2026-02-04T10:30:00Z"
      observedGeneration: 1
```

### Condition types

- `Ready` - overall readiness (true when pod is running and passing readiness probe)
- `StatefulSetReady` - StatefulSet has desired replicas available

### Status update flow

1. Controller creates/updates StatefulSet
2. Controller watches StatefulSet status
3. Controller queries pod status (name, IP, ready)
4. Controller updates ValkeyNode status

---

## Controller Reconciliation

```
ValkeyNode Created/Updated
         │
         ▼
┌─────────────────────────┐
│ 1. Ensure Headless      │
│    Service exists       │
└───────────┬─────────────┘
            │
            ▼
┌─────────────────────────┐
│ 2. Ensure StatefulSet   │
│    exists with spec     │
└───────────┬─────────────┘
            │
            ▼
┌─────────────────────────┐
│ 3. Get Pod from         │
│    StatefulSet          │
└───────────┬─────────────┘
            │
            ▼
┌─────────────────────────┐
│ 4. Update ValkeyNode    │
│    status               │
└─────────────────────────┘
```

### Owned resources

Via `controllerutil.SetControllerReference`:
- StatefulSet `<valkeynode-name>`
- Headless Service `<valkeynode-name>`

### Deletion handling

- Kubernetes garbage collection cleans up owned resources when ValkeyNode is deleted
- No custom finalizers needed for MVP

### Requeue behavior

- Requeue after 10s if StatefulSet not ready yet
- No requeue needed once stable

### Watched resources

- ValkeyNode (primary)
- StatefulSet (secondary, for status updates)

---

## Generated Resources

### Headless Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: myvalkey-0
  namespace: default
  ownerReferences: [{...ValkeyNode...}]
  labels:
    app.kubernetes.io/name: valkey
    app.kubernetes.io/instance: myvalkey-0
    app.kubernetes.io/managed-by: valkey-operator
spec:
  clusterIP: None
  selector:
    app.kubernetes.io/instance: myvalkey-0
  ports:
    - name: valkey
      port: 6379
      targetPort: 6379
```

### StatefulSet

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: myvalkey-0
  namespace: default
  ownerReferences: [{...ValkeyNode...}]
  labels:
    app.kubernetes.io/name: valkey
    app.kubernetes.io/instance: myvalkey-0
    app.kubernetes.io/managed-by: valkey-operator
spec:
  serviceName: myvalkey-0
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/instance: myvalkey-0
  template:
    metadata:
      labels:
        app.kubernetes.io/instance: myvalkey-0
    spec:
      containers:
        - name: valkey
          image: valkey/valkey:8.0  # from spec.image
          ports:
            - containerPort: 6379
              name: valkey
          resources: {}  # from spec.resources
          readinessProbe:
            tcpSocket:
              port: 6379
            initialDelaySeconds: 5
            periodSeconds: 5
      nodeSelector: {}     # from spec
      affinity: {}         # from spec
      tolerations: []      # from spec
```

---

## Testing Strategy

### Unit tests

File: `internal/controller/valkeynode_controller_test.go`

- Test helper functions for building StatefulSet/Service specs
- Test label generation
- Test status condition helpers
- Mock client for edge cases (create failures, update conflicts)

### Integration tests

File: `internal/controller/valkeynode_integration_test.go`

Using envtest (fake API server):

1. "creates StatefulSet and Service for new ValkeyNode"
2. "updates StatefulSet when spec.image changes"
3. "updates status when pod becomes ready"
4. "handles ValkeyNode deletion (garbage collection)"
5. "reconciles when StatefulSet is externally modified"
6. "sets degraded condition when StatefulSet fails"

### Test fixtures

- Sample ValkeyNode CRs in `config/samples/v1alpha1_valkeynode.yaml`
- Minimal and full-spec examples

### Not testing in MVP

- E2E with real cluster
- Actual Valkey connectivity
- Persistence/TLS features

---

## File Structure

### New/modified files

```
api/v1alpha1/
├── valkeynode_types.go          # NEW: CRD type definitions
├── zz_generated.deepcopy.go     # REGENERATE: make generate

internal/controller/
├── valkeynode_controller.go     # NEW: Reconciler
├── valkeynode_controller_test.go # NEW: Unit tests
├── valkeynode_resources.go      # NEW: StatefulSet/Service builders
├── suite_test.go                # MODIFY: Register ValkeyNode scheme

config/
├── crd/bases/valkey.io_valkeynodes.yaml  # GENERATED: make manifests
├── samples/v1alpha1_valkeynode.yaml      # NEW: Sample CR
├── rbac/role.yaml               # REGENERATE: make manifests

cmd/main.go                      # MODIFY: Register controller
```

### Implementation order

1. Define types (`valkeynode_types.go`)
2. Run `make generate && make manifests`
3. Implement resource builders (`valkeynode_resources.go`)
4. Implement controller (`valkeynode_controller.go`)
5. Register in `main.go`
6. Write unit tests
7. Write integration tests
8. Add sample CR

---

## Estimated Scope

~500-700 lines of Go code + tests
