# Design — apple-container Runtime backend

**Status:** Draft for review
**Date:** 2026-05-14
**Scope:** a second `Runtime` implementation that drives Apple's `container`
stack on macOS (Sequoia+ / arm64) as an alternative to `runtime/docker`.
Defines the cgo + Swift bridge architecture, the daemon dependency, version
pinning discipline, build constraints, and the M6 shipping subset.

Companion to `design/runtime.md` (Runtime / Engine layering, locked
decisions). The backend implements the existing `Runtime` interface
from `runtime/runtime.go` without changing its shape; one planned
engine-side adjustment is the `updateRemoteUserUID` short-circuit
described in §13.8 (driven by a small capability flag on `Runtime`,
not a new method).

---

## 1. Layering

```text
┌──────────────────────────────────────────────────────────────────┐
│ Engine (devcontainer pkg)                                        │
│   selects runtime.Runtime impl by EngineOptions                  │
└──────────────────────────────────────────────────────────────────┘
                              │
┌─────────────────────────────▼────────────────────────────────────┐
│ Runtime interface (runtime pkg)                                  │
│   unchanged — both DockerRuntime and AppleContainerRuntime       │
│   satisfy it; ComposeRuntime sub-interface is Docker-only        │
└─────────────────────────────┬────────────────────────────────────┘
                              │
┌─────────────────────────────▼────────────────────────────────────┐
│ runtime/applecontainer (Go, build-tagged darwin && arm64)        │
│   thin cgo wrapper; marshals options to JSON, calls @_cdecl      │
│   exports, decodes responses, owns the handle table for          │
│   cancellation and streams.                                      │
└─────────────────────────────┬────────────────────────────────────┘
                              │   cgo
┌─────────────────────────────▼────────────────────────────────────┐
│ libACBridge.dylib (Swift, this repo's applecontainer-bridge/)    │
│   imports apple/container's ContainerAPIClient + supporting      │
│   modules; wraps async APIs as fire-and-forget @_cdecl funcs;    │
│   pipes for high-throughput streams (exec stdio, logs, build).   │
└─────────────────────────────┬────────────────────────────────────┘
                              │   XPC (mach service)
┌─────────────────────────────▼────────────────────────────────────┐
│ container-apiserver (Apple's daemon; launchd LaunchAgent)        │
│   owns VMs (Virtualization.framework), virtiofs, image cache,    │
│   networks, persistent state. Installed via `brew install        │
│   container`. Started via `container system start`.              │
└──────────────────────────────────────────────────────────────────┘
```

**Strict separation, same as runtime.md §1:** `runtime/applecontainer` knows
nothing about `ResolvedConfig`, features, lifecycle phases, idempotency,
or substitution. It speaks the `Runtime` interface.

## 2. What's in Apple's stack

Pinned target version: **`apple/container` 0.12.x** (locked exact in
`Package.swift`; see §5).

| Apple module / product             | What it gives us                                        |
| ---------------------------------- | ------------------------------------------------------- |
| `ContainerAPIClient`               | Swift client for the apiserver — `ContainerClient`, `ClientHealthCheck.ping`, image / network / sandbox sub-clients. |
| `ContainerXPC`                     | XPC transport (mach service `com.apple.container.apiserver`). EUID-only auth — no entitlement required on callers (`XPCServer.swift:178-193`). |
| `ContainerBuild`                   | BuildKit-style builder. Spoken to over gRPC; runs in its own VM started via `container builder start`. |
| `ContainerizationOCI`              | OCI image refs, registry interaction. |
| `Containerization` (core)          | VM lifecycle, process I/O. Used transitively. |
| `ContainerResource`                | Shared types — `ContainerListFilters`, `ContainerSnapshot`, etc. |

The CLI binary `/usr/local/bin/container` (or brew's
`/opt/homebrew/bin/container`) is a thin client over `ContainerAPIClient`
— same Swift APIs we call. It's optional for our runtime; only the daemon
is mandatory.

## 3. Bridge architecture

cgo into a SwiftPM dynamic-library product (`libACBridge.dylib`) that
imports `ContainerAPIClient` and exposes a small C ABI of `@_cdecl`
exports. Hand-written header (`include/ac_bridge.h`) — no `swift-bridge`
or codegen.

