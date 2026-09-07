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

## Prerequisites

- Go v1.25.0+.
- Docker or Podman.
- kubectl v1.31+.
- Access to a Kubernetes v1.31+ cluster.

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

## Regenerating the version-gated config

`internal/valkey/config_version_metadata.go` is generated: it maps each Valkey
config directive to the minimum Valkey version that understands it, so the
operator can drop directives an older target image would reject. Regenerate it
when a new Valkey minor is released, or when its first release candidate appears
(to gate directives for users testing the rc). Regenerating after the final
release retightens any rc-based entries to the final version and drops
directives that did not survive to release.

Prerequisite: a local checkout of the [Valkey source](https://github.com/valkey-io/valkey)
with its release tags fetched.

```sh
# From the operator repo root; point --valkey-repo at your Valkey checkout.
hack/gen_config_version_metadata.py \
    --valkey-repo /path/to/valkey \
    --baseline 8.1.0 \
    --out internal/valkey/config_version_metadata.go
```

- `--baseline` is the lowest Valkey version to gate against (the operator's
  supported floor); directives present at or before it are omitted.
- For each Valkey minor version (9.0, 9.1, 9.2, ...), the script scans every
  release candidate and the final `X.Y.0`. A directive is gated at the earliest
  version it appears in; while only rcs exist it is gated at that rc (so an rc
  image being tested still gets it), and once the final ships the gate snaps to
  the final. A directive that disappears from the newest scanned release of its
  line is dropped. Patch releases and hidden (internal) directives are skipped.
  Do not edit the generated file by hand.

Verify the committed file is up to date with `--check`, which exits non-zero
if regeneration would change it:

```sh
hack/gen_config_version_metadata.py \
    --valkey-repo /path/to/valkey \
    --baseline 8.1.0 \
    --out internal/valkey/config_version_metadata.go \
    --check
```

Pass `--debug` to log, per scanned tag, how many directives were parsed and
which are newly introduced.

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
