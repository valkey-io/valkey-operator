# valkey-operator

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0)

A Kubernetes operator for deploying Valkey Clusters and managing their lifecycle.

## Description

Valkey Operator is a Kubernetes operator that automates the deployment and management of [Valkey](https://valkey.io/), a high-performance data structure server that primarily serves key/value workloads.
The operator simplifies deploying Valkey Clusters on Kubernetes, handling scaling, rolling upgrades, failover, TLS, and access control automatically.

> **⚠️ EARLY DEVELOPMENT NOTICE**
>
> This operator is in active development and **not ready for production use**.
> The `v1alpha1` API may change in future releases.
>
> **We welcome your feedback!**
>
> - 💡 [Share ideas and suggestions](https://github.com/valkey-io/valkey-operator/discussions/categories/ideas)
> - 🏗️ [Participate in design discussions](https://github.com/valkey-io/valkey-operator/discussions/categories/design)
> - 🙏 [Ask questions](https://github.com/valkey-io/valkey-operator/discussions/categories/q-a)
> - 🐛 [Report bugs](https://github.com/valkey-io/valkey-operator/issues)
>
> Want to discuss the operator development? Join the [tech call every Friday at 11:00-11:30 US Eastern](https://zoom-lfx.platform.linuxfoundation.org/meeting/99658320446?password=2eae4006-633e-4fed-aa93-631ab2101421).

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

## License

This project is licensed under the [Apache License 2.0](./LICENSE).
