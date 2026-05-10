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
explicitly experimental. See [PRD.md](#design-docs) for scope and
roadmap.

### Spec compliance

Status of each [Dev Containers spec](https://containers.dev/implementors/json_reference/)
field/behavior the library covers. Legend: ✅ acted on · ⚠️ parsed but not enforced (or partial) · ❌ missing · ➖ out of scope.

**Sources**

| Field | Status | Notes |
| --- | --- | --- |
| `image` | ✅ | Pull, run, exec |
| `build` (`dockerfile`, `context`, `args`, `target`, `cacheFrom`) | ✅ | User Dockerfile + features layered atop |
| `dockerComposeFile`, `service`, `runServices` | ✅ | compose-go parse, shell-out to `docker compose` |

**Container config**

| Field | Status | Notes |
| --- | --- | --- |
| `workspaceFolder`, `workspaceMount` | ✅ | |
| `mounts` (bind / volume / tmpfs) | ✅ | |
| `containerEnv`, `remoteEnv` | ✅ | |
| `containerUser`, `remoteUser` | ✅ | |
| `updateRemoteUserUID` | ✅ | Debian-family images; Alpine/BusyBox tracked in [#29](https://github.com/crunchloop/devcontainer/issues/29) |
| `userEnvProbe` (all four modes) | ✅ | |
| `runArgs`, `init`, `privileged`, `capAdd`, `securityOpt`, `overrideCommand` | ✅ | |
| `shutdownAction` | ⚠️ parsed | Not enforced at `Down`; [#22](https://github.com/crunchloop/devcontainer/issues/22) |

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
| `initializeCommand` (host) | ⚠️ stub | Opt-in; requires `HostExecutor` wiring; [#11](https://github.com/crunchloop/devcontainer/issues/11) |
| `onCreateCommand`, `updateContentCommand`, `postCreateCommand` | ✅ | Run-once idempotency markers |
| `postStartCommand`, `postAttachCommand` | ✅ | Run-every-start / run-every-Up |
| `waitFor` | ✅ | |
| Parallel command form (object → named commands run concurrently) | ✅ | |
| `secretsCommand` | ❌ | [#23](https://github.com/crunchloop/devcontainer/issues/23) |

**Substitution**

| Variable | Status | Notes |
| --- | --- | --- |
| `${localWorkspaceFolder[Basename]}`, `${containerWorkspaceFolder[Basename]}` | ✅ | |
| `${localEnv:VAR[:default]}`, `${devcontainerId}` | ✅ | |
| `${containerEnv:VAR[:default]}` | ⚠️ partial | Resolved post-create but not re-applied on `Exec`; [#25](https://github.com/crunchloop/devcontainer/issues/25) |

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
| Top-level unknown-field warnings | ✅ | Nested unknown-field strictness tracked in [#27](https://github.com/crunchloop/devcontainer/issues/27) |
| GPG / SSH agent forwarding | ❌ | [#28](https://github.com/crunchloop/devcontainer/issues/28) |

**Out of scope** (➖): Templates spec, dotfiles repos, IDE injection hooks,
Kubernetes / podman drivers. See [PRD §4](#design-docs) for the full
non-goals list.

## Install

```bash
go get github.com/crunchloop/devcontainer
```

Requires:

- Go 1.25+
- Docker daemon socket reachable
- Docker Compose v2 plugin (only for `dockerComposeFile` source)

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
- `runtime` — container backend abstraction (`Runtime`, `ComposeRuntime`)
- `runtime/docker` — Docker Engine API implementation (uses `moby/moby/client`)
- `feature` — feature resolution (OCI / HTTPS / local), DAG ordering, dockerfile generation
- `compose` — `dockerComposeFile` parsing via `compose-spec/compose-go`, override-file generation

## Tests

```bash
make test              # unit tests (~140 cases, ~3s)
make test-integration  # integration tests against real Docker (~30s)
make lint              # golangci-lint
```

The integration suite (build tag `integration`) exercises real Docker:
pulls public images from GHCR, builds Dockerfiles, runs feature install
scripts, drives `docker compose up/down`. Skipped automatically if a
Docker daemon isn't reachable.

## Design docs

PRD and design docs are kept in `design/` (private during incubation).
They will move to `docs/` and become public alongside the v0.1.0 tag.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports welcome via
[GitHub issues](https://github.com/crunchloop/devcontainer/issues).

## License

[Apache License 2.0](LICENSE).
