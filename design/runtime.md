# Design — Runtime layer

**Status:** Draft for review
**Date:** 2026-05-06
**Scope:** the layer between `ResolvedConfig` and the container engine. Defines
the `Runtime` interface, the `Workspace` value object, the high-level
`Engine`, container-context substitution, lifecycle idempotency, and the M2
shipping subset.

Companion to `design/resolved-config.md`. The high-level Runtime / Engine
shape is fixed; this doc fills in the operational details.

---

## 1. Layering

```text
┌──────────────────────────────────────────────────────────────────┐
│ Caller (your application, CLI, tests, ...)                               │
│   eng.Up(ctx, UpOptions{...}) → *Workspace                       │
└──────────────────────────────────────────────────────────────────┘
                              │
┌─────────────────────────────▼────────────────────────────────────┐
│ Engine (devcontainer pkg)                                        │
│   - resolves config (M1)                                         │
│   - builds image (M3)                                            │
│   - drives Runtime to create + start                             │
│   - runs lifecycle phases with idempotency markers               │
│   - manages container-context Substituter                        │
│   - emits Events                                                 │
└──────────────────────────────────────────────────────────────────┘
                              │
┌─────────────────────────────▼────────────────────────────────────┐
│ Runtime interface (runtime pkg)                                  │
│   container primitives only — no devcontainer-spec semantics     │
└──────────────────────────────────────────────────────────────────┘
                              │
┌─────────────────────────────▼────────────────────────────────────┐
│ DockerRuntime (runtime/docker)                                   │
│   uses the Docker Engine API Go SDK                              │
└──────────────────────────────────────────────────────────────────┘
```

**Strict separation:** `Runtime` knows nothing about `ResolvedConfig`,
features, lifecycle phases, idempotency, or substitution. It speaks containers
and images. Spec semantics live in `Engine` and helper packages.

This makes the interface stable enough that a future `runtime/kubernetes`
or `runtime/cli` slots in without touching anything above.

## 2. `Runtime` interface

```go
package runtime

type Runtime interface {
    BuildImage(ctx context.Context, spec BuildSpec, events chan<- BuildEvent) (ImageRef, error)
    PullImage(ctx context.Context, ref string, events chan<- BuildEvent) (ImageRef, error)

    RunContainer(ctx context.Context, spec RunSpec) (*Container, error)
    StartContainer(ctx context.Context, id string) error
    StopContainer(ctx context.Context, id string, opts StopOptions) error
    RemoveContainer(ctx context.Context, id string, opts RemoveOptions) error

    ExecContainer(ctx context.Context, id string, opts ExecOptions) (ExecResult, error)
    InspectContainer(ctx context.Context, id string) (*ContainerDetails, error)
    InspectImage(ctx context.Context, ref string) (*ImageDetails, error)
    ContainerLogs(ctx context.Context, id string, w io.Writer, follow bool) error

    // FindContainerByLabel returns the most recently created container
    // matching the given label key=value, or nil if none.
    FindContainerByLabel(ctx context.Context, key, value string) (*Container, error)
}
```

Notes:

- `RunContainer` creates the container. `StartContainer` is separate so the
  Engine can attach hooks (e.g. write idempotency markers) between create
  and start if needed. v1 always creates-then-starts via `Engine.up`, but
  the split is worth keeping.
- `BuildEvent` is a small union: `BuildLog`, `BuildLayer`, `BuildCompleted`,
  `ImagePullProgress`. Mapped to public `Event`s by the Engine.
- `ExecOptions` carries Cmd, Env, User, WorkingDir, Tty, Stdin/out/err.
  Returns `ExecResult{ExitCode, Stdout, Stderr}` for the buffered case;
  streaming is via the io.Writers in options.
- `InspectImage` returns labels, env, and other image config — load-bearing
  for the `devcontainer.metadata` pre-baked-image hot path (features.md §7).

## 3. `Container` and `ContainerDetails`

```go
type Container struct {
    ID    string
    Name  string
    Image string
    State State // running | created | exited
}

type ContainerDetails struct {
    Container
    Created   time.Time
    StartedAt time.Time
    User      string
    Env       []string         // the container's effective environment
    Mounts    []MountInspect
    Labels    map[string]string
    Networks  map[string]NetworkInspect
}
```

`ContainerDetails.Env` is the **source of truth for `${containerEnv:*}`
substitution** — see §6. We read it once after start (or on each Attach)
and cache on the `Workspace`.

## 4. `Workspace` (Engine layer)

```go
package devcontainer

type WorkspaceID string

type Workspace struct {
    ID     WorkspaceID
    Config *ResolvedConfig

    // Live container handle. Always present after Up/Attach.
    Container *runtime.ContainerDetails

    // Substituter bound to the live container's env. Used by Exec and
    // RunLifecycle to resolve ${containerEnv:*} placeholders left literal
    // by Resolve.
    subst *config.Substituter
}
```