Surface (representative; full set lands in PR-A..H):

```c
typedef uint64_t ac_call_t;
typedef void (*ac_done_cb)(void* ud, int32_t exit_code, const char* err);
typedef void (*ac_stream_cb)(void* ud, int32_t fd, const uint8_t* data, size_t len);

const char* ac_version(void);
const char* ac_ping(int timeout_seconds);

ac_call_t ac_inspect_container(const char* id, ac_done_cb, void* ud, char** json_out);
ac_call_t ac_inspect_image(const char* ref, ac_done_cb, void* ud, char** json_out);
ac_call_t ac_run(const char* spec_json, ac_done_cb, void* ud, char** id_out);
ac_call_t ac_exec(const char* id, const char* opts_json,
                  ac_done_cb done, void* ud,
                  int* stdin_fd_out, int* stdout_fd_out, int* stderr_fd_out);
ac_call_t ac_logs(const char* id, bool follow, ac_done_cb, void* ud, int* fd_out);
ac_call_t ac_build(const char* spec_json, ac_stream_cb progress, ac_done_cb, void* ud);

void ac_call_cancel(ac_call_t);
void ac_free(void* p);
```

Key rules:

- **Every call is fire-and-forget.** Returns a handle; callbacks fire from
  Swift `Task`s later. The `@_cdecl` function must return fast — never
  block the OS thread Go gave it.
- **Hot-path streams go through OS pipes**, not callbacks. Swift opens
  pipes, hands fds to Go; Go reads/writes them with `os.NewFile(fd, ...)`.
  This keeps the cgo boundary cold even during heavy stdio or log follow.
- **Control / completion / errors** use callbacks (low frequency).
- **Cancellation:** `ac_call_cancel(handle)` calls `Task.cancel()` on the
  stored task. Go side calls this from a goroutine watching `ctx.Done()`.
- **Memory ownership:** all `const char*` returns are `strdup`'d; caller
  frees via `ac_free`. JSON-shaped responses use intermediate `char**`
  out-params to keep the cgo signature simple.

## 4. Daemon dependency

The `container-apiserver` daemon is **mandatory**, analogous to needing
`dockerd` for `runtime/docker`. Same shape as Docker Desktop: the CLI
(or our bridge) is a client; containers run inside the daemon's
virtualized environment.

Why we can't embed the server side:

1. The apiserver and its ancillary services (`container-network-vmnet`,
   `container-core-images`) are launchd-managed LaunchAgents that own
   host-level state (vmnet routing, on-disk image cache) which must be
   shared across all clients on the user's machine.
2. Embedding it in our binary would require codesigning with
   `com.apple.security.virtualization` and friends — Developer ID +
   provisioning profile + the whole release dance.
3. Two engines fighting for the same image cache directory is a
   data-corruption hazard.

**Discovery:** `Engine.New` (when constructed with the apple-container
runtime) calls `ClientHealthCheck.ping` with a short timeout. On
failure the runtime constructor returns a typed
`*runtime.DaemonUnavailableError` (already defined for the Docker
path; we reuse it) with a message pointing the user at
`container system start`.

## 5. Version pinning

`apple/container` is pre-1.0. Spike findings showed both schema-level
and source-level breakage between minor versions:

- 0.4.1 → 0.12.3 added `apiServerBuild`, `apiServerAppName`, `logRoot`
  fields to `SystemHealth`. An older Swift client decoding a newer
  daemon response fails at parse time with
  `"failed to decode apiServerBuild in health check"`.
- The product name + primary type renamed across the same window:
  0.4.1 exported product `ContainerClient` with type `ClientContainer`;
  0.12.x exports product `ContainerAPIClient` with type
  `ContainerClient`.

**Rule:** `Package.swift` uses `.package(url: ..., exact: "0.12.3")` —
SwiftPM's `exact:` requires a fully-qualified semver and rejects
wildcards. Bumps are their own PRs, gated by re-running the parity
integration suite (PR-H) against the new version. We do NOT use `from:`
or `branch:`.

## 6. Build & distribution

