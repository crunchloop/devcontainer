# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.3] - 2026-05-13

### Fixed

- **config** — substitute `${devcontainerId}`,
  `${localWorkspaceFolder}`, `${localEnv:*}` and friends in feature-
  and base-image-contributed metadata (mount sources, env values,
  lifecycle commands). Previously only the user's `devcontainer.json`
  was resolved by `ResolveBytes`; fields folded in afterwards by
  `MergeMetadata` kept their literal `${...}` tokens and flowed into
  `ContainerCreate`, causing Docker to reject mounts like
  `dind-var-lib-docker-${devcontainerId}` with "includes invalid
  characters for a local volume name". `MergeMetadata` now takes a
  `SubstitutionContext` and resolves layer-contributed strings before
  folding them in. (#53)

## [0.1.2] - 2026-05-12

### Fixed

- **runtime/docker** — switch image builds to BuildKit
  (`Version: build.BuilderBuildKit`). The classic builder synthesizes
  one intermediate container per Dockerfile step and routes every
  container API through dockerd's authorization pipeline; behind an
  authz plugin (e.g. DAP's `dap-authz`) this turned a sub-second
  build into a multi-minute one (~140× slowdown observed on a 7-step
  Dockerfile against a 7.7 GB base). BuildKit uses a single
  streaming session and is unaffected. BuildKit is now a hard
  requirement — Docker Engine has shipped with it enabled by default
  since 23.0 (Feb 2023). (#47)
- **runtime/docker** — pre-pull `FROM` images via the classic
  `ImagePull` API before invoking `ImageBuild`. BuildKit refuses
  remote metadata resolution without an active session ("no active
  sessions"), even for fully anonymous public-image pulls; seeding
  the local image store side-steps the session requirement without
  pulling `github.com/moby/buildkit` in as a direct dep. (#47)
- **runtime/docker** — preserve symlinks in the build-context tar.
  `tarDirectory` was emitting `TypeSymlink` entries with an empty
  `Linkname` (passing `""` to `tar.FileInfoHeader` instead of
  `os.Readlink(path)`); some tar readers reject those as malformed
  and abort the build mid-stream, which broke compose-primary
  contexts containing `node_modules/.bin/*` or similar bin-symlinks.
  (#47)
- **useruid / feature** — drop the
  `# syntax=docker/dockerfile:1.4` directive from generated
  Dockerfiles. The instructions in use (`ARG`, `FROM $ARG`, `USER`,
  `COPY`, `RUN`) are all handled by buildkit's built-in frontend;
  declaring an external frontend forces buildkit to pull
  `docker/dockerfile:*` before parsing, which needs a session for
  credential forwarding (we don't open one) and hangs indefinitely
  behind broken registry mirrors. Test guards added so the directive
  can't be reintroduced. (#47, #48)
- **events** — populate `BuildCompletedEvent.DurationMs`. The field
  was declared but never set in the event-bus translator, so every
  completion shipped with `DurationMs == 0` regardless of actual
  wall-clock duration. Duration is now measured in the bus (start
  stamped in `BuildChan`, reset on every call so each build is timed
  independently). (#46)

### Build

- **ci** — bump `actions/setup-go` to v6, `actions/checkout` to v6,
  `golangci/golangci-lint-action` to v9. (#40, #41, #42)

## [0.1.1] - 2026-05-11

### Fixed

- **LICENSE** — restore canonical Apache 2.0 text. The previous file had
  wording deviations (§4(c) trademarks clause, §9 heading and body) that
  caused pkg.go.dev's license detector to classify the module as UNKNOWN
  and hide all generated documentation. (#43)

### Changed

- **engine** — emit `ConfigWarningEvent` for warnings appended after
  `Resolve` (feature option validation, DAG depth, post-fetch re-Order)
  so the event stream matches `Workspace.Config.Warnings`. (#39)

### Build

- **Makefile** — add `make tools` target installing `golangci-lint`
  pinned to `GOLANGCI_LINT_VERSION` (kept in sync with CI). (#39)
- **ci** — pin `golangci-lint` to `v2.5.0` and set
  `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24=true` to silence the Node 20
  deprecation banner ahead of the 2026-06-02 default flip. (#39)
- **dependabot** — weekly `gomod` + `github-actions` updates, grouped
  for `moby/*` and `go.opentelemetry.io/*` lockstep packages. (#39)

### Docs

- **workspace** — document the `Config` asymmetry between `Up` (full)
  and `Attach` (minimal: substituter-driving fields only). (#39)

## [0.1.0] - 2026-05-11

Initial public release. A programmatic Go runtime for [Dev Containers](https://containers.dev/)
— resolve, build, up, exec, lifecycle, down — embeddable into Go applications without
shelling out to `@devcontainers/cli`.

### Added

- **Config** — JSONC parsing of `devcontainer.json` (comments, trailing commas);
  full host- and container-context variable substitution
  (`${localWorkspaceFolder[Basename]}`, `${containerWorkspaceFolder[Basename]}`,
  `${localEnv:VAR[:default]}`, `${containerEnv:VAR[:default]}`, `${devcontainerId}`);
  warnings for unknown / deprecated fields (top-level and nested).
- **Sources** — `image`, `build` (Dockerfile + `args` + `target` + `cacheFrom`),
  `dockerComposeFile` (+ `service`, `runServices`) via compose-spec/compose-go and
  a `docker compose` shell-out.
- **Features** — OCI, HTTPS, and local resolvers with content-addressed disk cache;
  DAG ordering (`dependsOn`, `installsAfter`, `overrideFeatureInstallOrder`); options
  merge with enum/type validation; pre-baked-image hot path via the
  `devcontainer.metadata` label.
- **Container config** — `mounts`, `workspaceFolder`, `workspaceMount`, `runArgs`,
  `containerEnv`, `remoteEnv`, `containerUser`, `remoteUser`, `updateRemoteUserUID`
  (portable: Debian + Alpine/BusyBox), `userEnvProbe` (all four modes), `init`,
  `privileged`, `capAdd`, `securityOpt`, `overrideCommand`, `shutdownAction`,
  `hostRequirements` (surfaced).
- **Lifecycle** — all six phases (`initializeCommand`, `onCreateCommand`,
  `updateContentCommand`, `postCreateCommand`, `postStartCommand`,
  `postAttachCommand`) with `waitFor`, parallel command form, and versioned
  idempotency markers; `initializeCommand` and `secretsCommand` run via an opt-in
  `HostExecutor`.
- **Exec** — structured `ExecOptions` (stdin/stdout/stderr, env, user, working dir,
  tty); `Engine.Exec(*Workspace, …)` and `Engine.ExecByID(id, …)`.
- **Image metadata** — `devcontainer.metadata` label written on build and merged
  (last-write-wins scalars, union+dedup slices, per-phase append for lifecycle
  hooks) into the resolved config on subsequent `Up` / `Attach`.
- **Events** — experimental `events` package with an `Event` marker interface,
  monotonic `Seq()` + `Time()` on every event, and 21 concrete types across
  config / feature / build / container / lifecycle / exec / engine groups; opt-in
  per-`Exec` emission via `ExecOptions.EmitEvents`.
- **Customizations** — `customizations.<tool>` passed through as
  `map[string]json.RawMessage` for callers.
- **Test surface** — unit tests (no Docker) + an integration suite (`//go:build
  integration`) against a real Docker daemon, covering image/build/compose,
  features, lifecycle, exec, substitution, and pre-baked-image hot-path.

### Known limitations

- `forwardPorts`, `portsAttributes` are parsed and surfaced on `ResolvedConfig`
  but not actuated (image/build) or enforced (compose users declare ports in
  their compose file).
- `devcontainer-lock.json` feature lockfile is not produced or honored.
- SSH/GPG agent socket forwarding is not provided by default.
- `events` is doc-tagged **experimental** until v1.0.0 — type shapes may evolve
  without a SemVer-major bump.

[Unreleased]: https://github.com/crunchloop/devcontainer/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/crunchloop/devcontainer/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/crunchloop/devcontainer/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/crunchloop/devcontainer/releases/tag/v0.1.0
