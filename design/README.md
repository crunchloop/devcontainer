# Design records

This directory holds the architectural design notes for the
`devcontainer` library — the choices made before each major piece of
code landed, why they were made, and the trade-offs we accepted.
They are written for contributors and embedders who want to
understand *why* the code looks the way it does, not as user-facing
documentation. For usage, see the [top-level README](../README.md).

The docs reflect the state of the world at the time they were
written. They are not kept perfectly in sync with the code; when a
record disagrees with `main`, the code is authoritative. Sections
that explicitly call out alternatives, probe results, or "future
work" are kept because the reasoning is still useful even after the
work shipped.

## Documents

| File | Topic |
| --- | --- |
| [`resolved-config.md`](resolved-config.md) | The `ResolvedConfig` type — the central data structure that flows out of `devcontainer.Resolve` into every downstream package. Locks down the data shape, the parse → merge → substitute pipeline, and `Source` polymorphism. |
| [`runtime.md`](runtime.md) | The `runtime.Runtime` interface, the `Workspace` value object, the `Engine` layer, container-context substitution, and lifecycle idempotency. The boundary that makes a second backend possible. |
| [`runtime-applecontainer.md`](runtime-applecontainer.md) | The second `Runtime` backend: Apple's `container` stack on macOS via a cgo + Swift bridge. Daemon model, bridge ABI, version pinning, build constraints. |
| [`compose-native.md`](compose-native.md) | The runtime-agnostic compose orchestrator that drives any backend through `runtime.Runtime` primitives. Replaces (when opted in) the `docker compose` shell-out, and is what enables compose source on apple-container. |
| [`features.md`](features.md) | The Dev Container Features pipeline: OCI / HTTPS / local resolution, DAG ordering, dockerfile generation, the pre-baked-image fast path, and the content-addressed cache. |
| [`structured-errors.md`](structured-errors.md) | The `*devcontainer.Error` surface returned from every public failure path. Code catalog, `Cause` chain conventions, and the `StderrCarrier` interface for subprocess-output access. |
| [`checkpoint-restore.md`](checkpoint-restore.md) | The optional `CheckpointRuntime` sub-interface: CRIU-backed checkpoint/restore for migrating spot-evicted devcontainers. Pass-2 records the empirical finding that docker's restore is broken upstream and only **Podman** (`checkpoint --export`/`restore --import`) works end-to-end — so the primitive lands in a new `runtime/podman` backend. Includes the runtime matrix, capability gating, integration options, and what's proven vs open. |
| [`podman-backend.md`](podman-backend.md) | The implementation plan for the Podman backend that makes checkpoint/restore real: reuse the docker/moby backend against Podman's docker-compatible socket and add C/R via libpod, the build-path risk (BuildKit vs buildah) that gates it, the phased plan (de-risk spikes → contract → backend → consumer adoption), and the file-level change list. |

## What's *not* here

- Code-level API documentation lives next to the code as Go doc
  comments — `pkg.go.dev/github.com/crunchloop/devcontainer` renders
  it.
- The Dev Containers spec itself lives at
  [containers.dev](https://containers.dev/implementors/json_reference/).
  These records describe *our implementation choices* against that
  spec, not the spec itself.
- In-flight planning, milestone trackers, draft proposals, and
  scratch notes are kept under `design/private/` (gitignored).

## Status legend

Each document opens with a `**Status:**` line. The values you'll see:

- **Draft for review** — written before the work landed; reflects
  the design as proposed.
- **Accepted** — decided, code matches.
- **Implemented** — written before the work landed; code now
  matches and the document is the architectural record.

When a section in a document is superseded by a later decision, the
later document is linked from an "Appendix" at the bottom of the
older one.
