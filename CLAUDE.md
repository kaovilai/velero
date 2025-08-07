# CLAUDE.md - Velero Project Guide

## Project Overview

Velero (formerly Heptio Ark) is a Kubernetes backup and disaster recovery tool that helps you back up and restore Kubernetes cluster resources and persistent volumes. It can run on public cloud platforms or on-premises environments.

**Repository:** https://github.com/vmware-tanzu/velero  
**Language:** Go 1.24  
**Type:** Kubernetes backup/restore tool  

## Development Setup

### Prerequisites

- Go 1.24 or later
- Docker (with buildx for multi-platform builds)
- kubectl
- A Kubernetes cluster for testing
- golangci-lint for code linting

### Getting Started

1. **Clone and Navigate:**
   ```bash
   git clone https://github.com/vmware-tanzu/velero.git
   cd velero
   ```

2. **Install Dependencies:**
   ```bash
   go mod tidy
   ```

3. **Build the Project:**
   ```bash
   make build
   # Or for local development:
   make local
   ```

4. **Run Tests:**
   ```bash
   # Unit tests
   make test
   
   # Lint code
   make lint
   
   # Verify all checks
   make verify
   ```

## Key Commands

### Build Commands
- `make build` - Build velero binary for current platform
- `make all-build` - Build binaries for all supported platforms
- `make local` - Build locally without Docker container
- `make container` - Build Docker container images

### Testing Commands
- `make test` - Run unit tests
- `make test-local` - Run unit tests locally (without Docker)
- `make test-e2e` - Run end-to-end tests (requires cluster setup)
- `make lint` - Run golangci-lint
- `make local-lint` - Run linting locally

### Code Generation Commands
- `make update` - Update all generated code
- `make update-crd` - Update CRD code only (faster)
- `make verify` - Verify all generated code is up-to-date

### Development Utilities
- `make shell CMD="<command>"` - Run commands in the build container
- `make modules` - Tidy go modules
- `make verify-modules` - Verify modules are up-to-date

## Project Structure

### Key Directories

- **`cmd/`** - CLI and server entry points
  - `cmd/velero/` - Main CLI application
  - `cmd/velero-helper/` - Helper utilities
- **`pkg/`** - Core application logic
  - `pkg/apis/` - API definitions and CRDs
  - `pkg/backup/` - Backup logic and controllers
  - `pkg/restore/` - Restore logic and controllers
  - `pkg/plugin/` - Plugin framework
  - `pkg/controller/` - Kubernetes controllers
- **`internal/`** - Internal packages not exposed as APIs
- **`hack/`** - Build scripts and development tools
- **`test/`** - Test utilities and end-to-end tests
- **`site/`** - Documentation website source
- **`config/`** - Kubernetes YAML configs (CRDs, RBAC)

### Key Files

- `Makefile` - Build automation and development commands
- `go.mod/go.sum` - Go module definitions
- `Dockerfile` - Container image build instructions
- `hack/test.sh` - Unit test runner script
- `hack/lint.sh` - Linting script
- `hack/build.sh` - Build script

## Testing

### Unit Tests
```bash
# Run all unit tests
make test

# Run specific test packages
make shell CMD="-c 'go test ./pkg/backup/...'"

# Run tests with coverage
make shell CMD="-c 'go test -coverprofile=coverage.out ./pkg/...'"
```

### End-to-End Tests
E2E tests require a Kubernetes cluster and cloud storage setup. See `test/e2e/README.md` for detailed configuration.

**Basic E2E test setup:**
```bash
# Example with kind cluster and MinIO/AWS storage
BSL_BUCKET=my-test-bucket \
CREDS_FILE=/path/to/aws-credentials \
CLOUD_PROVIDER=kind \
OBJECT_STORE_PROVIDER=aws \
make test-e2e
```

### Performance Tests
```bash
make test-perf
```

## Development Workflow

### Making Changes

1. **Create a branch:**
   ```bash
   git checkout -b feature/my-feature
   ```

2. **Make your changes and test:**
   ```bash
   # Run tests
   make test
   
   # Run linting
   make lint
   
   # Verify generated code is current
   make verify
   ```

3. **Update generated code if needed:**
   ```bash
   make update
   ```

4. **Build and test locally:**
   ```bash
   make local
   # Binary will be in _output/bin/
   ```

### Commit Message Guidelines

When writing commit messages that reference issues, always use the canonical upstream repository reference:

- **Correct:** `Fixes vmware-tanzu/velero#123`
- **Incorrect:** `Fixes kaovilai/velero#123` or `Fixes #123`

This ensures issue references point to the official VMware Tanzu Velero repository regardless of which fork the development work is done in.

**Example commit messages:**
```
fix: resolve backup validation error

Fixes vmware-tanzu/velero#456

feat: add support for new storage provider

Implements vmware-tanzu/velero#789
```

### Code Quality

The project uses:
- **golangci-lint** for code linting
- **go fmt** for code formatting
- **go vet** for static analysis
- Unit tests with table-driven patterns
- End-to-end tests using Ginkgo/Gomega

### Plugin Development

Velero supports plugins for:
- **Backup Item Actions** - Custom logic during backup
- **Restore Item Actions** - Custom logic during restore  
- **Volume Snapshotters** - Storage provider integrations
- **Object Stores** - Backup storage backends

Plugin development uses go-plugin with gRPC communication.

## Common Development Tasks

### Adding a New Feature

1. Update API types in `pkg/apis/` if needed
2. Implement core logic in appropriate `pkg/` subdirectory
3. Add/update controllers in `pkg/controller/`
4. Add unit tests alongside your code
5. Update CRDs: `make update-crd`
6. Add E2E tests in `test/e2e/` if appropriate

### Debugging

1. **Enable debug mode:**
   ```bash
   make local DEBUG=1
   ```

2. **Run with verbose logging:**
   ```bash
   velero --log-level debug <command>
   ```

3. **Use the development shell:**
   ```bash
   make shell CMD="-c 'bash'"
   ```

## Architecture Notes

Velero consists of:

1. **Server components** - Run as pods in Kubernetes cluster
   - Backup/Restore controllers
   - Plugin manager
   - Webhook handlers

2. **CLI tool** - Installed locally for managing backups/restores

3. **Plugin system** - Extensible architecture for different storage providers

4. **CRDs** - Custom resources for backups, restores, schedules, etc.

The system integrates with cloud storage (AWS S3, Azure Blob, GCS) and volume snapshot APIs.

## Compatibility

**Kubernetes:** 1.18+ (see README.md for version matrix)  
**Go:** 1.24+  
**Architecture:** linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64  

## Resources

- [Official Documentation](https://velero.io/docs/)
- [Contributing Guide](https://velero.io/docs/start-contributing/)
- [Architecture Overview](https://velero.io/docs/main/how-velero-works/)
- [Plugin Development](https://velero.io/docs/main/plugins/)
- [Troubleshooting](https://velero.io/docs/troubleshooting/)

## Quick Reference

```bash
# Local development cycle
make local          # Build binary locally
make test           # Run unit tests  
make lint           # Check code quality
make verify         # Verify generated code

# Container development
make container      # Build container image
make shell          # Interactive development shell

# Full verification
make ci             # Run full CI pipeline locally
```