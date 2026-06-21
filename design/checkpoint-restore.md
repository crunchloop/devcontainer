# Design — Checkpoint / Restore

**Status:** Draft for review — Pass 2 (empirically revised 2026-06-19)
**Date:** 2026-06-19
**Scope:** an optional `Runtime` sub-interface, `CheckpointRuntime`, that
checkpoints a running container's process + memory state (and writable
rootfs) to a portable archive and restores it later, possibly on another
host — to migrate spot-evicted devcontainers without losing in-memory
work. Defines the primitive, the capability gate, the engine wrappers,
and the backend that actually implements it. Backends that can't do it
return `ErrNotImplemented` and advertise `Capabilities().Checkpoint == false`.

> **Pass-2 headline:** a day of empirical testing on a real consumer
> workspace pod (on a managed Kubernetes cluster) **disproved the original
> premise that the docker backend would implement this.** Docker's
> checkpoint/restore is broken on current versions (a known, open upstream
> bug). The mechanism works end-to-end only on **Podman**
> (`checkpoint --export` / `restore --import`). The primitive below
> survives; the backend that implements it changes from `runtime/docker`
> to a new `runtime/podman`. §2 records what we tested and why. The
> original docker-centric design is preserved in Appendix A as the
> reasoning trail.

Companion to `design/runtime.md` (the `Runtime` interface this extends)
and `design/structured-errors.md` (the error surface). Follows the
optional sub-interface pattern established by `ComposeRuntime`
(`design/compose-native.md` §3).

---

## 1. Motivation

The primary consumer runs coding agents inside devcontainers on
**spot instances**. A spot node can be reclaimed at any time. Today a
reclaim kills the workspace: the agent process dies and any in-memory
work (an in-progress build, a half-written file held open, a running
test, the agent's own working state) is lost.

The platform gets advance warning of a reclaim (cloud providers deliver a
termination notice ~30s–2min ahead). The target flow:

```text
1. Platform detects the node-reclaim notice.
2. Platform tells the runtime to CHECKPOINT the devcontainer
   (process + memory + rootfs → a portable archive on the workspace volume).
3. A new pod starts on a healthy node, RESTORES the archive, and the
   agent resumes mid-task instead of cold-booting.
```

This is textbook CRIU live-migration; the contribution is exposing it as
a clean library primitive with honest capability gating.

## 2. What we tested (2026-06-19) and what it proved

We ran the whole stack against a live consumer workspace pod and a dedicated
bench pod (same workspace runtime image, custom entrypoint that starts
dockerd/containerd and idles, so we could drive checkpoint/restore by
hand without the workspace supervisor tearing things down).

### 2.1 The environment is capable

On the real workspace pod's inner daemon (the devcontainers run in a
docker-in-docker inside the `runtime` container):

- `docker 29.2.1`, `containerd v2.2.5`, `runc 1.3.6`, `criu 4.1.1`.
- Storage: `overlayfs` (containerd snapshotter), **data-root on the
  workspace PVC** (`/workspace/docker`). This matters: the inner docker's
  entire graph — every container's writable layer and named volumes —
  already lives on the PVC and survives a pod move.
- The `runtime` container is **privileged**; dockerd runs as root with
  `cap_sys_admin` + `cap_checkpoint_restore`.
- `criu check` (as root) → **"Looks good."** The node kernel fully
  supports CRIU. (An earlier failure was only because criu was invoked as
  an unprivileged user.)

So kernel + CRIU + runc + containerd are all capable. **The blocker is
purely the container-manager layer**, not the substrate.

### 2.2 Runtime matrix — what works

