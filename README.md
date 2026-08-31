# devcontainer

[![CI](https://github.com/crunchloop/devcontainer/actions/workflows/ci.yml/badge.svg)](https://github.com/crunchloop/devcontainer/actions/workflows/ci.yml)
[![Status: alpha](https://img.shields.io/badge/status-alpha-orange)](#status)
[![Go Reference](https://pkg.go.dev/badge/github.com/crunchloop/devcontainer.svg)](https://pkg.go.dev/github.com/crunchloop/devcontainer)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

A programmatic Go runtime for [Dev Containers](https://containers.dev/).
Embed the full devcontainer lifecycle — resolve, build, up, exec,
lifecycle phases, down — into your Go application without shelling out
to the Node `@devcontainers/cli`.

## Why

The reference [`@devcontainers/cli`](https://github.com/devcontainers/cli)
is a Node binary. Embedding it in a Go service means a Node runtime
dependency, opaque failure modes (success exit codes with
`outcome:error` JSON on stdout), and CLI-flag-shaped APIs for every
interaction. This library is a clean Go implementation of the spec's
embedding-relevant subset, designed to be a drop-in replacement for
shelling out.

## Status

**Alpha.** API is stable enough for early integration but may change
between minor versions until v1.0.0. The `events` channel surface is
explicitly experimental.

### Backends

The container backend is pluggable: anything implementing
`runtime.Runtime` can be wired in at engine construction time. The
documented backend is:

- **`runtime/docker`** — Docker Engine over `moby/moby/client`.
  Requires a reachable Docker daemon socket.

The engine, feature pipeline, lifecycle, and compose paths are written
against the `runtime.Runtime` interface rather than against Docker
directly.

### Spec compliance

Status of each [Dev Containers spec](https://containers.dev/implementors/json_reference/)
field/behavior the library covers. Legend: ✅ acted on · ⚠️ parsed but not enforced (or partial) · ❌ missing · ➖ out of scope.

**Sources**

| Field | Status | Notes |
| --- | --- | --- |
| `image` | ✅ | Pull, run, exec |
| `build` (`dockerfile`, `context`, `args`, `target`, `cacheFrom`) | ✅ | User Dockerfile + features layered atop |
| `dockerComposeFile`, `service`, `runServices` | ✅ | compose-go parse + either shell-out to `docker compose` (default) or an in-process orchestrator (`EngineOptions.ComposeBackend = ComposeBackendNative`). The native orchestrator drives any `Runtime` implementing the compose primitives. |

**Container config**

| Field | Status | Notes |
| --- | --- | --- |
| `workspaceFolder`, `workspaceMount` | ✅ | |
| `mounts` (bind / volume / tmpfs) | ✅ | |
| `containerEnv`, `remoteEnv` | ✅ | |
| `containerUser`, `remoteUser` | ✅ | |
| `updateRemoteUserUID` | ✅ | Portable shell (Debian, Alpine/BusyBox) |
| `userEnvProbe` (all four modes) | ✅ | |
| `runArgs`, `init`, `privileged`, `capAdd`, `securityOpt`, `overrideCommand` | ✅ | |
| `shutdownAction` | ✅ | Honored via `Engine.Shutdown` (`none` / `stopContainer` / `stopCompose`) |

**Features**

| Field | Status | Notes |
| --- | --- | --- |
| `features` (OCI / HTTPS / local) | ✅ | DAG ordering, options validation, content-addressed cache |
| `dependsOn` / `installsAfter` / `overrideFeatureInstallOrder` | ✅ | |
| Pre-baked-image short-circuit | ✅ | `devcontainer.metadata` label read on Up |
| `devcontainer-lock.json` | ❌ | [#26](https://github.com/crunchloop/devcontainer/issues/26) |

**Lifecycle**

| Phase | Status | Notes |
| --- | --- | --- |
| `initializeCommand` (host) | ✅ | Opt-in via `UpOptions.RunInitializeCommand`; requires `EngineOptions.HostExecutor` |
| `onCreateCommand`, `updateContentCommand`, `postCreateCommand` | ✅ | Run-once idempotency markers |
| `postStartCommand`, `postAttachCommand` | ✅ | Run-every-start / run-every-Up |
| `waitFor` | ✅ | |
| Parallel command form (object → named commands run concurrently) | ✅ | |
| `secretsCommand` | ✅ | Opt-in via `UpOptions.RunSecretsCommand`; requires `EngineOptions.HostExecutor` |

**Substitution**

| Variable | Status | Notes |
| --- | --- | --- |
| `${localWorkspaceFolder[Basename]}`, `${containerWorkspaceFolder[Basename]}` | ✅ | |
| `${localEnv:VAR[:default]}`, `${devcontainerId}` | ✅ | |
| `${containerEnv:VAR[:default]}` | ✅ | Two-pass: host context pre-create, re-applied per `Exec` via the workspace substituter |

**Ports**

| Field | Status | Notes |
| --- | --- | --- |
| `forwardPorts` | ⚠️ parsed | Not actuated; [#7](https://github.com/crunchloop/devcontainer/issues/7) |
| `portsAttributes`, `otherPortsAttributes` | ⚠️ parsed | Surfaced on `ResolvedConfig`; not enforced |
| `appPort` (deprecated) | ✅ translated | Folded into `forwardPorts` (skipping container ports already declared); deprecation warning still emitted |

**Other**

| Behavior | Status | Notes |
| --- | --- | --- |
| `customizations.<tool>` pass-through | ✅ | `map[string]json.RawMessage` for callers |
| `hostRequirements` | ⚠️ parsed | Surfaced; not enforced |
| `devcontainer.metadata` label round-trip | ✅ | Written on build, read + merged on next Up |
| Unknown-field warnings (top-level + nested) | ✅ | Includes `build`, `hostRequirements`, `gpu` sub-objects |
| GPG / SSH agent forwarding | ❌ | [#28](https://github.com/crunchloop/devcontainer/issues/28) |

**Out of scope** (➖): Templates spec, dotfiles repos, IDE injection
hooks, Kubernetes drivers.

## Install

```bash
go get github.com/crunchloop/devcontainer
```

Requires:

- Go 1.25+
- Docker: daemon socket reachable; Docker Compose v2 plugin only when
  running `dockerComposeFile` projects under the default shellout
  backend (skip the plugin if you opt into `ComposeBackendNative`).

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

func main() {
	ctx := context.Background()

	rt, err := docker.New(ctx, docker.Options{})
	if err != nil {
		log.Fatalf("docker: %v", err)
	}
	defer rt.Close()

	eng, err := devcontainer.New(devcontainer.EngineOptions{Runtime: rt})
	if err != nil {
		log.Fatalf("engine: %v", err)
	}

	ws, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: "/path/to/your/project",
	})
	if err != nil {
		log.Fatalf("up: %v", err)
	}
	defer eng.Down(ctx, ws, devcontainer.DownOptions{Remove: true})

	res, err := eng.Exec(ctx, ws, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "echo $USER"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("user:", res.Stdout)
}
```

Runnable end-to-end examples in [`examples/`](examples/):

- [`image-source/`](examples/image-source/) — minimal image-only devcontainer
- [`with-features/`](examples/with-features/) — image + a local feature with `containerEnv`
- [`compose/`](examples/compose/) — multi-service `dockerComposeFile`

## API surface

The main entry points live in the root package:

```go
type Engine struct { /* ... */ }

func New(opts EngineOptions) (*Engine, error)
func Resolve(ctx context.Context, opts ResolveOptions) (*ResolvedConfig, error)

func (*Engine) Up(ctx, UpOptions) (*Workspace, error)
func (*Engine) Attach(ctx, WorkspaceID) (*Workspace, error)
func (*Engine) Exec(ctx, *Workspace, ExecOptions) (ExecResult, error)
func (*Engine) ExecByID(ctx, WorkspaceID, ExecOptions) (ExecResult, error)
func (*Engine) RunLifecycle(ctx, *Workspace, LifecyclePhase) error
func (*Engine) Down(ctx, *Workspace, DownOptions) error
```

Sub-packages:

- `config` — devcontainer.json parsing, merging, host-context substitution
- `runtime` — container backend abstraction (`Runtime`, `ComposeRuntime`, network/volume/list primitives)
- `runtime/docker` — Docker Engine API implementation (uses `moby/moby/client`)
- `feature` — feature resolution (OCI / HTTPS / local), DAG ordering, dockerfile generation
- `compose` — `dockerComposeFile` parsing via `compose-spec/compose-go`, plus a runtime-agnostic in-process orchestrator (`Orchestrator`, `Plan`, topological + health gating) used when `ComposeBackendNative` is selected

## Tests

```bash
make test              # unit tests
make test-integration  # integration tests against real Docker
make lint              # golangci-lint
```

The integration suite (build tag `integration`) exercises real Docker:
pulls public images from GHCR, builds Dockerfiles, runs feature install
scripts, drives `docker compose up/down`. Skipped automatically if a
Docker daemon isn't reachable.

## Design

Architectural notes — the choices behind the public API and why
they were made — live under [`design/`](design/README.md). Useful
when contributing or when embedding the library in a non-trivial
way; not required reading for normal use.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports welcome via
[GitHub issues](https://github.com/crunchloop/devcontainer/issues).

## License

[Apache License 2.0](LICENSE).