Build pipeline:

1. `make bridge` → `swift build -c release` in `applecontainer-bridge/`
   → `libACBridge.dylib` in `.build/arm64-apple-macosx/release/`.
2. Go build embeds the dylib bytes via `go:embed`. At process start,
   the runtime constructor writes the dylib to a per-user cache path
   (`os.UserCacheDir()/devcontainer-go/applecontainer/<sha256>.dylib`),
   skips the write if the hashed file already exists, then `dlopen`s
   it.
3. Cgo `LDFLAGS` references the dylib at build time via an absolute
   path for the link, but the runtime loader uses the embed+dlopen
   path — this avoids users needing the build-time path at runtime.

Rejected alternatives:

- **Ship as a separate file next to the binary.** Forces every consumer
  to manage two artifacts; awkward for consumer release pipelines that
  expects a single binary.
- **Static link.** SwiftPM's static product mode is fragile when the
  graph pulls Foundation/XPC; the Swift runtime libs in `/usr/lib/swift`
  are dynamic on the system anyway, so we don't save the dlopen.
- **Require `brew install applecontainer-bridge`.** Cleanest builds,
  worst UX.

## 7. Platform constraints

- Whole `runtime/applecontainer` package is `//go:build darwin && arm64`.
- macOS 15+ is a hard runtime floor (apple/container requirement).
- Swift 6.x toolchain required to build the bridge.
- CI: add a macOS arm64 runner for the M6 PRs. The rest of the repo
  continues to build cross-platform on Linux runners — the build tag
  excludes the whole package on non-darwin.
- Cross-compilation from Linux is **not supported** for binaries that
  include this runtime. Consumer release pipelines build the macOS binary
  on a macOS runner already; no change required there.

## 8. Mapping to `Runtime` methods

Every method on `runtime.Runtime` (runtime/runtime.go:83-129) is
reachable through Apple's stack. Direct mapping:

| `Runtime` method        | Apple API                                          | Bridge shape                 | Notes |
| ----------------------- | -------------------------------------------------- | ---------------------------- | ----- |
| `BuildImage`            | `ContainerBuild` (BuildKit gRPC)                   | callback stream for progress | Builder runs in a separate VM; clear error if not started. Heaviest PR (PR-G). |
| `PullImage`             | `ImagePull` via images service                     | callback stream for progress | low risk |
| `RunContainer`          | `ContainerClient.create` + create-not-start path   | sync request/reply           | Mounts, env, labels, runArgs map cleanly. Verify UID semantics (§11). |
| `StartContainer`        | `ContainerClient.start`                            | sync                         | low risk |
| `StopContainer`         | `ContainerClient.stop`                             | sync                         | Timeout maps directly. |
| `RemoveContainer`       | `ContainerClient.delete` / `kill`                  | sync                         | Force + remove-volumes flags. |
| `ExecContainer`         | `ContainerExec` flow with `ProcessIO`              | pipe fds + callback          | Hardest. TTY mode supported by `config.terminal`. Cancellation via Task. |
| `InspectContainer`      | `ContainerClient.get` / `inspect`                  | sync, JSON response          | Confirm Created/StartedAt/ExitCode/FinishedAt all present. |
| `InspectImage`          | image service `inspect`                            | sync, JSON response          | Labels round-trip — load-bearing for the `devcontainer.metadata` fast path. |
| `ContainerLogs`         | `ContainerLogs` flow                               | pipe fd                      | Follow + non-follow. |
| `FindContainerByLabel`  | `ContainerClient.list` + client-side label filter  | sync                         | If Apple adds server-side filtering, switch. |
| `ComposeRuntime` (sub)  | not implemented                                    | type assertion fails         | See §9. |

## 9. `ComposeRuntime` not implemented

Apple's stack has no compose concept. The `ComposeRuntime` sub-interface
(`runtime/runtime.go:19-35`) is intentionally a separate type that
`Engine.Up` type-asserts before invoking the compose path
(`runtime/runtime.go:9-18` comment). With `AppleContainerRuntime` the
assertion fails and the engine returns `ErrNotImplemented` — same
behavior as v1's `runtime/docker` for the build/compose source
distinction.

