# valkey-operator

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0)

A Kubernetes operator for deploying Valkey, Valkey Clusters and managing its lifecycle.

## Description

Valkey Operator is a Kubernetes operator that automates the deployment and management of Valkey clusters with optional persistent storage support. The operator simplifies the process of deploying Valkey on Kubernetes clusters, ensuring that it is configured correctly and operates efficiently.

### Key Features

- 🚀 **Automated Cluster Management** - Deploy and manage Valkey clusters with configurable shards and replicas
- 💾 **Persistent Storage Support** - Optional PersistentVolume support for data durability
- 🔒 **Security Hardened** - Non-root containers, security contexts, and volume permissions
- 📊 **Metrics Export** - Built-in Prometheus metrics exporter
- 🔄 **High Availability** - Multi-shard clusters with replica support
- ⚙️ **Flexible Configuration** - Resource limits, tolerations, node selectors, and affinity rules

### Storage Modes

The operator supports two storage modes:

- **Ephemeral Storage (Default)** - Uses emptyDir for temporary storage, suitable for caching workloads
- **Persistent Storage** - Uses PersistentVolumeClaims with configurable size and storage class for production workloads

> **⚠️ EARLY DEVELOPMENT NOTICE**
>
> This operator is in active development and **not ready for production use**. We're actively working on:
>
> - Core cluster management features
> - API stability and design
> - Testing and validation
>
> **We welcome your feedback!**
>
> - 💡 [Share ideas and suggestions](https://github.com/valkey-io/valkey-operator/discussions/categories/ideas)
> - 🏗️ [Participate in design discussions](https://github.com/valkey-io/valkey-operator/discussions/categories/design)
> - 🙏 [Ask questions](https://github.com/valkey-io/valkey-operator/discussions/categories/q-a)
> - 🐛 [Report bugs](https://github.com/valkey-io/valkey-operator/issues)

## Getting Started

### Prerequisites

- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster

**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/valkey-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/valkey-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
# Deploy with persistent storage (production)
kubectl apply -f config/samples/v1alpha1_valkeycluster.yaml

# OR deploy with ephemeral storage (dev/test)
kubectl apply -f config/samples/v1alpha1_valkeycluster_ephemeral.yaml
```

### Example Configurations

#### Persistent Storage (Production)

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkey-prod
spec:
  shards: 3
  replicas: 2
  resources:
    requests:
      memory: "512Mi"
      cpu: "250m"
    limits:
      memory: "1Gi"
      cpu: "500m"
  storage:
    enabled: true
    size: "10Gi"
    storageClassName: "gp3"  # AWS EBS gp3
    accessModes:
      - ReadWriteOnce
  volumePermissions: true  # Recommended for persistent storage
```

#### Ephemeral Storage (Dev/Test)

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkey-dev
spec:
  shards: 3
  replicas: 1
  resources:
    requests:
      memory: "256Mi"
      cpu: "100m"
  # storage section omitted - uses emptyDir
```

### Deploying to AWS EKS

1. **Configure AWS credentials and kubectl context:**

```sh
aws eks update-kubeconfig --region us-east-1 --name your-cluster-name
```

2. **Build and push the operator image:**

```sh
# Set your Docker registry
export IMG=<your-dockerhub-username>/valkey-operator:latest

# Build for AMD64 (EKS)
make docker-buildx PLATFORMS=linux/amd64

# Or build and push separately
docker buildx build --platform linux/amd64 -t ${IMG} --push .
```

3. **Install CRDs and deploy the operator:**

```sh
make install
make deploy IMG=${IMG}
```

4. **Create a ValkeyCluster with EBS storage:**

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkey-eks
spec:
  shards: 3
  replicas: 2
  storage:
    enabled: true
    size: "20Gi"
    storageClassName: "gp3"  # EBS gp3 storage class
  volumePermissions: true
```

5. **Verify deployment:**

```sh
# Check operator pod
kubectl get pods -n valkey-operator-system

# Check ValkeyCluster status
kubectl get valkeycluster
kubectl describe valkeycluster valkey-eks

# Check pods and PVCs
kubectl get pods,pvc -l app.kubernetes.io/name=valkey
```

> **NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall

**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/valkey-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/valkey-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
   can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Community & Support

### Getting Help

- 📖 **[Documentation](./docs/)** - Developer guides and architecture docs
- 🙏 **[Ask Questions](https://github.com/valkey-io/valkey-operator/discussions/categories/q-a)** - GitHub Discussions Q&A
- 💬 **[Slack Channel](https://valkey.io/slack)** - Join `#valkey-k8s-operator` to discuss and connect with the community
- 📝 **[Support Guide](./SUPPORT.md)** - How to get help

### Contributing

We welcome contributions from the community! Whether you're fixing bugs, adding features, or improving documentation, your help is appreciated.

- 📋 **[Contributing Guide](./CONTRIBUTING.md)** - How to contribute code and documentation
- 💡 **[Feature Ideas](https://github.com/valkey-io/valkey-operator/discussions/categories/ideas)** - Suggest new features
- 🏗️ **[Design Discussions](https://github.com/valkey-io/valkey-operator/discussions/categories/design)** - Architectural proposals and RFCs
- 🐛 **[Report Issues](https://github.com/valkey-io/valkey-operator/issues)** - Bug reports

**All contributors must sign off commits (DCO).** See [CONTRIBUTING.md](./CONTRIBUTING.md) for details.

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2025 Valkey Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
