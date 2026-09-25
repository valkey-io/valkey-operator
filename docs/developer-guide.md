# Developer guide

## Contributing

Any changes to the CRD API (`api/v1alpha1/`) must be agreed with the core team before implementation.

## Development commands

```sh
make test          # Run unit/integration tests (no cluster needed)
make lint          # Run golangci-lint
make lint-fix      # Run golangci-lint with auto-fix
make fmt           # Run go fmt
make vet           # Run go vet
make build         # Build the manager binary (runs manifests, generate, fmt, vet, lint)
make manifests     # Regenerate CRD manifests and RBAC from controller-runtime markers
make generate      # Regenerate DeepCopy methods
make test-e2e      # Run end-to-end tests in a Kind cluster (creates one if needed, tears it down after)
```

After modifying types in `api/v1alpha1/`, always run `make manifests generate` before testing.

Run a single test package (requires `make test` or `make setup-envtest` to have been run first):

```sh
KUBEBUILDER_ASSETS="$(./bin/setup-envtest use --bin-dir ./bin -p path)" go test ./internal/controller/... -v
```

Filter to a specific Ginkgo spec with `--ginkgo.focus`:

```sh
KUBEBUILDER_ASSETS="$(./bin/setup-envtest use --bin-dir ./bin -p path)" go test ./internal/controller/... -v --ginkgo.focus "spec name"
```

Run a specific e2e test by label:

```sh
TEST_LABELS="<label>" make test-e2e
```

## Keeping the "_operator" ACL in sync

The operator connects to Valkey as a system user called `_operator`, whose ACL is
hand-maintained in `internal/controller/users.go`. If reconciliation code starts
issuing a new command, that ACL has to grant it too, or the operator locks itself
out at runtime.

Rather than trust that ACL to be kept up to date by hand, `hack/aclscan/scan`
statically discovers every Valkey command the operator's reconciliation code
(`cmd/`, `internal/`) actually issues, by scanning its source for valkey-go
client calls — both the builder pattern (`client.B().ClusterInfo()...Build()`)
and raw `Arbitrary(...)` calls. Builder method names are resolved to command
tokens by parsing valkey-go's own generated command builders on disk, so the
mapping tracks whichever valkey-go version the operator is built against
instead of being a second hand-maintained list.

To see the current set of commands the operator issues:

```sh
go run ./hack/aclscan
```

The e2e suite (`test/e2e/valkeycluster_test.go`, "validating the _operator ACL
covers every command the operator runs") uses the same package to check this
list against a live cluster: for every discovered command it runs
`ACL DRYRUN _operator <command>` and fails if any of them come back denied.
That test only runs as part of `make test-e2e`, so a missing ACL grant won't
be caught by `make test` alone — when you add a new valkey-go client call to
the operator's reconciliation code, update the `_operator` ACL in
`internal/controller/users.go` accordingly and run `make test-e2e` (or at
least `go run ./hack/aclscan` plus a manual `ACL DRYRUN`) to confirm.

Because it works by parsing Go source rather than executing it, `aclscan` can't
resolve a command whose tokens aren't literal at the call site (e.g. an entirely
dynamic `Arbitrary(cmdVar)`); it errors out in that case rather than silently
skipping the call, so an unscannable call site gets noticed instead of quietly
falling out of ACL coverage.

## Prerequisites

- Go v1.25.0+.
- Docker or Podman.
- kubectl v1.32+.
- Access to a Kubernetes v1.32+ cluster.

## Build and deploy from source

**Build and push the operator image:**

```sh
make docker-build docker-push IMG=<some-registry>/valkey-operator:tag
```

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the operator to the cluster:**

```sh
make deploy IMG=<some-registry>/valkey-operator:tag
```

**Create a sample ValkeyCluster:**

```sh
kubectl apply -f config/samples/v1alpha1_valkeycluster.yaml
```

## Uninstall

**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -f config/samples/v1alpha1_valkeycluster.yaml
```

**Delete the CRDs from the cluster:**

```sh
make uninstall
```

**Undeploy the controller from the cluster:**

> **⚠️ Warning:** `make undeploy` removes all resources in the operator's namespace. Always deploy the operator in a dedicated namespace to avoid accidentally deleting unrelated workloads.

```sh
make undeploy
```

## Build the install bundle

Generate a single YAML file containing all resources (CRDs, RBAC, deployment):

```sh
make build-installer IMG=<some-registry>/valkey-operator:tag
```

This produces `dist/install.yaml` which can be applied with `kubectl apply -f`.

## Run the operator locally

The kubebuilder scaffolding gives a build target `make run` which runs the operator process locally, but towards a K8s cluster.
Since Pod IPs are not routable outside the cluster, any attempt by the operator to connect to a Valkey pod will fail.

Below are procedures for Linux and macOS.

### Linux

#### Prerequisites

* [kind](https://kind.sigs.k8s.io/).

#### Steps

##### 1. Create a kind cluster and install the operator CRD.

```bash
kind create cluster --name valkey-dev --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
EOF