| Layer | Checkpoint | Restore | Verdict |
| --- | --- | --- | --- |
| **`docker checkpoint` CLI** | ✅ (~0.5s, idle) | ❌ netns bind-mount `/proc/0/ns/net` fails; custom `--checkpoint-dir` unsupported on restore | **Dead end** (open upstream bug) |
| **raw `runc` (plain OCI bundle)** | ✅ | ✅ with `--empty-ns network` — counter resumed 10→14 | Works, but bypasses any manager |
| **raw `runc` on a docker-managed container** | ✅ | ❌ docker unmounts rootfs + removes the bundle on task exit | No home to restore into |
| **containerd `ctr containers …` (plain `ctr run`)** | ✅ `--rw --task` | ✅ `--rw --live` — counter resumed 9→13, into a *new* container | Works for manager-free containers |
| **containerd `ctr` on a docker-created container** | ❌ `snapshot does not exist` | — | Docker leaves the containerd container's `SnapshotKey` **empty** |
| **nerdctl (CNI bridge) via `ctr`** | ✅ `--rw` (proper `SnapshotKey`) | ❌ nerdctl's OCI hooks (CNI + `/etc/hosts`) fail outside nerdctl's control | Manager hooks break generic restore |
| **Podman `checkpoint --export` / `restore --import`** | ✅ | ✅ **full e2e** — memory resumed 7→10, **bridge networking re-attached & egress working**, into a fresh container | **This is the path.** |

### 2.3 The two recurring failure modes

