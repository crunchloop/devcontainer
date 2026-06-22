# Design — Podman backend (for checkpoint/restore)

**Status:** Draft for review
**Date:** 2026-06-19
**Scope:** how the library gains Podman support so it can checkpoint and
restore devcontainers (the `CheckpointRuntime` primitive). Commits to an
approach — **reuse the existing docker/moby backend pointed at Podman's
docker-compatible socket, and add checkpoint/restore via Podman's libpod
API** — and lays out the phased plan, the build-path risk that makes or
breaks it, file-level changes, testing, and the consumer-adoption dependency.

Companion to `design/checkpoint-restore.md` (defines the primitive and the
empirical reason docker can't do it) and `design/runtime.md` (the
`Runtime` interface).

---

## 1. Why a Podman backend

`design/checkpoint-restore.md` §2 records the empirical result: on the
current docker/containerd stack, `docker checkpoint` *dumps* fine but
`docker start --checkpoint` is broken (an open upstream bug), and
container managers (docker, nerdctl) leave state generic restore can't
reconstruct. **Podman is the only tool that does the full round trip**
(`checkpoint --export` → `restore --import`: memory resumed, networking
re-attached, into a fresh container), which is why CRIU recommends it.

To checkpoint a devcontainer with Podman, the devcontainer must be
**Podman-managed**. So the library needs a Podman backend. This doc is
about getting that backend with the least new, brittle code.

## 2. Chosen approach: reuse docker backend + libpod C/R (Option A)

> **IMPLEMENTED (2026-06-21) — fully API-driven, no CLI shell-out.** The
> backend embeds `*docker.Runtime` over the Podman docker-compat socket
> for the standard surface, and drives **build + checkpoint/restore
> through the libpod REST API on the *same* socket** via a thin stdlib
> `net/http` client (`runtime/podman/libpod.go`) — no `podman` CLI
> subprocess. The endpoint shapes were captured from the official client
> (`podman --remote --log-level=debug`) and verified end-to-end on a live
> bench: checkpoint `POST …/checkpoint?export=true` → tar response body;
> restore `POST …/containers/import/restore?import=true` with the archive
> in the body → `{"Id":…}`; build `POST …/build` with the context tar in
> the body → streamed `{"stream":…}` (last line = image id). Both
> integration tests PASS. The earlier CLI plan (below) was dropped in
> favour of REST to match the library's SDK-first design (the docker
> backend uses the moby SDK, not shell-out) — see §2.1 for why not
> `pkg/bindings`.

Podman's `system service` exposes **two APIs on one socket**:

- a **docker-compatible API** (the moby REST surface), and
- the **libpod API** (Podman's native extensions, including
  checkpoint/restore — which are *not* in the docker-compatible API).

The library's `runtime/docker` backend already talks to the moby REST API
via `moby/moby/client` and already supports a `DOCKER_HOST` override
(`runtime/docker/client.go`). So:

- **Point the existing docker backend at Podman's socket** for the bulk
  of the `Runtime` surface (Run/Start/Stop/Remove, Exec, Inspect, Logs,
  FindByLabel, Pull, networks/volumes). These are docker-compatible and
  should work unchanged.
- **Add checkpoint/restore against the libpod API** (a thin addition),
  since those endpoints don't exist in the docker-compat surface.

This avoids writing a second full backend. The new code is small: a
constructor that wires the docker runtime to the podman socket, the
`CheckpointRuntime` methods, a `Capabilities` override, and (pending §4)
a build-path override.

### 2.1 Why not B (full native backend) or C (CLI)

- **B — full `runtime/podman` via libpod REST / `pkg/bindings`:** the most
  idiomatic, but it re-implements the entire `Runtime` surface we already
  have working over the moby client, and `pkg/bindings` drags in the very
  large `containers/podman` module. **Spiked 2026-06-21:** importing
  `pkg/bindings` takes the dependency tree from **76 → 384 modules** and
  fails to build CGO-free (`proglottis/gpgme` needs cgo + `pkg-config`),
  requiring the `containers_image_openpgp` build tag threaded through every
  downstream consumer. Rejected — too heavy and build-hostile for a
  library. We hit the libpod endpoints with a **thin stdlib HTTP client**
  instead (zero new deps, cross-compiles cleanly).
- **C — full backend via `podman` CLI:** streaming exec/build events over
  the CLI are brittle — exactly the brittleness the docker backend avoids
  by using the SDK. Reserve CLI shell-out for the few calls where it's
  simplest (possibly the C/R calls themselves; §5.2).

