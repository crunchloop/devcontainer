# devcontainer

[![Status: alpha](https://img.shields.io/badge/status-alpha-orange)](#status)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

A programmatic Go runtime for [Dev Containers](https://containers.dev/) — embed
the full devcontainer lifecycle (resolve, build, up, exec, down) into your Go
application without shelling out to the Node `@devcontainers/cli`.

## Status

**Alpha.** API is not stable. The `events` package is explicitly experimental
until v1.0.0. See [PRD.md](PRD.md) for scope, roadmap, and design rationale.

## Why

The reference [`@devcontainers/cli`](https://github.com/devcontainers/cli) is
a Node binary. Embedding it in a Go service means a Node runtime dependency,
opaque failure modes (success exit codes with `outcome:error` JSON on stdout),
and CLI-flag-shaped APIs for every interaction. This library is a clean Go
implementation of the spec, designed to be embedded.

## Install

```bash
go get github.com/crunchloop/devcontainer
```

Requires Go 1.24+ and Docker (with the Compose v2 plugin if you use compose
sources).

## Quick start

```go
// See examples/minimal for a runnable version.
import "github.com/crunchloop/devcontainer"

eng, err := devcontainer.New(devcontainer.EngineOptions{})
if err != nil { /* ... */ }

ws, err := eng.Up(ctx, devcontainer.UpOptions{
    WorkspaceFolder: "/path/to/project",
})
```

## Scope

Implements the [Dev Container spec](https://containers.dev/implementors/spec/)
subset that real projects use: image / Dockerfile / compose sources, features
(OCI, HTTPS, local), all five lifecycle phases, mounts, env, remoteUser,
substitution. Templates spec, dotfiles, IDE/SSH integration, and Kubernetes
drivers are out of scope for v1.

Full scope, non-goals, and roadmap: [PRD.md](PRD.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache License 2.0](LICENSE).