This is documented here so a future contributor doesn't try to wire
compose-go into this backend without a separate design conversation.
Pre-baked compose images (`image: foo:bar` on a compose service) are
also out — the spec edge case of "one service, image-only, treat as
single container" is a v2+ conversation.

## 10. Spike findings

A two-day spike (this branch, `examples/applecontainer-spike/`) proved
the load-bearing assumptions before writing this design:

1. **cgo + Swift dynamic library linking works.** `@_cdecl` exports
   callable from Go via cgo with rpath set to the dylib build dir.
   `libswift*` runtime libs resolve from `/usr/lib/swift` without
   special flags.
2. **`apple/container` SwiftPM dep resolves and builds inside our
   bridge.** Full dependency graph (~1000 compile units) builds in
   ~3min cold, ~6s incremental.
3. **No codesigning or entitlements required for clients.** Ad-hoc
   signed Go binary (Go's default) successfully establishes the XPC
   connection. The apiserver checks EUID match only
   (`XPCServer.swift:178-193`) — no Team ID, no entitlement string.
4. **Round-trip on 0.12.3 returns the full `SystemHealth` schema.**
   `ClientHealthCheck.ping` from a cgo-linked Go binary:

   ```text
   ping ok: SystemHealth(
     appRoot: file:///.../Application Support/com.apple.container/,
     installRoot: file:///opt/homebrew/Cellar/container/0.12.3/,
     apiServerVersion: "container-apiserver version 0.12.3 ...",
     apiServerBuild: "release",
     apiServerAppName: "container-apiserver",
     ...
   )
   ```

5. **Version-skew failure observable.** Same code against 0.4.1 daemon
   surfaced exactly the schema mismatch the §5 rule prevents —
   evidence the pinning discipline is the right one.

`ClientContainer.list()` against a clean daemon returned `count=0`
as expected.

### 10.1 Validation probes (2026-05-14)

After PR-A landed, a throwaway probe branch (`m6/probe-validation`)
extended the bridge with `ac_probe_list` / `_get` / `_stop` / `_delete`
/ `_exec` exports to validate the design's most uncertain bets before
committing to PR-B..H. Three probes ran against a live daemon; results
informed §11.1 (resolved) and confirmed PR-D's pipe pattern.

1. **Lifecycle round-trip — green.** Bridge drove List → Get → Stop →
   Delete on a CLI-created `alpine sleep 120` container. JSON encoding
   of `[ContainerSnapshot]` round-tripped cleanly into Go's typed
   deserializer; mutations took effect (subsequent list confirmed
   removal). Validates cgo handles both complex XPC responses and
   stateful operations.

2. **Exec stdin/stdout/exit-code — green.** Probe wrapped
   `ContainerClient.createProcess` + `ProcessIO` with three subcases:
   stdout-only (`echo` captured), stdin → stdout roundtrip (`cat` with
   a marker string roundtripped exactly), exit-code propagation
   (`exit 42` returned `42`). The pipe-fd pattern with Swift-side
   `Pipe()` + `closeAfterStart` + `readToEnd()` works as PR-D will
   need. No async/cancellation glue yet — that's PR-D's work.

3. **Bind mount UID — green, with design impact.** Created host
   tmpdir with marker file (`uid=501 gid=20`), bind-mounted into
   container as `/mnt/probe`. As root inside the VM: files appeared
   `root:root`. As `uid=1000`: same files appeared `nonroot:nonroot`,
   `cat` read OK, `echo > write.txt` returned `WRITE_OK`. Virtiofs
   is **identity-permissive** — every container-side user sees
   themselves as owner of bind-mounted files, regardless of host UID.
   This is the same pattern Docker Desktop's VirtioFS uses.
   Resolution captured in §11.1 and §13.8.

4. **BuildKit gRPC** — not run. Reachability isn't in doubt (the CLI
   uses the same gRPC client); only event-mapping shape is open, and
   that's a PR-G design detail, not a blocker.

## 11. Open questions

These are integration details to resolve during PR-A..H, not blockers
for the design itself:

