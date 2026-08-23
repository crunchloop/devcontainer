# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.1] - 2026-08-22

### Fixed

- **compose (native)** — non-primary services that declare `build:` are
  now built before the orchestrator runs, matching the shellout backend
  (where `docker compose up` builds them implicitly). Previously a
  `build:` sidecar reached `ContainerCreate` with an empty image and the
  Up failed. Compose semantics are preserved: `image:` + `build:` tags
  the built image with `image:`, a build-only service gets compose v2's
  default `<project>-<service>` name.
- **compose (native)** — containers the native orchestrator did not
  create (shellout backend, plain `docker compose up`) are now adopted
  on a non-recreate Up instead of being stopped and removed. Without the
  `dev.containers.config-hash` / `image-digest` labels there is no drift
  to detect, and removal destroyed the writable layer (in-container
  `$HOME` and friends) that the shellout path's `NoRecreate` contract
  preserved across restarts — a data-loss hazard for every workspace
  migrating from the shellout backend. Recreate-mode Ups still tear the
  whole project down first, so forced refreshes behave as before.

## [0.4.0] - 2026-06-24

### Added

- **runtime/engine** — checkpoint/restore via an optional
  `CheckpointRuntime` sub-interface plus a new **Podman backend** that
  implements it. Docker's checkpoint/restore is broken on current
  engines (the netns bind-mount on restore — upstream containerd#12141 /
  moby#37344), so Podman does the full round trip (`checkpoint --export`
  / `restore --import`: process + memory + writable rootfs in a portable,
  node-independent archive). Adds `runtime.CheckpointRuntime`,
  `CheckpointSpec`/`RestoreSpec`/`CheckpointRef`, `Capabilities.Checkpoint`,
  typed errors (`ErrCheckpointUnsupported`, `CheckpointFailedError`,
  `RestoreFailedError`), `Engine.Checkpoint`/`Engine.Restore` (Restore
  returns a fully reattached `*Workspace`), and
  `Engine.CheckpointProject`/`RestoreProject` for multi-service compose
  projects (enumerated by the `com.docker.compose.project` label). (#98)

### Fixed

- **compose** — Dev Container Feature security metadata (`privileged`,
  `init`, `capAdd`, `securityOpt`) and `entrypoint` scripts are now
  applied to docker-compose services, matching the reference
  `devcontainers/cli`. Previously the metadata was merged into
  `ResolvedConfig` but never carried onto the service, so features like
  **docker-in-docker** silently failed on compose-source devcontainers:
  the daemon came up unprivileged and its `docker-init.sh` entrypoint
  never ran. Feature entrypoints are now chained ahead of the service
  command via a generated wrapper (native and shellout paths), and
  `ContainerDetails` surfaces `Privileged`/`CapAdd`/`SecurityOpt` from
  inspect. A failed image inspect in the entrypoint-preservation fallback
  now emits a `WarnEvent` instead of silently dropping the image
  `ENTRYPOINT`. Image-source (non-compose) entrypoint chaining and
  `overrideCommand` gating remain follow-ups (#104). (#103)
- **compose/podman** — orchestrator-driven health probing on Podman.
  Podman runs a container's `HEALTHCHECK` as root and fires the first
  probe immediately at start (ignoring `start_period`), which breaks
  privilege-dropping images — e.g. RabbitMQ's `rabbitmq-diagnostics`
  probe creates a root-owned `.erlang.cookie` the gosu-dropped uid-999
  server can't read. The compose orchestrator now probes health itself on
  backends that opt in (Podman returns true; Docker and Apple unchanged),
  deferring the first probe until after the service initializes, matching
  Docker. Also fixes multi-service checkpoint/restore. See
  `design/compose-native-health.md`. (#102)

### Changed

- **deps** — bump `github.com/google/go-containerregistry` 0.21.6 →
  0.21.7. (#101)
- **dev environment / CI** — prebuild-based dev environment + CI (#88);
  pin prebuild base to bookworm (#89); use Compose v2 in
  docker-in-docker (#90); skip legacy `docker-compose` in
  docker-in-docker (#91); pin docker-in-docker to 2.x (#92); add `:sha`
  image tag and prune stale build intermediates (#93).

## [0.3.0] - 2026-06-01

### Added

- **cli** — new `cmd/devcontainer` binary, a cobra-based CLI on top
  of the engine. Commands: `up`, `exec`, `down` / `stop`,
  `read-configuration`, `run-user-commands`. Global flags:
  `--workspace-folder`, `--config`, `--runtime`
  (`docker` | `applecontainer`, default `docker`), `--log-level`.
  Logs to stderr, results to stdout, non-zero exit on failure. Tools
  wanting programmatic access keep importing the library directly;
  no outcome-envelope JSON shim. The CLI surface is new and may
  evolve before v1.0.0. (#79)
- **engine** — `Engine.Build(ctx, BuildOptions) (*BuildResult, error)`
  resolves a workspace's `devcontainer.json` and produces the final
  image (base + feature pipeline) without creating or running a
  container. Skips `updateRemoteUserUID` so build output is portable
  across hosts; refuses compose sources with a typed
  `ErrComposeSourceUnsupported`; image-source-with-no-features
  short-circuits to a pull. Single `ImageName` tag for now;
  multi-tag, `--push`, and `--cache-to` need `BuildSpec` extensions
  on both backends and are tracked as follow-ups. (#82)
- **runtime** — `ExecOptions.InitialTtySize` and `ExecOptions.ResizeCh`
  plumb host pty geometry into `ExecContainer`. Docker backend
  passes `ConsoleSize` to `ExecCreate` so the pty starts at the right
  size, and a small goroutine coalesces resize bursts onto
  `ExecResize` (keeps only the latest size when updates arrive
  faster than the daemon round-trip) so a fast drag doesn't back up.
  Mirrored on `devcontainer.ExecOptions`. Apple-container backend
  retains its prior no-resize behavior; resize support there is a
  follow-up. (#83)
- **cli/exec** — `-e` / `--env KEY=VALUE` (repeatable; split on the
  first `=` so values containing `=` survive — URLs, base64, etc.);
  `--recreate` as an alias for `--remove-existing-container` to
  match upstream `@devcontainers/cli` muscle memory. (#74, #75)
- **cli/exec** — interactive TTY exec now forwards `SIGWINCH` into
  the resize channel. Curses apps (`htop`, editors, etc.) render at
  the host terminal size and reflow when the window is resized,
  instead of being pinned at 80x24 until first redraw. (#83)

### Changed

- **cli/exec** — when `--working-dir` is unset, default `WorkingDir`
  to `cfg.ContainerWorkspaceFolder` instead of falling through to
  the image's `WORKDIR`. Stock devcontainer base images happened to
  coincide, but a custom Dockerfile with a different `WORKDIR` was
  landing `devcontainer exec` outside the project. Explicit
  `--working-dir` still overrides. (#80)
- **cli** — `--log-level` is now applied (it was previously parsed
  but never enforced). The two chatty event types —
  `events.BuildLogEvent` (raw docker build output) and
  `events.LifecycleOutputEvent` (raw script stdout/stderr) — are
  filtered out below `debug`; progress, warnings, and state changes
  still render at `info`. Unknown values are rejected at command
  entry. `trace` is reserved (currently equivalent to `debug`) so
  the flag's contract doesn't break once more granular event types
  are added. (#76)

### Fixed

- **runtime/docker** — normalize tar headers in build-context
  streaming so BuildKit's COPY vertex digest is reproducible across
  invocations and machines. `tarDirectory` previously stamped
  wall-clock mtimes (from `os.WriteFile` in synthesized contexts
  like `useruid`'s `uid-fix.sh` + `Dockerfile`) and the host
  uid/gid into every entry, so byte-identical content produced
  different digests on consecutive runs. Downstream pipelines that
  snapshot a workspace after one `Up` and restore it for a later
  `Up` were hitting full BuildKit cache misses on the second `Up`,
  re-extracting GBs of image-layer data into new snapshotter dirs
  and pushing 30Gi PVCs toward ENOSPC. `tarDirectory` now stamps
  `ModTime` to the unix epoch, zeroes `AccessTime` / `ChangeTime`,
  and clears `uid` / `gid` / `uname` / `gname` on every entry;
  `useruid` additionally `os.Chtimes`-pins its synthesized files to
  the epoch so the context's reproducibility is a local property
  of the synthesizer too. (#86)

### Build

- **deps** — bump `github.com/compose-spec/compose-go/v2` 2.10.2 →
  2.11.0 (#85); `github.com/google/go-containerregistry` 0.21.5 →
  0.21.6 (#84); `google.golang.org/protobuf` 1.34.2 → 1.36.11 (#70).
- **ci** — bump `actions/cache` 4 → 5 (#69).

## [0.2.0] - 2026-05-18

### Added

- **applecontainer** — new runtime backend targeting Apple's native
  `container` runtime on darwin/arm64. Containers are launched
  directly via a Swift bridge (`ACBridge`, exposed over a C ABI) and
  the bridge dylib is embedded into the Go binary and `dlopen`-ed at
  runtime, so callers get an Apple-native path without shelling out
  to `docker`. Includes `Inspect` and `FindContainerByLabel` parity
  with the docker backend. (#58, #59, #60, #62)
- **compose** — runtime-agnostic compose orchestrator, opt-in via
  `EngineOptions.ComposeBackend = ComposeBackendNative`. Drives
  compose entirely through `runtime.Runtime` primitives, with no
  dependency on the `docker compose` v2 plugin, and works against
  backends that lack a Docker-API equivalent — notably the
  applecontainer runtime, which the orchestrator now drives
  end-to-end. The shell-out path remains the default; existing
  callers see no behavior change. (#64)
- **runtime** — `RunSpec.MemoryBytes` and `RunSpec.NanoCPUs` let
  callers size each container's resources. Maps to
  `HostConfig.Memory`/`HostConfig.NanoCPUs` on docker, and to
  `ContainerConfiguration.resources.memoryInBytes`/`.cpus` on
  applecontainer (sizing the per-container VM at boot — Apple takes
  whole CPUs, so fractional nano-cpus round up). The native compose
  orchestrator translates `deploy.resources.limits.{memory,cpus}`
  with the legacy top-level `mem_limit` / `cpus` as fallback,
  matching docker compose's own precedence. (#68)

### Fixed

- **compose** — preserve container state across docker daemon
  restarts. The shell-out path now passes `--no-recreate` to
  `docker compose up -d` when a container is already known for the
  workspace (mirroring the upstream `devcontainers/cli` gate); the
  native orchestrator now `StartContainer`s a config-matched stopped
  container instead of stop+remove+recreating it. Both code paths
  were treating spurious config-hash drift (or a temporarily-stopped
  container left behind by a daemon restart) as a recreate signal,
  which destroyed the container's writable layer — taking
  `~/.claude/projects/`, `~/.bash_history`, install caches, and
  anything else in `$HOME` with it. (#71, #72)
- **applecontainer** — named volume mounts now resolve to the volume's
  backing image path. The Swift bridge was treating `MountType=volume`
  as a virtiofs bind, and Apple's apiserver was resolving the source
  via `URL(fileURLWithPath:)` against the launching process's CWD —
  yielding non-existent paths like
  `/private/tmp/runner/<project>_<vol>` and `errno 2` at VM
  bootstrap. The bridge now calls `ClientVolume.inspect(name)` to
  fetch the backing image path and constructs a proper
  `Filesystem.volume(...)`, matching apple/container's own CLI. (#67)

## [0.1.4] - 2026-05-13

### Fixed

- **runtime** — probe the user's interactive shell environment
  *before* running lifecycle hooks instead of only after. `Engine.Up`
  used to leave `ws.probedEnv` empty for the entire lifecycle chain,
  so `postCreateCommand` (and earlier phases) ran with only the
  container's default env plus `remoteEnv`. PATH entries published
  via `.bashrc` / `/etc/profile.d` snippets — nvm- or asdf-managed
  binaries, or pnpm installed by the official
  `ghcr.io/devcontainers-extra/features/pnpm` feature — were
  invisible to hooks, causing `command not found` failures that
  diverged from `@devcontainers/cli`. Probing happens both before
  and after lifecycle now: the pre-pass gives hooks rc-derived env,
  the post-pass picks up any rc edits the hooks themselves made for
  subsequent `Exec` calls. (#55)

### Changed

- **runtime** — `Engine.Up` now returns `(ws, err)` instead of
  `(nil, err)` when a lifecycle hook exits non-zero. The container
  is created and still running; consumers building VS Code /
  Codespaces-style UX want to surface a warning and keep the
  container reattachable rather than treat every `postCreateCommand`
  bug as a fatal Up failure. Matches `@devcontainers/cli`, which
  exits 1 on lifecycle failure but leaves the container intact.
  `*LifecycleError` is the discriminator — non-lifecycle errors
  (marker I/O, ctx cancellation, image build, feature install)
  still return `(nil, err)` unchanged. Strict callers keep current
  behavior with the usual `if err != nil { return nil, err }`. (#56)

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

[Unreleased]: https://github.com/crunchloop/devcontainer/compare/v0.4.1...HEAD
[0.4.1]: https://github.com/crunchloop/devcontainer/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/crunchloop/devcontainer/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/crunchloop/devcontainer/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/crunchloop/devcontainer/compare/v0.1.4...v0.2.0
[0.1.4]: https://github.com/crunchloop/devcontainer/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/crunchloop/devcontainer/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/crunchloop/devcontainer/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/crunchloop/devcontainer/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/crunchloop/devcontainer/releases/tag/v0.1.0
