# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Overview

Kurtosis is a platform for building, running and testing distributed systems. It abstracts over Docker and Kubernetes to provide a unified experience for spinning up ephemeral development and test environments. This monorepo contains all components of the Kurtosis platform.

## Architecture

Kurtosis operates on a three-tier architecture:

1. **CLI** (`cli/`): User-facing command-line tool that communicates with the Engine
2. **Engine** (`engine/`): Container that manages enclaves (isolated environments)
3. **Core (APIC)** (`core/`): API Container launched inside each enclave to coordinate service state

Supporting libraries:
- `api/`: Protobuf definitions for Engine and Core gRPC APIs (with Go, TypeScript, and Rust bindings)
- `container-engine-lib/`: Abstraction layer over Docker/Kubernetes/Podman
- `contexts-config-store/`: Manages Kurtosis context configurations
- `enclave-manager/`: Manages enclave lifecycle
- `grpc-file-transfer/`: File transfer protocol implementation
- `metrics-library/`: Telemetry collection
- `name_generator/`: Generates human-readable names
- `kurtosis_version/`: Version constants (auto-generated)

## Common Commands

### Building the Project

Build everything:
```bash
./scripts/build.sh
```

Build individual components:
```bash
./cli/scripts/build.sh
./engine/scripts/build.sh
./core/scripts/build.sh
./api/scripts/build.sh
./container-engine-lib/scripts/build.sh
```

Build for Podman:
```bash
./scripts/build.sh false true
```

Build with debug images (for Engine and Core):
```bash
./scripts/build.sh true
```

Generate version constants (required if you see module import errors):
```bash
./scripts/generate-kurtosis-version.sh
```

### Running the Dev CLI

After building, use the launch script instead of the installed `kurtosis` command:
```bash
./cli/cli/scripts/launch-cli.sh <command>
```

Set up convenient aliases:
```bash
source ./scripts/set_kt_alias.sh
ktdev enclave add  # Uses locally built version
```

Aliases created:
- `ktdev`: Run locally built CLI
- `ktdebug`: Run locally built CLI with debug support

### Testing

Run all unit tests:
```bash
./scripts/build.sh  # Build scripts include unit tests
```

Run unit tests for specific Go module:
```bash
cd cli/cli/
go test ./...
```

Run end-to-end tests:
```bash
# Prerequisites: Docker running, Kurtosis engine running
./internal_testsuites/scripts/test.sh
```

### Regenerating Protobuf Bindings

When `.proto` files change in `api/protobuf/`:
```bash
./api/scripts/regenerate-protobuf-bindings.sh
```

This regenerates bindings for:
- Go: `api/golang/`
- TypeScript: `api/typescript/`
- Rust: `api/rust/`

### Linting and Formatting

```bash
./scripts/go-lint-all.sh
./scripts/go-tidy-all.sh
```

## Development Workflow

### Using the Dev CLI

The launch script automatically uses locally built Engine and Core images:
```bash
./cli/cli/scripts/launch-cli.sh engine status
# Shows version matching your local build (e.g., "53d823" or "53d823-dirty")
```

Override Engine version:
```bash
ktdev engine restart --version <image-tag>
```

Override Core (APIC) version:
```bash
ktdev enclave add --api-container-version <image-tag>
```

### Debugging Go Code

**Debug CLI:**
1. Build: `cli/cli/scripts/build.sh`
2. Source aliases: `source ./scripts/set_kt_alias.sh`
3. Run with debugger: `ktdebug version`
4. Connect remote debugger (GoLand: "CLI-remote-debug" configuration) or use Delve in terminal: `ktdebug dlv-terminal version`

**Debug Engine:**
1. Build debug image: `scripts/build.sh true`
2. Start engine in debug mode: `ktdev engine start --debug-mode`
3. Connect remote debugger (GoLand: "Engine-remote-debug" configuration)
4. For K8s: Upload image (`k3d image load kurtosistech/engine:<version>-debug`), run `scripts/port-forward-engine-debug.sh`, start gateway (`ktdev gateway`)

**Debug Core (APIC):**
1. Build debug image: `scripts/build.sh true`
2. Start engine: `ktdev engine start --debug-mode` (optional)
3. Add enclave in debug mode: `ktdev enclave add --debug-mode` (only one enclave at a time)
4. Connect remote debugger (GoLand: "APIC-remote-debug" configuration)
5. Find APIC's gRPC port (mapped to container's 7443)
6. For K8s: Upload image, run `scripts/port-forward-apic-debug.sh <enclave-name>`, start gateway

## Key Technical Details

### Go Workspace

The repository uses Go workspaces (`go.work`) with multiple modules. All Go modules use Go 1.23+.

### Container Images

- Engine image: `kurtosistech/engine`
- Core (APIC) image: `kurtosistech/core`
- Debug images have `-debug` suffix

### TypeScript Development

When developing TypeScript tests in `internal_testsuites/typescript/`:
- Build `api/typescript` first
- Changes to `api/typescript` are not hot-reloaded

### Starlark Packages

Users write Kurtosis packages in Starlark (`.star` files). Test packages are in `internal_testsuites/starlark/`.

## Dependencies

### Required (Nix)
```bash
nix develop  # Loads all dependencies
```

### Required (Manual)
- Bash 5+
- Git
- Docker (or Podman)
- Go 1.23+
- Goreleaser
- Node 20+ and Yarn
- Rust (for Rust bindings)
- Protoc compiler binaries: `protoc-gen-go`, `protoc-gen-go-grpc`, `protoc-gen-connect-go`
- TypeScript protoc tools: `ts-protoc-gen`, `grpc-tools`
- OpenAPI generators: `oapi-codegen`, `openapi-typescript`

## Important Notes

- This is a monorepo but each major component (`cli/`, `engine/`, `core/`, etc.) has its own build lifecycle
- Version is determined by `version.txt` and injected during build via `scripts/generate-kurtosis-version.sh`
- The CLI, Engine, and Core must be version-compatible; dev builds use matching versions automatically
- Kurtosis is NOT recommended for production deployments
