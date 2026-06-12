# Development Container Configuration

This directory contains the devcontainer configuration for developing the
`crunchloop/devcontainer` CLI.

## Key Concepts

The devcontainer uses a **prebuild strategy** (the same one as
`crunchloop/dap`):

1. CI builds a complete development environment image using
   `devcontainer-build.json` (base image + features).
2. The image is published multi-arch (amd64 + arm64) to the GitHub Container
   Registry as `ghcr.io/crunchloop/devcontainer/devcontainer:latest`.
3. Developers pull that prebuild image via `devcontainer.json` →
   `docker-compose.yml` instead of building the toolchain locally.
4. `post-create.sh` runs lightweight, per-checkout setup (Go module download,
   Claude config persistence).

This keeps container startup fast while the toolchain stays reproducible.

## Contents

- `devcontainer.json` — local development configuration (used by developers).
- `devcontainer-build.json` — prebuild image configuration (used by CI).
- `docker-compose.yml` — runs the prebuilt `app` service.
- `post-create.sh` — per-checkout setup hook.
- `features/` — local devcontainer features:
  - `golangci-lint` — installs the linter pinned to the Makefile / `ci.yml`
    version (`v2.5.0`).
  - `claude-code` — installs the Claude Code CLI.

## Toolchain

The prebuild image provides everything the Linux CI jobs need:

- **Go** 1.26 (CI also exercises 1.25; `go.mod` declares 1.25.0).
- **golangci-lint** `v2.5.0` (keep in sync with `Makefile`'s
  `GOLANGCI_LINT_VERSION` and the `ci.yml` lint job).
- **docker-in-docker** so the integration suite
  (`go test -tags=integration ./test/integration/...`) can drive
  `docker` / `docker compose` from inside the container.
- **GitHub CLI**, **Node.js 22** (for Claude Code), and `make`.

> The Apple `container` backend (`runtime/applecontainer`) is darwin/arm64-only
> and cannot be built inside this Linux container — exactly as on the Linux CI
> jobs, where `make bridge` is a no-op. Use a native macOS checkout for that
> backend.

## Common tasks

```bash
make lint              # golangci-lint run ./...
make test              # go test -race ./...   (bridge is a no-op on Linux)
make test-integration  # docker-backed integration suite
```

## CI

- `.github/workflows/devcontainer-cache.yml` — rebuilds and republishes the
  prebuild image on pushes to `main` that touch `.devcontainer/**`.
- `.github/workflows/devcontainer-release.yml` — publishes the local
  `features/` to GHCR (manual dispatch).
