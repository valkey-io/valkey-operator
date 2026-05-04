# Quickstart

Deploy a Valkey Cluster on Kubernetes in under 5 minutes.

## Prerequisites

- Kubernetes cluster v1.31+
- kubectl v1.31+
- Helm 3

## 1. Install the operator

```sh
helm repo add valkey https://valkey.io/valkey-helm
helm repo update
helm install valkey-operator valkey/valkey-operator -n valkey-operator-system --create-namespace --set image.tag=main
```

Verify the operator is running:

```sh
kubectl get pods -n valkey-operator-system
```

## 2. Deploy a Valkey Cluster

Create a 3-shard cluster with 1 replica per shard (6 pods total):

```sh
kubectl apply -f - <<EOF
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: my-cluster
spec:
  shards: 3
  replicas: 1
EOF
```

## 3. Verify the cluster is healthy

Watch the cluster reach `Ready` state:

```sh
kubectl get valkeycluster -w
```

Expected output:

```
NAME         STATE   REASON           AGE
my-cluster   Ready   ClusterHealthy   30s
```

For more detail:

```sh
kubectl get valkeynodes
```

## 4. Connect to the cluster

Exec into a Valkey pod and use the CLI:

```sh
kubectl exec -it $(kubectl get pods -l app.kubernetes.io/name=valkey -o jsonpath='{.items[0].metadata.name}') -- valkey-cli -c
```

Try some commands:

```
127.0.0.1:6379> SET hello world
OK
127.0.0.1:6379> GET hello
"world"
127.0.0.1:6379> CLUSTER INFO
```

## 5. Clean up

```sh
kubectl delete valkeycluster my-cluster
helm uninstall valkey-operator -n valkey-operator-system
```

## Current limitations

- Requires Valkey 9.0+ for scale-out/in support
- No persistent storage
- No cert-manager integration (manual TLS Secret only)
- Config changes trigger a rolling restart (no live reload yet)
- No module support
- No backup/restore
- No default shard-aware anti-affinity (user-configurable via Kubernetes affinity rules)
- No operator-managed external access (LoadBalancer/Ingress)
- API is `v1alpha1` and may change in future releases
- Cluster mode only (no standalone or sentinel)

## Next steps

- [Status conditions](./status-conditions.md) — understanding cluster health
