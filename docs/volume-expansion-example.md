# Volume Expansion Example

This example demonstrates how to expand the persistent storage for a ValkeyCluster using online volume expansion.

## Prerequisites

Before attempting volume expansion, ensure:

1. **Storage Class supports expansion**: Your StorageClass must have `allowVolumeExpansion: true`
   ```bash
   kubectl get storageclass <your-storage-class> -o yaml
   ```

2. **Backup your data**: Always backup your data before performing volume expansion

3. **Check underlying storage provider**: Ensure your storage provider (e.g., AWS EBS, GCE PD, Azure Disk) supports online volume expansion

## Initial Cluster with 5Gi Storage

Create a ValkeyCluster with 5Gi of storage:

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkey-expandable
  namespace: default
spec:
  shards: 3
  replicas: 1
  storage:
    enabled: true
    size: "5Gi"
    storageClassName: "standard"  # Use your storage class that supports expansion
```

Apply the manifest:
```bash
kubectl apply -f valkey-cluster-5gi.yaml
```

Wait for the cluster to be ready:
```bash
kubectl wait --for=condition=Ready valkeyCluster/valkey-expandable --timeout=5m
```

## Expanding Storage to 10Gi

To expand the storage, simply update the `spec.storage.size` field:

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkey-expandable
  namespace: default
spec:
  shards: 3
  replicas: 1
  storage:
    enabled: true
    size: "10Gi"  # Changed from 5Gi to 10Gi
    storageClassName: "standard"
```

Apply the updated manifest:
```bash
kubectl apply -f valkey-cluster-10gi.yaml
```

## Monitoring the Expansion

### Check Operator Events
```bash
kubectl get events --sort-by='.lastTimestamp' | grep VolumeExpan
```

You should see events like:
- `VolumeExpanding`: Expansion has been initiated
- `VolumeExpanded`: Expansion completed successfully

### Check PVC Status
```bash
kubectl get pvc -l valkey.io/cluster=valkey-expandable
```

Look for PVCs in `Resizing` state or check conditions:
```bash
kubectl describe pvc <pvc-name>
```

### Check Cluster Status
```bash
kubectl get valkeyCluster valkey-expandable -o jsonpath='{.status.conditions[?(@.type=="Progressing")]}'
```

During expansion, you'll see a condition with reason `VolumeExpanding`.

### Watch Cluster State
```bash
watch kubectl get valkeyCluster valkey-expandable
```

## Important Notes

### ✅ Supported Operations
- **Expanding volumes**: Increase storage size from smaller to larger values
- **Online expansion**: No downtime required (depends on storage provider)
- **Mixed units**: Can change from `5G` to `5Gi` if the size is equivalent

### ❌ Unsupported Operations
- **Shrinking volumes**: Reducing storage size is **not supported** by Kubernetes
- **Changing storage class**: Cannot change the StorageClass of existing PVCs

### Best Practices

1. **One change at a time**: Don't apply other cluster changes simultaneously with volume expansion

2. **Consistent size units**: Use consistent size definitions (`Gi` or `G`)
   - `1Gi` = 1024³ bytes (binary)
   - `1G` = 1000³ bytes (decimal)

3. **Monitor the process**: Watch cluster events and PVC status during expansion

4. **Plan capacity**: Expand proactively before running out of space

5. **Test first**: Test volume expansion in a non-production environment first

## Troubleshooting

### Expansion Fails

If expansion fails, check:

1. **StorageClass configuration**:
   ```bash
   kubectl get sc <storage-class-name> -o yaml | grep allowVolumeExpansion
   ```
   Should be `true`.

2. **Storage provider support**: Verify your cloud provider or storage system supports expansion

3. **PVC conditions**:
   ```bash
   kubectl describe pvc <pvc-name>
   ```
   Look for error messages in the conditions.

4. **Operator logs**:
   ```bash
   kubectl logs -n valkey-operator-system deployment/valkey-operator-controller-manager -f
   ```

### Expansion Stuck

If expansion appears stuck:

1. Check if filesystem resize is pending:
   ```bash
   kubectl get pvc <pvc-name> -o jsonpath='{.status.conditions[?(@.type=="FileSystemResizePending")]}'
   ```

2. Check pod status: The pod may need to be restarted for filesystem expansion:
   ```bash
   kubectl get pods -l valkey.io/cluster=valkey-expandable
   ```

3. Check storage provider: Some providers require manual intervention for certain volume types

## Example: Complete Workflow

```bash
# 1. Create cluster with 5Gi
cat <<EOF | kubectl apply -f -
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkey-expandable
  namespace: default
spec:
  shards: 2
  replicas: 1
  storage:
    enabled: true
    size: "5Gi"
    storageClassName: "standard"
EOF

# 2. Wait for cluster to be ready
kubectl wait --for=condition=Ready valkeyCluster/valkey-expandable --timeout=5m

# 3. Check current PVC sizes
kubectl get pvc -l valkey.io/cluster=valkey-expandable -o custom-columns=NAME:.metadata.name,SIZE:.spec.resources.requests.storage,STATUS:.status.phase

# 4. Expand to 10Gi
kubectl patch valkeyCluster valkey-expandable --type='json' -p='[{"op": "replace", "path": "/spec/storage/size", "value": "10Gi"}]'

# 5. Monitor expansion
watch "kubectl get pvc -l valkey.io/cluster=valkey-expandable -o custom-columns=NAME:.metadata.name,SIZE:.spec.resources.requests.storage,CAPACITY:.status.capacity.storage,CONDITIONS:.status.conditions[*].type"

# 6. Verify expansion complete
kubectl get pvc -l valkey.io/cluster=valkey-expandable -o custom-columns=NAME:.metadata.name,SIZE:.spec.resources.requests.storage,CAPACITY:.status.capacity.storage
```

## Cleanup

To delete the cluster:
```bash
kubectl delete valkeyCluster valkey-expandable
```

Note: PVCs are not automatically deleted. To delete them:
```bash
kubectl delete pvc -l valkey.io/cluster=valkey-expandable
```