1. **Docker's restore is broken.** `docker start --checkpoint` fails on
   the network-namespace bind-mount (`/proc/0/ns/net`, pid 0) regardless
   of network mode (bridge *and* none). This is a **known, open upstream
   bug** on the current containerd-integrated engine
   ([containerd#12141](https://github.com/containerd/containerd/issues/12141),
   our exact stack), and docker's custom-checkpoint-dir support has been
   broken since the containerd 1.0 integration
   ([moby#37344](https://github.com/moby/moby/issues/37344)). CRIU's own
   project [recommends Podman over Docker](https://criu.org/Docker).
   Docker's CLI also can't pass the CRIU options that would help
   (`--empty-ns`, `--tcp-established`) — those live behind
   `/etc/criu/runc.conf` at the runc layer, which fixes TCP handling but
   *not* the daemon-level netns bug.

2. **Container managers inject state/hooks that generic restore can't
   reconstruct.** Docker leaves the containerd container's `SnapshotKey`
   empty (so `ctr` can't bundle the rootfs). nerdctl bakes CNI + hosts
   OCI hooks that fail when restored outside nerdctl. In both cases the
   *checkpoint* succeeds but a *manager-agnostic restore* cannot rebuild
   the container's environment (mounts, `/etc/hosts`, network).

**Podman is the one tool that owns the full lifecycle on both ends:**
`restore --import` reconstructs the rootfs, the bind-mount sources, and
re-attaches the network itself. That is exactly why it works end-to-end
where everything else stalls — and why CRIU recommends it.

### 2.4 Filesystem note (supersedes the old §2)

The original draft warned that "a checkpoint is process state, not the
filesystem," making cross-node restore fragile. Two findings soften this:

- Podman's `--export` archive **bundles the writable rootfs layer**
  alongside the CRIU images, so the artifact is self-contained and
  portable by construction.
- The consumer's inner docker keeps **data-root on the PVC**, so writable
  layers persist across pods anyway.

The constraint is no longer "everything mutable must be on a volume" — the
export artifact carries the rootfs. The constraint that remains is that
the **destination must be able to reconstruct the container's external
mounts** (the workspace bind, secrets, and any other injected mounts) —
which the orchestrator can, because it created the devcontainer and knows
its mount set.

## 3. The primitive

Mirror `ComposeRuntime`: an optional sub-interface the engine
type-asserts, gated by `Capabilities()`. Backends that don't implement it
are invisible to the rest of the library. The shape now models Podman's
**export/import** (a portable archive), not docker's checkpoint-dir.

```go
package runtime

// CheckpointRuntime is the optional sub-interface a Runtime implements
// when it can checkpoint a running container to a portable archive
// (process + memory via CRIU, plus the writable rootfs layer) and later
// restore it — possibly on another host — into a fresh container.
//
// Implemented by runtime/podman (podman container checkpoint --export /
// restore --import). NOT implemented by runtime/docker: docker's restore
// is broken on current engines (see design/checkpoint-restore.md §2 and
// Appendix A). Backends without it cause Engine.Checkpoint/Restore to
// return ErrCheckpointUnsupported.
type CheckpointRuntime interface {
    // Checkpoint writes a self-contained checkpoint archive for a running
    // container to spec.ArchivePath. The archive carries the CRIU image,
    // the writable rootfs diff, and the config needed to restore. With
    // spec.StopAfter the container is stopped/removed after the archive
    // is written (the eviction path); otherwise it keeps running.
    Checkpoint(ctx context.Context, id string, spec CheckpointSpec) (CheckpointRef, error)

    // Restore re-creates and resumes a container from a checkpoint
    // archive, reconstructing its mounts and re-attaching networking.
    // Restores into a NEW container (migration), so the source may be
    // gone. Returns the new Container handle.
    Restore(ctx context.Context, spec RestoreSpec) (*Container, error)
}

// CheckpointSpec configures Checkpoint → `podman container checkpoint`.
type CheckpointSpec struct {
    // ArchivePath is where the export archive is written. Point it at the
    // workspace PVC (or anywhere that travels to the destination — a
    // registry blob, object storage). Maps to `--export`.
    ArchivePath string

    // StopAfter leaves the container stopped after export (eviction
    // path). False keeps it running ("backup" checkpoint).
    StopAfter bool

    // TCPEstablished requests checkpoint of established TCP connections
    // (`--tcp-established`). Needed if the agent holds connections we
    // want to survive; otherwise they reset and the agent reconnects.
    TCPEstablished bool
}

// RestoreSpec configures Restore → `podman container restore --import`.
// Note: unlike the old docker model, no RunSpec is needed — the archive
// is self-describing (image, config, mounts, rootfs).
type RestoreSpec struct {
    ArchivePath    string // the archive Checkpoint wrote (--import)
    Name           string // optional new container name
    TCPEstablished bool   // must match the checkpoint if it had connections
}

// CheckpointRef describes a written checkpoint archive.
type CheckpointRef struct {
    ArchivePath string
    // Size is the archive size in bytes — feeds the platform's
    // eviction-window / transfer budgeting.
    Size int64
}
```

### 3.1 Restore is into a new container, by design

Podman `restore --import` creates a fresh container from the archive — it
does not need the original to exist. This is the migration shape exactly:
checkpoint on the dying pod, ship the archive, import on a new pod. We
verified this end-to-end (removed the original entirely, imported into a
new container, memory + networking intact). No `RunContainer`-then-start
dance is needed (that was a docker-model artifact; see Appendix A §3.1).

### 3.2 No `ListCheckpoints` / `RemoveCheckpoint` in v1

The archive is a plain file the platform owns and reclaims (PVC/registry
lifecycle). No server-side enumeration needed; defer it.

### 3.3 Project orchestration above the primitive (decision 2026-06-21)

The primitive stays strictly per-container (§3). §9 originally pushed
*all* multi-container sequencing onto the platform. **Revised:** the
engine also ships a thin **project orchestrator** —
`Engine.CheckpointProject` / `RestoreProject` (root `checkpoint_project.go`)
— layered above the per-container primitive, so a caller can checkpoint and
restore a whole compose project in one call rather than re-implementing the
loop. It is intentionally decoupled from the compose-go / `docker compose`
machinery: it identifies the project's containers purely by their
`com.docker.compose.project` label (via `runtime.ListContainers`),
checkpoints each through the same `CheckpointRuntime` the single-container
path uses, and writes one archive per service plus a `project.json`
manifest. Restore reads the manifest, restores each archive, and reattaches
the devcontainer service (the one carrying `dev.containers.id`) as the
`Primary *Workspace` — the rest are returned as restored containers.

Model notes (validated by the Phase-0 spike, §7):

- **Order:** checkpoint/restore in deterministic service-name order; restore
  order is forgiving because reconnecting services self-heal.
- **Network:** the shared network re-forms as containers come back
  (`restore --import` re-attaches networking, and Podman restores the
  original container name, so service-name DNS resolves again). The network
  must still exist on the target; recreating it cross-node is the
  orchestrator's caller's job (or a follow-up).
- **Completeness:** the manifest is written last, so its presence implies a
  complete set; a partial checkpoint leaves no manifest and `RestoreProject`
  fails cleanly.
- **Scale:** one container per service in v1 (no compose `scale`).

## 4. Capability gate

Add one field to the `Capabilities` struct
(`runtime/compose_primitives.go`):

```go
type Capabilities struct {
    // ... existing fields ...

    // Checkpoint reports whether this backend can checkpoint/restore a
    // running container (CheckpointRuntime). True on runtime/podman when
    // the libpod API is reachable (and a deployer-supplied CRIU probe, if
    // any, passes); false on runtime/docker (restore is broken upstream)
    // and runtime/applecontainer.
    Checkpoint bool
}
```

The podman backend probes at construction (libpod reachable, plus an
optional `Options.CheckpointProbe` the deployer supplies to assert CRIU —
the REST transport can't run `criu check` itself; see
`design/podman-backend.md` §5.3) and sets the bit. The engine checks it
before attempting an operation and returns a typed error (§6) rather than
surfacing an opaque failure.

## 5. Implementation: `runtime/podman`

The mechanism lives in a **new backend**, `runtime/podman`, implementing
the full `runtime.Runtime` interface plus `CheckpointRuntime`. The
checkpoint/restore methods map directly to verified commands:

```text
Checkpoint → podman container checkpoint --export <ArchivePath> \
                 [--tcp-established] [--leave-running=!StopAfter] <id>
Restore    → podman container restore --import <ArchivePath> \
                 [--tcp-established] [--name <Name>]
```

Three integration options for talking to podman, lightest first:

1. **Shell out to the `podman` CLI.** Simplest, lowest coupling — and the
   library already shells out for `docker compose`. Recommended for v1.
2. **Thin HTTP client against the libpod REST API**
   (`POST /libpod/containers/{name}/checkpoint` + `/restore`), served by
   `podman system service`. No heavy deps. Checkpoint/restore is a
   *libpod* extension, not in podman's docker-compatible API, so target
   the libpod endpoints.
3. **`github.com/containers/podman/v5/pkg/bindings/containers`**
   (`Checkpoint` / `Restore`). Official and typed, but pulls in the very
   large `containers/podman` module (cgo, storage build tags) — a heavy
   dependency for a Go library. Avoid unless we want the full surface.

### 5.1 The bigger architectural implication

This backend means **running devcontainers under Podman instead of
Docker**. That is a consumer-runtime decision beyond this library, but it
is the price of working checkpoint/restore: docker cannot do it on current
versions. Podman runs standard OCI/Docker images and offers a
docker-compatible API + `podman-compose`, so it is a plausible swap, but
it is real migration work and should be costed separately.

## 6. Error surface

Per `design/structured-errors.md`:

- `ErrCheckpointUnsupported` — active runtime doesn't implement
  `CheckpointRuntime` or `Capabilities().Checkpoint == false`. Returned by
  the engine wrappers before any work.
- `CheckpointFailedError` — checkpoint/export failed; carries the
  container id and podman's stderr via the `StderrCarrier` convention.
- `RestoreFailedError` — import/restore failed; carries the archive path
  and podman's message. Distinct from a cold-start failure so the
  platform can deterministically **fall back to a cold `Up`** (workspace
  data on the PVC is intact; only in-memory state is lost).

## 7. Validation status

What's **proven** (2026-06-19, bench pod, real image):

- ✅ Kernel/CRIU/runc/containerd all capable (`criu check` good).
- ✅ Podman `checkpoint --export` → remove original → `restore --import`
  round trip: memory resumed (counter 7→10), **bridge networking
  re-attached and egress working**, single 43 KB portable archive,
  ~1.8s checkpoint / ~0.5s restore on an idle container.
- ✅ Docker is a dead end (netns restore bug, reproduced; corroborated by
  open upstream issues).
- ✅ **Cross-pod transfer / self-contained archive.** Checkpointed on
  pod A, copied the archive to pod B (separate, *empty* Podman store that
  had never pulled the image), `restore --import` on B: container resumed
  (counter 9→12), networking functional (egress ok), and the image was
  populated *from the archive*. The archive needs nothing node-local —
  cross-node is just "copy the file." (Pods were same-node by scheduler
  chance; stores were fully isolated, so the node boundary isn't
  load-bearing. A forced cross-node run with anti-affinity is the only
  belt-and-suspenders gap.) Build-path spike also done — see
  `design/podman-backend.md` §4 (buildah build works).
- ✅ **Multi-service project + inter-container networking.** Two services
  on a user-defined podman network (`app` → `db:9000` every second,
  service-name DNS). Checkpointed *both* (`--export`), removed both,
  restored *both* (`--import`): both resumed (app counter 8→18, db
  tracking), and the **inter-container link re-established** (app resolves
  `db` and reconnects). Per-container checkpoint/restore + the shared
  network is sufficient for loosely-coupled (reconnecting) services; no
  compose-level C/R primitive needed. Restore ordering is forgiving
  (db-then-app; app self-heals each tick).
- ✅ **`--tcp-established` is required for connection-holding services.**
  Without it, checkpoint of a service with a live TCP connection fails
  intermittently (timing-dependent, exit 125). With it, checkpoint
  succeeds. Caveat: it lets checkpoint *succeed* and reconnecting clients
  recover; a service relying on a *persistent* connection surviving a
  peer-IP change on restore is the residual edge (matches the
  "agents reconnect" assumption).

What's **still open** (minor):

- **Working-set timing.** Idle container was fast (~0.5s same-pod, ~3.5s
  cross-pod incl. rootfs unpack); a busy agent's memory footprint sets the
  real checkpoint time vs the eviction window. Measure on a real workload.
- **Forced cross-node** placement (anti-affinity) — belt-and-suspenders;
  the archive is already proven node-independent.

## 8. Constraints, risks & mitigations

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Docker can't do restore | Original premise invalid | Pivot to `runtime/podman`; docker backend reports `Checkpoint=false`. Tracked: [containerd#12141](https://github.com/containerd/containerd/issues/12141). |
| Running devcontainers under Podman is a stack change | Consumer migration cost | Costed separately; Podman runs OCI/Docker images + has a docker-compat API. |
| Multi-container compose project | Per-container checkpoint, ordering | Open spike; consider podman pods / staged restore. |
| In-flight TCP breaks on new IP | Agent resumes into dead sockets | `--tcp-established` available; otherwise agents reconnect (transient-blip semantics). |
| Kernel / CRIU parity between nodes | Restore fails across mismatched nodes | Homogeneous spot fleet + node-image requirement; `RestoreFailedError` → cold-boot fallback. |
| Eviction window too short | Checkpoint doesn't finish | `CheckpointRef.Size` + working-set timing feed a go/no-go; degrade to cold boot for large footprints. |
| Restore failure | Lost in-memory state | `RestoreFailedError` distinct from cold-start; fall back to cold `Up` on the PVC — data intact. |

## 9. What this does not do (v1)

- **No live migration without an eviction notice.** Checkpoint-then-
  restart, predicated on advance warning. Not transparent fault tolerance
  for instant node death.
- **No docker / applecontainer support.** `Capabilities().Checkpoint == false`;
  `ErrCheckpointUnsupported`. (Docker: broken upstream. Apple: no CRIU.)
- **No multi-container orchestration in the primitive.** The primitive is
  per-container. Sequencing a multi-service project is done one level up by
  the engine orchestrator (`Engine.CheckpointProject` / `RestoreProject`,
  §3.3) — not by the primitive, and not (for the simple loosely-coupled
  case) by the platform. Heavier sequencing (dependency-ordered restore,
  cross-node network recreation) remains the platform's job.
- **No engine-driven scheduling.** *When* to checkpoint and *where* to
  restore are the platform's job.

---

## Appendix A — The docker-centric design (superseded 2026-06-19)

The original Pass-1 draft assumed `runtime/docker` would implement
`CheckpointRuntime` via the moby client's `CheckpointCreate` +
checkpoint-aware `ContainerStart`, with a `--checkpoint-dir` redirected
onto the PVC and a daemon-experimental capability probe. The API surface
(`CheckpointCreate{CheckpointID, CheckpointDir, Exit}`,
`ContainerStart{CheckpointID, CheckpointDir}`) is real and present in
`moby/moby/client v0.4.1`, and **checkpoint (dump) does work**.

It was abandoned for restore, not checkpoint. Empirically (§2):

- `docker start --checkpoint` fails on the network-namespace bind-mount
  on the current containerd-integrated engine — an open upstream bug, not
  a config error.
- Custom `--checkpoint-dir` is unsupported on restore (broken since
  containerd 1.0); the default-dir workaround doesn't help because the
  netns failure is downstream of it.
- Docker leaves the containerd container's `SnapshotKey` empty, so the
  containerd-level checkpoint path can't bundle the rootfs either.

The reasoning is kept because "did anyone try docker?" is the first
question any reviewer will ask. Yes — in depth, on the real image — and
it does not work. See §2.3 and the linked issues.