`Workspace` is **not threadsafe for mutation**. Concurrent `Exec` calls
against the same `*Workspace` are fine (they read `subst` and `Container`
which are immutable after construction). `Attach` produces a new
`*Workspace`; callers don't share workspace pointers across reattaches.

## 5. `Engine`

```go
type Engine struct {
    runtime    runtime.Runtime
    featureStore feature.Store // M3
    clock      Clock
    opts       EngineOptions
}

func New(opts EngineOptions) (*Engine, error)

// Up resolves config, builds/pulls image, creates and starts the container,
// runs lifecycle through the WaitFor phase, and returns the live workspace.
// Up is idempotent: if a container with the matching workspace label is
// already running, it Attaches and runs only the still-needed phases.
func (e *Engine) Up(ctx context.Context, opts UpOptions) (*Workspace, error)

func (e *Engine) Attach(ctx context.Context, id WorkspaceID) (*Workspace, error)
func (e *Engine) Exec(ctx context.Context, ws *Workspace, opts ExecOptions) (ExecResult, error)
func (e *Engine) ExecByID(ctx context.Context, id WorkspaceID, opts ExecOptions) (ExecResult, error)
func (e *Engine) RunLifecycle(ctx context.Context, ws *Workspace, phase LifecyclePhase) error
func (e *Engine) Down(ctx context.Context, ws *Workspace, opts DownOptions) error
```

`UpOptions` mirrors `ResolveOptions` plus runtime-side knobs:

```go
type UpOptions struct {
    LocalWorkspaceFolder string
    ConfigPath           string
    LocalEnv             map[string]string

    Events     chan<- Event   // optional; ungated channel of typed events
    PullPolicy PullPolicy     // always | ifNotPresent | never
    NoCache    bool           // pass --no-cache to image build (M3)

    // Opt-in: run initializeCommand on the host. Default off (security).
    RunInitializeCommand bool
}
```

## 6. Container-context substitution

`Resolve` (M1) leaves `${containerEnv:*}` as literal placeholders. After
`RunContainer` + `InspectContainer`, we know the container's effective env
and can resolve them.

Approach: extend `config.SubstitutionContext` with `ContainerEnv map[string]string`
(nil = leave literals). `ResolveString` already pass-throughs `${containerEnv:*}`
in M1 → swap that branch to look up in `ContainerEnv` when populated. **No
new function needed**, just feature-flag the existing one.

`Workspace.subst` is a thin closure over the merged context:

```go
type Substituter struct {
    ctx config.SubstitutionContext
}

func (s *Substituter) String(in string) (string, []config.Warning) {
    return config.ResolveString(in, s.ctx)
}

func (s *Substituter) Slice(in []string) []string { ... }
func (s *Substituter) Map(in map[string]string) map[string]string { ... }
```

`Engine.Exec` and `Engine.RunLifecycle` route every user-facing string
(cmd, args, env values) through `ws.subst.String` before handing them to
`Runtime.ExecContainer`. The `ResolvedConfig` itself is never mutated.

## 7. Lifecycle phases & idempotency

Spec semantics:

| Phase           | Runs once per...      | Marker key       |
| --------------- | --------------------- | ---------------- |
| `initialize`    | host invocation (caller opt-in) | n/a (host-side) |
| `onCreate`      | container creation    | `Created`        |
| `updateContent` | container creation OR caller-requested re-run | `Created` |
| `postCreate`    | container creation    | `Created`        |
| `postStart`     | container start       | `StartedAt`      |
| `postAttach`    | every Attach call     | always run       |

Marker storage: files inside the container at
`/.devcontainer-go/markers/<phase>`. Contents are the keying timestamp
(RFC3339Nano). On phase entry we read the marker; if absent or its
timestamp doesn't match the current container's `Created` / `StartedAt`,
we run the phase and rewrite the marker.

Why files (not container labels): labels are immutable post-create on
Docker, can't be touched per-phase. Files survive restarts and are easy
to inspect during debugging (`docker exec ... cat /.devcontainer-go/markers/...`).

Permissions: marker dir is created by the Engine via `ExecContainer` as
root (or as `containerUser` if root isn't available — fail loudly if
neither works). Markers are mode 0644.

`postAttach` has no marker — it always runs on Attach by design.

## 8. Events

Buffered `chan<- Event` supplied via `EngineOptions.Events` or per-call.
The Engine drops events on full channel rather than blocking — caller
guarantees consumption pace. M2 emits the lifecycle and container events;
build/feature/exec events arrive in M3 and beyond.

Event ordering: monotonic `Seq uint64` field, allocated under a single
atomic counter on the Engine. Consumers can sort/replay if they multiplex.

## 9. Container naming & lookup

- **Name:** `devcontainer-<devcontainerId>` (deterministic from the workspace
  id; collision-free across workspaces).