# Install the operator CRD.
make install
```

##### 2. Setup local access to the pods in the kind cluster.

kind uses a Docker network. Add routes so your host can reach Pod CIDRs directly:

```bash
# Route Pod CIDRs to their respective nodes
for node_info in $(kubectl get nodes -o jsonpath='{range .items[*]}{@.metadata.name}={@.spec.podCIDR}{"\n"}{end}'); do
  node_name="${node_info%=*}"
  pod_cidr="${node_info#*=}"
  node_ip=$(docker inspect "$node_name" -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
  sudo ip route add "$pod_cidr" via "$node_ip"
done
```

##### 3. Start the operator locally and create a CR to trigger the reconciler.

In one terminal, start the operator:

```bash
make run
```

In another terminal, create the CR:

```bash
kubectl create -f config/samples/v1alpha1_valkeycluster.yaml
kubectl get valkeycluster -w
```

### macOS (Podman)

On macOS, Pod IPs are not directly routable from the host because containers run inside a Podman VM.
This procedure uses [podman-mac-net-connect](https://github.com/jasonmadigan/podman-mac-net-connect) to bridge that gap.

#### Prerequisites

* [Podman](https://podman.io/) for macOS (v5.0.0+).
* [kind](https://kind.sigs.k8s.io/).
* [podman-mac-net-connect](https://github.com/jasonmadigan/podman-mac-net-connect) — creates a WireGuard tunnel between macOS and the Podman VM.

#### Steps

##### 1. Set up a rootful Podman machine and install podman-mac-net-connect.

A rootful machine is required to get a bridge network, giving containers routable IPs
that `podman-mac-net-connect` can route to from macOS.

```bash
podman machine init --rootful
podman machine start

brew install jasonmadigan/tap/podman-mac-net-connect
sudo brew services start jasonmadigan/tap/podman-mac-net-connect
```

##### 2. Create a kind cluster and install the operator CRD.

```bash
KIND_EXPERIMENTAL_PROVIDER=podman kind create cluster --name valkey-dev --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
EOF

# Install the operator CRD.
make install
```

##### 3. Add routes for Pod CIDRs.

`podman-mac-net-connect` makes container IPs (kind nodes) reachable from macOS, but Pod CIDRs
are internal to the kind nodes. Add routes both in the Podman VM and on macOS:

```bash
kubectl get nodes -o jsonpath='{range .items[*]}{@.metadata.name}={@.spec.podCIDR}{"\n"}{end}' | while IFS='=' read -r node_name pod_cidr; do
  node_ip=$(podman inspect "$node_name" -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
  podman machine ssh sudo ip route add "$pod_cidr" via "$node_ip" < /dev/null
  sudo route -n add -net "$pod_cidr" "$node_ip" < /dev/null
done
```

##### 4. Start the operator locally and create a CR to trigger the reconciler.

In one terminal, start the operator:

```bash
make run
```

In another terminal, create the CR:

```bash
kubectl create -f config/samples/v1alpha1_valkeycluster.yaml
kubectl get valkeycluster -w
```

The operator should now be able to connect to Valkey containers in the kind cluster.

## Profiling the operator

The operator can serve Go's pprof endpoints, which attribute memory, CPU and
goroutine use to the code responsible. Container metrics say how much the process
consumes; a profile says which call sites account for it.

pprof is disabled by default and only serves when `--pprof-bind-address` is set.
Unlike the metrics endpoint, which is served over HTTPS with authentication, the
pprof server is plain HTTP with no authentication, and its profiles expose heap
contents. Bind it to localhost and never add it to a Service.

### Running locally

`make run` passes no arguments, so run the binary directly. This needs a cluster
to talk to, as described in [Run the operator locally](#run-the-operator-locally):

```bash
go run ./cmd/main.go --pprof-bind-address=localhost:8082
```

### Running in a cluster

Add the flag to the manager container and port-forward to reach it:

```bash
kubectl -n valkey-operator-system patch deployment valkey-operator-controller-manager \
  --type=json -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--pprof-bind-address=localhost:8082"}]'

kubectl -n valkey-operator-system port-forward deploy/valkey-operator-controller-manager 8082:8082
```

Port-forwarding is for debugging a specific operator instance, not a way to
expose pprof as a service. Remove the flag when finished; if the endpoint ever
has to be reachable remotely, restrict it with network policies and access
controls first.

### Collecting profiles

`-http=:8080` starts pprof's own web UI locally to render the profile; it is
unrelated to the operator's port.

```bash
# Total bytes allocated since start. Use for allocation churn and GC pressure,
# where memory is freed again but the rate is the problem.
go tool pprof -http=:8080 http://localhost:8082/debug/pprof/allocs

# Live heap at this instant. Use for retained memory, such as caches and
# long-lived state that never gets released.
go tool pprof -http=:8080 http://localhost:8082/debug/pprof/heap

# 30s CPU profile. Use for hot loops and slow reconciles.
go tool pprof -http=:8080 "http://localhost:8082/debug/pprof/profile?seconds=30"

# Goroutine dump. Use for leaks, deadlocks, and reconciles that never return.
curl -s "http://localhost:8082/debug/pprof/goroutine?debug=2"
```

To attribute a change rather than just measure a total, save a baseline and diff:

```bash
curl -s http://localhost:8082/debug/pprof/allocs > before.pprof
# apply load, or deploy the change
curl -s http://localhost:8082/debug/pprof/allocs > after.pprof

go tool pprof -top -nodecount=25 -base before.pprof after.pprof
go tool pprof -top -nodecount=20 -cum -base before.pprof after.pprof
```

Profile while the operator is doing the work you want to understand. It is mostly
idle between reconciles, so a sample taken at rest shows little beyond the Go
runtime and the informer cache. Reproduce the workload first, whether that is
creating clusters, scaling shards, or a failover, and sample while it runs.