1. **~~Bind-mount UID mapping across the VM boundary.~~ RESOLVED
   2026-05-14 (§10.1 probe 3).** Apple's virtiofs is
   identity-permissive: every container-side user sees themselves as
   owner of bind-mounted files, regardless of host UID. Files written
   from inside the VM appear to the host as owned by the host user
   (because the host kernel attributes writes by inode owner, which
   the mount layer reflects back). The `updateRemoteUserUID` dance
   from `useruid.go` is unnecessary for this backend — see decision
   §13.8. Perf characterization on a large monorepo deferred to PR-C's
   integration test.
2. **Builder VM lifecycle.** `container builder start` boots a
   dedicated VM. Open questions: cold-start latency, can it stay warm
   across builds, what happens to in-flight builds if it crashes,
   how do we surface "builder not started" cleanly. PR-G owns this.
3. **Exec stream cancellation semantics.** Does cancelling the Swift
   `Task` propagate to a SIGTERM of the in-VM process, or do we leak
   long-running execs? Critical for long-running readiness probes and attach patterns. PR-D owns this.
4. **Distribution codesigning.** Ad-hoc signing was sufficient for the
   spike, run locally. Need to confirm that consumers receiving the
   binary via the consumer's release pipeline (signed Developer ID +
   notarized) don't hit any TCC / Gatekeeper friction loading the
   embedded dylib. Test on a clean machine before M6 ships.

## 12. M6 ship target

PR-A..H land sequentially; each gates on the previous. Detailed
breakdown in `design/status.md`.

In scope:

- `runtime/applecontainer/` Go package implementing `runtime.Runtime`.
- `applecontainer-bridge/` SwiftPM package producing `libACBridge.dylib`.
- Embed-and-dlopen distribution path.
- `Engine.New` integration: opt-in via `EngineOptions.Runtime` or a
  factory helper (`devcontainer.NewAppleContainer(...)`).
- Parity integration suite re-running M2/M3 fixtures against the new
  backend, behind a build tag.

Out of scope for M6:

- `ComposeRuntime` (see §9).
- `forwardPorts` actuation — same as v1's Docker backend.
- Linux / x86 hosts.
- Downstream CLI cutover — consumers pick a backend;
  we just provide one.

## 13. Decisions

Resolved during this design review (2026-05-14):

1. **Bridge: cgo + Swift dynamic library.** No sidecar process, no
   shell-out to the `container` CLI. Single binary at the Go side, one
   embedded dylib. Spike evidence in `examples/applecontainer-spike/`.
2. **SwiftPM dep pinned via `exact:`.** Bumps are deliberate PRs that
   re-run PR-H's parity suite. Documented evidence of breakage in §5.
3. **Daemon required, probed at runtime construction.** Constructor
   calls `ClientHealthCheck.ping`; returns
   `*runtime.DaemonUnavailableError` if missing. No fallback, no
   auto-start.
4. **Dylib distribution: `go:embed` + dlopen.** Hash-named file in
   `os.UserCacheDir()`. Single artifact for consumers; no install
   step beyond `brew install container` for the daemon itself.
5. **`ComposeRuntime` not implemented.** Type assertion fails, engine
   returns `ErrNotImplemented`. Documented per §9 so future
   contributors don't backtrack into the question.
6. **Build constraints: `//go:build darwin && arm64`, macOS 15+.**
   Cross-compilation from non-darwin hosts not supported when this
   runtime is compiled in. Rest of repo unaffected.
7. **API surface marked stable from M6 v0.1.** The Go-side
   `runtime.Runtime` interface doesn't change for this backend; the
   bridge dylib version is an internal detail not exposed to consumers.
8. **Skip `updateRemoteUserUID` for apple-container.** Validated by
   the §10.1 probe 3 — virtiofs is identity-permissive, so the UID
   reconciliation that `runtime/docker` performs is unnecessary and
   would be harmful (modifies the in-container user's UID for no
   benefit). PR-C wires `Engine.Up` to short-circuit the
   `updateRemoteUserUID` path when the runtime is apple-container.
   Mechanism: a small capability flag on the `Runtime` interface
   (rather than a type assertion against an apple-container-only
   marker interface) so future backends can opt in without coupling
   the engine to backend identity.