The cost of Option A is a hard dependency on Podman's docker-compat
fidelity. The one place that's known to leak is **build** — see §4.

## 3. Packaging: a thin `runtime/podman` that composes `runtime/docker`

```go
// runtime/podman/podman.go (Linux build tag; Podman is Linux-only)
package podman

// Runtime is the Podman backend. It embeds a docker.Runtime wired to
// Podman's docker-compatible socket for the standard Runtime surface,
// and adds the libpod-only checkpoint/restore on top.
type Runtime struct {
    *docker.Runtime          // docker-compat API at the podman socket
    libpod  *libpodClient    // thin client for libpod-only endpoints (C/R)
}

func New(ctx context.Context, opts Options) (*Runtime, error) {
    // opts.Socket defaults to the podman service socket
    // (e.g. unix:///run/podman/podman.sock). Construct the docker.Runtime
    // against it, construct the libpod client against the same socket,
    // probe capabilities (libpod reachable + optional deployer-supplied
    // CRIU probe — see §5.3 for why CRIU can't be checked over REST).
}

// Checkpoint / Restore implement runtime.CheckpointRuntime via libpod.
// Capabilities overrides docker.Runtime's to set Checkpoint=true.
// BuildImage may be overridden depending on §4.
```

Embedding `*docker.Runtime` means the Podman backend satisfies
`runtime.Runtime` for free and we override only what differs
(`Capabilities`, `Checkpoint`/`Restore`, possibly `BuildImage`). The
docker backend is untouched.

## 4. The make-or-break risk: the build path

The library's `runtime/docker/build.go` is **BuildKit-only** (requires
Docker 23+ BuildKit). **Podman has no BuildKit** — it builds with buildah.
Podman's docker-compat `/build` endpoint exists but does not provide
BuildKit semantics, so the library's build path will not work as-is
against Podman.

> **IMPLEMENTED — build-path spike + backend, 2026-06-19 (bench).** Native
> `podman build` (buildah) of a representative devcontainer Dockerfile
> succeeds, so the buildah path is viable. **`runtime/podman.BuildImage`
> now overrides the embedded `docker.Runtime`'s BuildKit build** and shells
> out to `podman build` (`runtime/podman/build.go`): maps `BuildSpec` →
> `podman build` flags, reads the image ID from `--iidfile`, and streams
> the build log as `BuildEventLog` events. Validated by the gated
> integration test `TestIntegration_BuildImage` (PASS on live podman:
> built `sha256:9cecd9d…`, tag applied, 6 log events). Pre-baked/pulled
> images remain the fast path; this covers the in-container build case.

This is the deciding factor for Option A and was settled in a Phase-0
spike (above). Candidate resolutions, in order of preference:

1. **Pre-built / pulled images (no in-container build).** If the consumer's
   devcontainer images are pre-baked and pulled (the common case — see the
   pre-baked-image fast path in `design/features.md`), the build path is
   rarely exercised at workspace start. `BuildImage` could return
   `ErrNotImplemented` on the Podman backend for v1, and the feature
   pipeline relies on pulled images.
2. **Route build through Podman/buildah** — override `BuildImage` to use
   the libpod build endpoint or shell out to `podman build`. More work;
   loses BuildKit features (cache mounts, etc.) the current path uses.
3. **Build elsewhere, load into Podman** — keep a real BuildKit builder in
   CI / a sidecar, push to a registry, `podman pull`. Shifts build out of
   the workspace runtime entirely.

**Decision:** Phase-0 build-path spike decides between (1) and (2). Lean
(1) for v1 if the consumer's images are pre-baked; otherwise (2).

## 5. Checkpoint / restore implementation

The primitive (`CheckpointRuntime`, `CheckpointSpec`, `RestoreSpec`,
`CheckpointRef`) is defined in `design/checkpoint-restore.md` §3. The
Podman backend implements it.

### 5.1 Mapping (verified on the bench, 2026-06-19)

> **IMPLEMENTED via libpod REST, not the CLI (2026-06-21).** §2 settled the
> transport as the libpod REST API on the Podman socket (no `podman` CLI
> shell-out). The CLI flags below are kept only as the human-readable
> equivalent of the endpoints the backend actually calls:
> `POST …/libpod/containers/{id}/checkpoint?export=true&tcpestablished=&leaverunning=`
> (response body = the archive) and
> `POST …/libpod/containers/import/restore?import=true&tcpestablished=&name=`
> (archive in the request body → `{"Id":…}`). See `runtime/podman/checkpoint.go`.

