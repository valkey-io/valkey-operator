# valkey-operator

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0)

A Kubernetes operator for deploying Valkey, Valkey Clusters and managing its lifecycle.

## Description

Valkey Operator is a Kubernetes operator that automates the deployment and management of Valkey, a secure and scalable key management solution. The operator simplifies the process of deploying Valkey on Kubernetes clusters, ensuring that it is configured correctly and operates efficiently. It provides features such as automated installation, configuration management, and lifecycle management of Valkey instances.

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
> 
> Want to discuss the operator development? Join the [tech call every Friday at 11:00-11:30 US Eastern](https://zoom-lfx.platform.linuxfoundation.org/meeting/99658320446?password=2eae4006-633e-4fed-aa93-631ab2101421
).


## Getting Started

- **Users:** [Quickstart](./docs/quickstart.md)
- **Contributors:** [Developer Guide](./docs/developer-guide.md)

## Community & Support

### Getting Help

- 📖 **[Documentation](./docs/)** - Guides and architecture docs
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