- **Labels** written on every container we create:
  - `dev.containers.id` = `devcontainerId`
  - `dev.containers.localWorkspaceFolder` = absolute host path
  - `dev.containers.configPath` = absolute config path
  - `dev.containers.engine` = `devcontainer-go/<version>`
- `Engine.Attach(id)` finds via `FindContainerByLabel("dev.containers.id", id)`.
  We do not rely on the name for lookup — labels are the source of truth.

## 10. Networking

v1 uses Docker's default bridge network. Per-workspace networks (so two
workspaces of the same project don't collide on hostname or service-name
DNS) are an M4 concern when compose lands. Documented as a known
limitation.

## 11. Errors

Every Runtime method returns concrete error types where useful:

- `*runtime.ImageNotFoundError`
- `*runtime.ContainerNotFoundError`
- `*runtime.ExecFailedError{ExitCode int, Stderr string}`
- `*runtime.DaemonUnavailableError` — when the Docker daemon socket is unreachable

Engine wraps with phase-aware types (see `design/structured-errors.md`):

- `*LifecycleError{Phase LifecyclePhase, Cmd, ExitCode, Stderr}`
- `*ContainerStartFailedError`

## 12. M2 ship target

PR4 (M2 first PR): docker runtime skeleton + Engine.Up for image source.

In scope:
- `runtime` package with the interface and shared types from §2–§3.
- `runtime/docker` with `BuildImage` (no-op for now), `PullImage`,
  `RunContainer`, `StartContainer`, `StopContainer`, `RemoveContainer`,
  `ExecContainer`, `InspectContainer`, `FindContainerByLabel`.
  `BuildImage` is implemented but only for the trivial "pull and tag"
  path; real Dockerfile build is M3.
- `Engine.Up` for `*ImageSource` only. `*BuildSource` and `*ComposeSource`
  return a typed `ErrNotImplemented`.
- `Engine.Exec`, `Engine.Down`, `Engine.Attach`.
- `Engine.RunLifecycle` for all phases with marker files. PR4 does NOT
  invoke lifecycle automatically from Up; that's PR5 once we're confident
  the container path is stable.
- Container-context substitution wired (extend `SubstitutionContext`).

Out of scope for PR4:
- Auto-running lifecycle from `Engine.Up` (PR5)
- Build-source and compose-source paths (M3, M4)
- Features (M3)
- Idempotency markers (PR5; PR4 always runs phases)

### Integration test for PR4

One end-to-end flow against a real Docker daemon, behind
`//go:build integration`:

1. Resolve a fixture devcontainer.json (`image: mcr.microsoft.com/devcontainers/base:debian`).
2. `Engine.Up` → workspace returned, container running.
3. `Engine.Exec` running `whoami` → expected `vscode` (the image's default user).
4. `Engine.Exec` echoing `${containerEnv:HOME}` (after substitution) → non-empty path.
5. `Engine.Down` → container stopped and removed; verify with `docker ps -a`.

Pulled image is reused across test runs (the test never removes it).

## 13. Decisions

Resolved during runtime design review (2026-05-06):

1. **Docker SDK: `github.com/moby/moby/client`** — the newer SDK split from `docker/docker`; avoids dragging both
   `docker/docker` + `moby/moby` near-duplicate trees into consumers'
   go.mod files. Choice documented in `runtime/docker` package doc.
2. **Up re-attach: restart by default, opt-in `Recreate`.** Existing
   stopped container is restarted (preserving in-container state);
   `postCreate` skipped via marker, `postStart` + `postAttach` run.
   `UpOptions.Recreate: true` stops + removes + creates fresh.
   Decision matrix in §5. Config-drift detection (auto-recreate on
   `devcontainer.json` change) deferred — explicit caller action only
   in v1.
3. **Cancellation: wrap streaming reads with a `cancellableCopy` helper.**
   Goroutine pattern that closes the `ReadCloser` on `ctx.Done()` and
   returns `ctx.Err()` instead of the resulting "use of closed connection"
   error. Applied to `PullImage`, `BuildImage` (M3), and `ContainerLogs`
   (follow mode).
4. **Marker format: versioned JSON.** Single struct in `lifecycle/marker.go`:
   ```json
   {
     "v": 1,
     "phase": "postCreate",
     "containerCreatedAt": "...",
     "ranAt": "...",
     "durationMs": 8124,
     "exitCode": 0
   }
   ```
   Idempotency keying compares `containerCreatedAt` to the live
   container's `Created` timestamp (or `startedAt` for `postStart`).
   Schema is additive going forward.
5. **`workspaceMount` default: match devpod.** If
   `ResolvedConfig.WorkspaceMount` is non-nil, caller owns; otherwise
   default bind from `LocalWorkspaceFolder` → `ContainerWorkspaceFolder`
   with `Propagation: "consistent"` on non-Linux hosts (no-op on
   modern Docker Desktop with VirtioFS, but harmless and matches devpod
   for migration least-surprise from devpod-style tooling).