```text
Checkpoint ≈ podman container checkpoint --export <ArchivePath> \
                 [--tcp-established] [--leave-running=!StopAfter] <id>
Restore    ≈ podman container restore --import <ArchivePath> \
                 [--tcp-established] [--name <Name>]
```

`restore --import` rebuilds the container (rootfs + mounts + network) from
the self-contained archive into a *new* container — no `RunContainer`
pre-step needed.

### 5.2 libpod REST vs CLI for these two calls

- **libpod REST:** `POST /libpod/containers/{name}/checkpoint?export=…` and
  `POST /libpod/containers/{name}/restore?import=…`. No CLI dependency,
  programmatic errors. Preferred if the thin libpod client is small.
- **CLI shell-out:** simplest; mirrors the `docker compose` shell-out the
  library already does. Acceptable fallback.

Either way, keep it behind the backend so the engine only sees
`CheckpointRuntime`.

> **Transport: RESOLVED (integration test, 2026-06-19).** The earlier
> "wedge" concern (podman service + CLI contending on the store) was
> **disproven for normal use**: the live integration test ran the full
> Option A path — moby client over the podman service for
> pull/run/start/exec/remove, *plus* podman-CLI `checkpoint`/`restore`
> against the same store — and passed cleanly in 25.6s (memory resumed
> 7→10). The original wedge symptom traced to (a) a forcibly-killed
> `podman system service` leaving a stale lock and (b) a test-harness bug
> (`pkill -f "system service"` matching the runner's own shell). So
> **CLI for C/R is viable alongside the moby-over-service surface.**
>
> **Superseded (2026-06-21):** although CLI C/R was proven viable, the
> final implementation uses **libpod REST**, not the CLI — to keep the
> backend SDK-first and shell-out-free like the docker backend (§2/§2.1).
> The transport-wedge finding above still stands and is why REST over the
> *same* socket is safe.

### 5.3 Capability probe

> **IMPLEMENTED with a caveat (2026-06-21).** `Capabilities().Checkpoint`
> is gated at construction on the **libpod API being reachable**
> (`GET /libpod/_ping` → 2xx, which also confirms it is genuinely Podman:
> a docker socket 404s the `/libpod/` path), cached in `checkpointOK`.
>
> The original plan was to *also* gate on `criu check` passing. That is
> **not achievable over the REST transport**: libpod has no `criu check`
> endpoint, `/info` does not report CRIU, and the backend is deliberately
> CLI-free (§2.1) so it won't shell out to `criu check` either. So the
> backend cannot verify CRIU itself. Instead, `podman.Options.CheckpointProbe
> func(context.Context) bool` lets the **deployer** — who runs
> `podman system service` and therefore knows the host — fold in that
> assertion (exec `criu check`, read a provisioning marker, etc.); it runs
> once at `New` alongside the ping. Nil means "don't probe": the bit then
> reflects libpod reachability only.
>
> When CRIU is absent and no probe caught it, `Checkpoint` fails at call
> time with a `*runtime.CheckpointFailedError` (carrying libpod's stderr),
> which is distinct from `ErrCheckpointUnsupported` and lets the platform
> fall back to a cold `Up`. `Engine.Checkpoint/Restore` still return
> `ErrCheckpointUnsupported` up front when the bit is false.

## 6. Phased plan

### Phase 0 — de-risk spikes (no library code; on `ckpt-bench`)

Gates the whole investment. Each is a go/no-go:

- **Build-path spike (§4):** does the consumer's devcontainer come up under
  Podman with pre-built/pulled images (option 4.1), or do we need a buildah build
  path (4.2)? *Decides the BuildImage strategy.*
- **Multi-service compose:** ✅ DONE (2026-06-19). Two networked services
  (`app`→`db`), checkpointed + restored *per container*; both resumed and
  the inter-container link (service-name DNS + TCP) re-established. No
  compose-level primitive needed — the engine checkpoints/restores each
  service and the shared network re-forms the links. Restore ordering
  forgiving for reconnecting services.
- **`--tcp-established` survival:** ✅ DONE (2026-06-19). Required for any
  service holding a live TCP connection — without it checkpoint fails
  intermittently (exit 125). The backend should pass it by default for
  C/R. Residual edge: a persistent connection across a peer-IP change
  still breaks; reconnecting clients recover.
- **Cross-node transfer:** ✅ DONE (2026-06-19). Checkpointed on pod A,
  copied the 43 KB archive to pod B (separate empty Podman store), restored
  on B — resumed + networking + image populated from the archive. Archive
  is fully self-contained; cross-node is just file transfer. (Same-node by
  scheduler chance; a forced cross-node run is the only remaining nicety.)

### Phase 1 — the contract (small, backend-agnostic library change)

- `runtime/runtime.go`: add `CheckpointRuntime` + `CheckpointSpec` /
  `RestoreSpec` / `CheckpointRef`.
- `runtime/compose_primitives.go`: add `Capabilities.Checkpoint`.
- `runtime/errors.go` (+ `devcontainer.Error`): `ErrCheckpointUnsupported`,
  `CheckpointFailedError`, `RestoreFailedError`.
- New `checkpoint.go` (repo root): `Engine.Checkpoint` / `Engine.Restore`
  wrappers that type-assert `CheckpointRuntime` (mirror compose-source
  handling).
- Unit tests against a fake runtime implementing the sub-interface.
- *No backend implements it yet — this just lands the API shape.*

### Phase 2 — the Podman backend

- `runtime/podman/` package (Linux build tag): `New` wiring `docker.Runtime`
  to the podman socket + libpod client; capability probe.
- `runtime/podman/checkpoint.go`: `Checkpoint`/`Restore` (§5).
- `BuildImage` per the Phase-0 build decision (§4).
- `Capabilities` override (`Checkpoint=true`).
- Integration tests behind a real-podman gate (mirror the existing
  real-docker compose integration-test gate).

### Phase 3 — consumer adoption (out of library scope; consumer repo)

- The consumer's workspace runtime image: Podman + criu (keep), drop the
  docker-in-docker dependency for the devcontainer runtime.
- The consumer runtime wires `runtime/podman` and orchestrates checkpoint-on-
  eviction → archive to PVC/registry → restore-on-new-pod.
- Separate design effort in the consumer's repo; this is the largest piece
  and the real prerequisite for production use.

## 7. File-level change list (Phases 1–2)

| File | Change |
| --- | --- |
| `runtime/runtime.go` | `CheckpointRuntime` interface + spec/ref types |
| `runtime/compose_primitives.go` | `Capabilities.Checkpoint` |
| `runtime/errors.go` | `ErrCheckpointUnsupported`, `CheckpointFailedError`, `RestoreFailedError` |
| `checkpoint.go` (new, root) | `Engine.Checkpoint` / `Engine.Restore` wrappers |
| `runtime/podman/podman.go` (new) | backend: embed `docker.Runtime` @ podman socket, probe |
| `runtime/podman/checkpoint.go` (new) | libpod/CLI `Checkpoint`/`Restore` |
| `runtime/podman/build.go` (new, maybe) | build-path override per §4 |
| `runtime/docker/client.go` | (verify) `DOCKER_HOST`/socket override is reusable as-is |
| `*_test.go` | unit (fake runtime) + integration (real-podman gate) |

## 8. Testing

- **Unit:** engine wrappers + capability gating against a fake runtime
  (no podman needed).
- **Integration (real podman gate):** bring a container up via the Podman
  backend, checkpoint→remove→restore, assert memory + networking resume —
  the bench test, codified. Gated like the existing real-docker compose
  tests (skipped without a reachable podman).

## 9. Open questions / decisions

- **Build path (§4)** — the gating decision; Phase-0 spike.
- **libpod REST vs CLI (§5.2)** for the two C/R calls.
- **Multi-container** — per-container primitive + engine sequencing, vs
  modelling Podman pods.
- **Socket provisioning** — who runs `podman system service` and where the
  socket lives (consumer-runtime concern; the library just takes the address).

## 10. Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Podman docker-compat build incompatible (BuildKit) | Backend can't build images | Phase-0 build spike; pre-built/pulled images for v1 (§4.1) or buildah path (§4.2) |
| Other docker-compat fidelity gaps (exec/inspect edge cases) | Subtle backend bugs | Integration tests against real podman; fall back to native libpod calls per-method if needed |
| Running devcontainers under Podman is a stack change | Consumer migration cost | Phase 3, costed separately; Podman runs OCI/Docker images |
| Multi-service compose unproven | Real devcontainer may not migrate cleanly | Phase-0 spike before committing |
