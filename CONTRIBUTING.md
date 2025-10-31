# Contributing to Valkey Operator

Thank you for your interest in contributing to the Valkey Operator! We welcome contributions from the community.

## Getting Started

### Prerequisites

- Go 1.24.0+
- Docker 17.03+
- kubectl v1.11.3+
- Access to a Kubernetes cluster (kind, minikube, or cloud provider)

### Development Setup

See the [Developer Guide](./docs/developer-guide.md) for detailed setup instructions.

Quick start:

```bash
# Clone the repository
git clone https://github.com/valkey-io/valkey-operator.git
cd valkey-operator

# Install dependencies
make generate
make manifests

# Run tests
make test

# Run the operator locally
make install
make run
```

## How to Contribute

### Reporting Bugs

Found a bug? Please [open an issue](https://github.com/valkey-io/valkey-operator/issues/new) with:

- Clear description of the problem
- Steps to reproduce
- Expected vs actual behavior
- Your environment (K8s version, operator version, etc.)
- Relevant logs and configuration (sanitize secrets!)

### Suggesting Features

Have an idea? Start a discussion in [GitHub Discussions](https://github.com/valkey-io/valkey-operator/discussions) under the "Ideas" category. This helps gather community feedback before opening an issue or PR.

### Submitting Pull Requests

1. **Fork the repository** and create a branch from `main`
2. **Make your changes** following our coding standards (below)
3. **Add tests** for new functionality
4. **Run tests and linting**: `make test lint`
5. **Update documentation** if needed
6. **Commit with clear messages** describing what and why
7. **Open a Pull Request** with a clear description

We'll review your PR as soon as possible. Be patient and responsive to feedback.

## Coding Standards

- Follow standard Go conventions and idiomatic Go code
- Run `go fmt` before committing (or use `make fmt`)
- Add comments for exported functions and types
- Write unit tests for new code
- Keep PRs focused - one feature/fix per PR
- Update CRDs by modifying `api/v1alpha1/*_types.go` then run `make manifests generate`

## Project Structure

```
valkey-operator/
├── api/v1alpha1/          # CRD definitions
├── cmd/                   # Main application entry point
├── config/                # Kubernetes manifests and Kustomize configs
├── internal/controller/   # Controller reconciliation logic
├── docs/                  # Documentation
└── test/                  # E2E and integration tests
```

## Testing

```bash
# Run unit tests
make test

# Run E2E tests (requires cluster)
make test-e2e

# Run specific tests
go test ./internal/controller/... -v
```

## Making Changes to the API

If you need to modify the ValkeyCluster CRD:

1. Edit `api/v1alpha1/valkeycluster_types.go`
2. Add kubebuilder markers for validation and defaults
3. Regenerate manifests: `make manifests generate`
4. Update the sample CR in `config/samples/`
5. Test with `make install` to apply the updated CRD

## Development Workflow

```bash
# 1. Make code changes
vim internal/controller/valkeycluster_controller.go

# 2. Update generated code and manifests
make generate manifests

# 3. Run tests
make test

# 4. Test locally
make install    # Install CRDs
make run        # Run controller locally

# 5. Build and test container
make docker-build IMG=valkey-operator:dev
make deploy IMG=valkey-operator:dev
```

## Code Review Process

- All submissions require review from maintainers
- We may ask for changes or improvements
- Once approved, a maintainer will merge your PR
- PRs should pass all CI checks before merging

## Questions?

- 💬 Ask in [GitHub Discussions](https://github.com/valkey-io/valkey-operator/discussions)
- 📖 Check the [Developer Guide](./docs/developer-guide.md)
- 📝 Read the [Support Guide](./SUPPORT.md)

## Code of Conduct

Be respectful, inclusive, and constructive. We're all here to build something great together.

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.

---

**Thank you for contributing to Valkey Operator!** 🙌
