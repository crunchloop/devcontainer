# Design — Runtime-agnostic Compose Orchestrator

**Status:** Draft for review
**Date:** 2026-05-14
**Scope:** replace the `docker compose` CLI shell-out (`runtime/docker/compose.go`,
`runtime.ComposeRuntime` interface) with a Go-native, runtime-agnostic
orchestrator under `compose/` so the compose source path works against any
`runtime.Runtime` backend — including `runtime/applecontainer`, which has no
compose plugin and no Docker-API socket.

Companion to `design/compose.md` (the existing shell-out path, kept as the
historical record and the §13 "future Go-native" sketch that this design
supersedes) and `design/runtime-applecontainer.md` (the second backend whose
introduction makes this work load-bearing). The compose v2 spec and the
project-name convention (`dc-<devcontainerId>`) are inherited from prior
design without change.

This design triggers `compose.md` §13.6 criterion #4 — "drop the `docker compose`
dependency for a packaging reason" — concretely, the Apple container backend
ships in M6 and cannot satisfy `ComposeRuntime` through any shell-out path
because Apple's stack has no compose concept and no Docker-API-compatible
socket. Without this work, compose-source devcontainers are Docker-only forever.

---

## 1. Layering

```text
┌──────────────────────────────────────────────────────────────────┐
│ Engine (devcontainer pkg)                                        │
│   compose source path → compose.Orchestrator (not the runtime)   │
└─────────────────────────────┬────────────────────────────────────┘
                              │
┌─────────────────────────────▼────────────────────────────────────┐
│ compose pkg (this design — runtime-agnostic)                     │
│   - Load: compose-go parse + override merge (unchanged)          │
│   - Plan: services → topological order, networks, volumes        │
│   - Orchestrator.Up/Down: drives runtime.Runtime primitives      │
│   - Convergence: existing-container diff + recreate decision     │
│   - Health: depends_on condition waiting via InspectContainer    │
└─────────────────────────────┬────────────────────────────────────┘
                              │   runtime.Runtime interface
                              │   (no compose knowledge)
        ┌─────────────────────┼─────────────────────┐
        ▼                     ▼                     ▼
┌───────────────┐    ┌─────────────────┐    ┌────────────────────┐
│ runtime/docker│    │ runtime/        │    │ future: podman,    │
│ moby SDK      │    │ applecontainer  │    │ containerd, etc.   │
│               │    │ cgo bridge      │    │                    │
└───────────────┘    └─────────────────┘    └────────────────────┘
```

**Strict separation, mirroring `design/runtime.md` §1 and
`design/runtime-applecontainer.md` §1:** the `compose` package knows nothing
about Docker, Apple containers, or which backend is in use. It speaks the
`runtime.Runtime` interface and a small set of new primitives (§4).
Backends know nothing about compose semantics, project naming, dependency
ordering, or override files. The orchestrator is the only thing that
implements compose.

Concretely, this deletes the `runtime.ComposeRuntime` sub-interface
(runtime/runtime.go:19-35), removes `runtime/docker/compose.go` and its
`docker compose` shell-outs, and moves all compose logic into the new
`compose/` package alongside the existing `compose/load.go` and
`compose/override.go`.

## 2. Scope: the devcontainer subset of compose

Compose is a 100+ page spec. We do not reimplement it. We reimplement the
**subset that real devcontainers in the wild use**, defined empirically from:

- The compose features `compose-go/v2` already parses for us (zero new code).
- The features the existing `runtime/docker/compose.go` actually exercises
  (`up -d`, `down [--rmi local --volumes]`, `ps -q`).
- The features the M4 integration suite covers (PR12 fixture: 2-service
  compose, primary with feature, sidecar database).

### 2.1 In scope (the orchestrator implements)

| Compose feature | Why |
| --- | --- |
| `services.<name>.image` | Pull / use existing image. |
| `services.<name>.build` | Delegated to `runtime.BuildImage` (already runtime-agnostic). |
| `services.<name>.command` / `entrypoint` | Mechanical passthrough. |
| `services.<name>.environment` / `env_file` | Resolved by compose-go; passed to `RunSpec.Env`. |
| `services.<name>.volumes` (bind + named) | Bind mounts go straight to `RunSpec.Mounts`; named volumes need creation (§4.2). |
| `services.<name>.ports` | Passed to `RunSpec.PortBindings`. Compose mode keeps using compose's port directive, not our `forwardPorts` (compose.md §12.3 decision). |
| `services.<name>.depends_on` (long + short form) | Topo-sort + health gating (§5). |
| `services.<name>.healthcheck` | Passed through to backend; orchestrator reads health state for `condition: service_healthy`. |
| `services.<name>.restart` | Passthrough to `RunSpec.RestartPolicy`. |
| `services.<name>.labels` | Merged with our project-scoping labels (§3.1). |
| `services.<name>.networks` (default network only) | Project gets one network (`<project>_default`); services join it. |
| `services.<name>.network_mode: service:<other>` | Single edge case; needed for sidecars sharing network namespace. **Docker backend only** — Apple's VM-per-container model cannot share namespaces (see §11.3 R4). |
| `volumes:` top-level | Named volume creation with project-scoped names. |
| Variable interpolation (`${VAR}`) | Handled by compose-go; we never touch the substituted output. |
| `extends:` (local files) | Handled by compose-go. |
| `profiles:` | Handled by compose-go via project load options. |

### 2.2 Out of scope — refuse with a typed error

Same list as `compose.md` §13.2, repeated here so a new contributor doesn't
have to cross-reference:

- `secrets:` and `configs:` (Swarm constructs)
- `develop: watch:` (file-sync — out of scope for our runtime)
- `deploy:` (Swarm orchestration — `replicas`, `update_config`,
  `rollback_config`)
- Custom network drivers / IPAM beyond the backend default
- Multiple named networks per project (we create exactly one,
  `<project>_default`)
- `extends:` referencing remote files
- `services.<name>.platforms` (multi-arch builds via compose)
- `links:` (legacy, replaced by network DNS in compose v2)
- `external: true` networks / volumes (user-managed shared resources)
- `services.<name>.scale` > 1 (multiple replicas of the same service)

Mechanism: `compose.Plan` walks the parsed `*types.Project`, returns a
typed `*compose.UnsupportedFieldError` listing exactly which fields on
which services tripped the refusal. Wired through to `EngineEvent`
warnings so the user gets a clear "your compose file uses X which our
engine does not implement" rather than a silent partial run.

### 2.3 Out of scope — quietly ignored (documented)

These are spec fields that compose-go parses but the orchestrator does not
act on. Documented in the package doc; not warned per-run (would be noise):

- `tmpfs:` shorthand (use `tmpfs` in `volumes:` instead)
- `cap_add` / `cap_drop` beyond what the backend's `RunSpec.CapAdd/Drop`
  surfaces. Compose's full list maps cleanly; this is a "verify field
  parity" task during PR13b.
- `sysctls:`, `ulimits:` — pass through `RunSpec.Sysctls` /
  `RunSpec.Ulimits` if present; documented as backend-dependent.

## 3. Project model — names, labels, identity

### 3.1 Labels: the convergence currency

Every container the orchestrator creates carries a fixed label set. These
are how `Down`, restart-on-Up, and `Engine.Attach` find our resources.

```text
com.docker.compose.project        = <projectName>     # interop label
com.docker.compose.service        = <serviceName>     # interop label
com.docker.compose.oneoff         = False             # interop label
com.docker.compose.config-hash    = <sha256>          # see §3.3
dev.containers.id                 = <devcontainerId>  # primary service only
dev.containers.engine             = devcontainer-go/<version>
```

**Why the `com.docker.compose.*` labels.** Compose's `docker compose ps`,
`docker compose logs`, and `docker compose down` commands locate containers
by these exact labels. By writing them, our orchestrator's containers are
visible to the user's `docker compose` CLI for read-only inspection — the
user can still `docker compose ps` in their project dir and see what we
ran. This is a stability concession (compose.md §13.5: "tracking ecosystem
labels"), but it's a one-time mechanical mapping; compose has not changed
these label names since v2.0 (2021). If they ever do, our `docker compose
ps`-compat surface degrades but our own operation does not.

Networks and volumes get a subset:

```text
com.docker.compose.project = <projectName>
com.docker.compose.network = default       # on networks
com.docker.compose.volume  = <volName>     # on volumes
```

### 3.2 Project name

`dc-<devcontainerId>` (the project-wide naming convention, unchanged from
the shell-out design). Resource naming follows
compose v2 conventions so the user's existing tooling keeps working:

- Container name: `<project>-<service>-<index>` (e.g. `dc-abc123-app-1`)
  — single replica means `<index>` is always `1`.
- Default network name: `<project>_default`.
- Named volume name: `<project>_<volume>` (top-level `volumes:` map key).
- Anonymous volumes: backend-assigned.

### 3.3 `config-hash` — recreation policy

Compose stamps each container with `com.docker.compose.config-hash`
(a SHA256 of the canonical service config + image digest) so the next
`up` can decide "do I need to recreate this service?" Replicating
compose's exact algorithm is brittle (compose.md §13.3 rates it Medium
stability). Two options:

1. **Compute the same hash compose does.** Track upstream changes.
2. **Use our own hash (`dev.containers.config-hash`), accept the
   asymmetry.** Compose CLI run from outside our library would always
   think our containers are stale and want to recreate them.

**Decision (§14.5):** option 2. Our containers are *ours*; the user
running `docker compose up` in our project's directory while our engine
is also running it is undefined behavior anyway. The interop labels
(§3.1) are enough for `ps` / `logs` visibility; recreation policy stays
internal.

The hash inputs are: image ID + canonical-JSON of service config
(env sorted, volumes sorted, ports sorted, command + entrypoint + labels
sans config-hash itself). Inputs are deterministic by construction;
this means a bit-for-bit identical compose project re-applied yields no
recreation.

## 4. New `runtime.Runtime` primitives

The orchestrator drives the runtime through the existing interface plus
five additions. Every backend that wants to support compose source
implements these; backends that opt out (or get them by inheriting a
default that returns `ErrNotImplemented`) cause `Engine.Up` to surface
`runtime.ErrComposeUnsupported` for compose sources, same shape as
today's failed `ComposeRuntime` type assertion.

```go
// runtime/runtime.go additions

// CreateNetwork creates a user-defined network with the given name and
// labels. Returns the network's backend ID. If a network with the same
// name and matching labels already exists, returns its ID without error
// (idempotent, like compose's own up behavior).
CreateNetwork(ctx context.Context, spec NetworkSpec) (string, error)

// RemoveNetwork removes a network by ID. No-op if missing.
RemoveNetwork(ctx context.Context, id string) error

// CreateVolume creates a named volume. Idempotent on name+label match.
CreateVolume(ctx context.Context, spec VolumeSpec) (string, error)

// RemoveVolume removes a volume by name. No-op if missing.
RemoveVolume(ctx context.Context, name string) error

// ListContainers returns containers matching every label in filter.
// Empty filter is rejected (we never want to list everything). The
// existing FindContainerByLabel becomes a thin wrapper over this.
ListContainers(ctx context.Context, filter LabelFilter) ([]Container, error)
```

`NetworkSpec`, `VolumeSpec`, `LabelFilter` are runtime-neutral structs:
name, labels, driver-options-as-string-map. No Docker-API types leak
through. Backends translate.

### 4.1 Why these specifically

Five additions cover everything the §2.1 in-scope feature set needs:

| Compose feature       | Primitive(s) used                           |
| --------------------- | ------------------------------------------- |
| Project network       | `CreateNetwork` / `RemoveNetwork`           |
| Named volumes         | `CreateVolume` / `RemoveVolume`             |
| `down` label scan     | `ListContainers`                            |
| Service start/stop    | existing `RunContainer` / `StartContainer` / `StopContainer` / `RemoveContainer` |
| Health gating         | existing `InspectContainer` (reads `State.Health.Status`) |
| Build                 | existing `BuildImage`                       |
| Pull                  | existing `PullImage`                        |
| `--rmi local`         | new image-prune primitive (§4.2)            |

### 4.2 The image-prune question

`docker compose down --rmi local` removes images the project built
locally, sparing pulled images. To replicate:

- Option A: add `ListImages(filter LabelFilter)` + `RemoveImage(id)`
  primitives. We stamp our built images with project labels at build
  time; down enumerates by label, removes.
- Option B: scope narrower — the orchestrator tracks built-image IDs
  in-memory during `Up` and only prunes those it created in the current
  process. Loses parity with compose (orphan images from crashed runs
  stick around) but avoids two more primitives.

**Decision (§14.6):** option A. Two more SDK methods on the docker
backend is trivial; orphan-image cleanup is a real ops requirement when
people iterate on feature definitions. Apple-container backend
implements via the image service `list` + `delete`.

### 4.3 Compose-runtime sub-interface removal

`runtime.ComposeRuntime` (runtime/runtime.go:19-35) is **deleted**.
`Engine.Up` no longer type-asserts; the compose source path always
calls `compose.Orchestrator`, which takes a `runtime.Runtime`. A backend
"supports compose source" iff it implements the §4 primitives — which is
spelled out at the type level (the `Runtime` interface gains them; a
backend that returns `ErrNotImplemented` from any of them is detected
on first call by the orchestrator and surfaces a clean error).

This is a v0 breaking change to `runtime.Runtime`. We're pre-1.0;
the existing public consumers are downstream tools maintained alongside this library. Migration cost is one PR in each.

## 5. Orchestration algorithm

### 5.1 Up

```text
Input: Plan {
    Project    *types.Project   // from compose-go
    ProjectName string
    Services   []string         // empty = all in profile
    Labels     map[string]string // project-wide additions
}
Output: map[serviceName]containerID, error

1. Validate: walk Plan.Project, reject §2.2 fields, return
   *UnsupportedFieldError if any. Done before any side effects.

2. Topo-sort: build DAG from depends_on (+ network_mode: service:x
   counts as an edge). Detect cycles → typed error. Result is a list
   of "levels": independent services within a level can start in
   parallel; levels run sequentially.

3. Ensure infrastructure:
   - CreateNetwork(<project>_default, labels)
   - For each top-level volume: CreateVolume(<project>_<name>, labels)
   Both are idempotent on (name, labels) match; safe to re-run.

4. For each level, in order:
   a. For each service in level (parallel within level):
      i.   Check existing container by labels
           (com.docker.compose.project + .service).
      ii.  If exists and config-hash matches and state is running:
           reuse — record container ID, continue.
      iii. If exists and config-hash differs OR not running:
           stop (if running), remove, fall through.
      iv.  Translate service → RunSpec:
           - Labels: §3.1 set merged with service labels
           - Mounts: bind mounts + volume mounts (volumes use
             <project>_<name>)
           - Env, command, entrypoint, ports, restart, healthcheck:
             passthrough
           - Network: join <project>_default with service-name alias
           - Hostname: service name (compose default)
      v.   RunContainer + StartContainer.
      vi.  Record container ID.
   b. After level completes, gate next level: for each dependency edge
      from level N+1 to level N with condition: service_healthy or
      service_completed_successfully, poll InspectContainer until the
      condition is met or timeout. Default timeout: 60s per dependency,
      configurable via SpecCompose.HealthTimeout. condition:
      service_started (the default) needs no polling.

5. Return service → container ID map.
```

### 5.2 Down

```text
Input: DownPlan { ProjectName string, RemoveImages bool, RemoveVolumes bool }

1. ListContainers(filter: com.docker.compose.project = <name>).
2. Topologically reverse: stop dependents first. Use depends_on if
   the compose project is available; fall back to "all in parallel"
   if only the project name is known (Down can be called without a
   project file, e.g. workspace cleanup from cached state).
3. For each container: StopContainer (10s timeout) → RemoveContainer.
4. RemoveNetwork(<project>_default).
5. If RemoveVolumes: ListContainers labelled with the project may have
   already pinned volumes; after container removal, look up volumes
   by label and RemoveVolume each.
6. If RemoveImages: ListImages(filter: com.docker.compose.project
   = <name> + dev.containers.built=true) → RemoveImage each.
```

### 5.3 Failure handling

- **Partial up:** any service fails to create or start → orchestrator
  stops short, returns a typed `*PartialUpError` listing which services
  came up and which didn't, plus the underlying error. **Does not roll
  back** by default — leaving the started containers makes debugging
  vastly easier (the user can exec into them, read logs). `Engine.Up`
  surfaces this; the caller may then issue `Down` to clean up.
- **Idempotent retry:** re-running `Up` after a partial failure resumes
  cleanly — services already running with matching config-hash are
  reused; only the failed ones are retried.
- **Health-gate timeout:** typed `*HealthTimeoutError` naming the
  service that didn't become healthy. The dependent services are not
  started.

## 6. Mapping to `runtime.Runtime` methods

Same shape as `runtime-applecontainer.md` §8.

| Orchestrator step                     | `Runtime` method               | Notes |
| ------------------------------------- | ------------------------------ | ----- |
| Build service with `build:`           | `BuildImage`                   | Existing. Build tag goes into orchestrator's image map; no override file needed (we pass image ref directly to `RunContainer`). |
| Pull service with `image:` (missing)  | `PullImage`                    | Orchestrator checks `InspectImage` first; pull only on miss. Compose v2 also pulls-on-miss. |
| Create project network                | `CreateNetwork` (new)          | Once per project. Idempotent. |
| Create named volume                   | `CreateVolume` (new)           | Once per declared volume. Idempotent. |
| Existing container lookup             | `ListContainers` (new)         | Filter by `com.docker.compose.project` + `.service`. |
| Diff existing container               | `InspectContainer` + own hash  | Computed in pkg `compose`; no runtime change. |
| Create + start service container      | `RunContainer` + `StartContainer` | Existing. |
| Wait `condition: service_healthy`     | `InspectContainer` (poll)      | Read `State.Health.Status`. Backend responsibility to populate. |
| Wait `service_completed_successfully` | `InspectContainer` (poll)      | Read `State.Status == "exited"` + `ExitCode == 0`. |
| `Down`: list                          | `ListContainers` (new)         | Filter by project label. |
| `Down`: stop + remove                 | `StopContainer` + `RemoveContainer` | Existing. |
| `Down --volumes`                      | `RemoveVolume` (new)           | Enumerate by project label first. |
| `Down --rmi local`                    | `ListImages` + `RemoveImage` (new, §4.2) | Filter `dev.containers.built=true` + project. |

Everything Apple-container-specific is invisible to the orchestrator —
it just calls these methods on whatever `runtime.Runtime` it was handed.

## 7. Apple-container backend implications

`runtime/applecontainer` (M6) inherits a `Runtime` interface that now
includes the §4 primitives. The `runtime-applecontainer.md` §8 mapping
table needs a five-row extension. Sketch (validated, not yet probed —
see §11):

| Primitive          | Apple API                                              | Confidence |
| ------------------ | ------------------------------------------------------ | ---------- |
| `CreateNetwork`    | `NetworkService.create` (apple/container 0.12 ships explicit network mgmt; per-container vmnet allocation is the default but user-defined nets exist) | Medium — need to probe networking model |
| `RemoveNetwork`    | `NetworkService.delete`                                | Medium |
| `CreateVolume`     | Open question — Apple's model is bind-mount centric; "named volume" may need synthesis as a `~/.devcontainer-go/volumes/<name>` dir bind-mounted in. | Low — likely the biggest open question |
| `RemoveVolume`     | rmdir if synthetic; native if Apple gains volumes      | Low |
| `ListContainers`   | `ContainerClient.list` + client-side label filter (already in `runtime-applecontainer.md` §8 for `FindContainerByLabel`) | High |
| `ListImages`       | image service `list`                                   | High |
| `RemoveImage`      | image service `delete`                                 | High |

The `CreateVolume` question is the load-bearing unknown. The validation
probe (§11.1 below) must answer it before PR15 starts. If Apple's
networking model can't provide service-name DNS resolution within a
project (a hard compose semantic — `app` resolves to the `app` service's
IP), the entire compose-on-apple story is blocked at the spec level,
not at our code.

**Open question (§12.1):** can Apple's networking guarantee that
container `db` reaches container `app` by hostname `app`? If no,
compose-on-apple is feature-gated off in M7 and waits for an Apple
networking update.

## 8. compose-go usage — unchanged from M4

`compose/load.go` stays as-is. The hybrid model (parse via compose-go,
execute via our code) is exactly what this design extends: the
"execute" half moves from shell-out to in-process. Parser usage doesn't
change.

`compose/override.go` mostly disappears. Today it writes `dc-build.yaml`
and `dc-run.yaml` to a tmpdir for the `docker compose -f` pipeline to
ingest. In the new world, those overrides apply as **in-memory mutation
of the `*types.Project`** before `Plan` runs:

```go
proj, _ := compose.Load(spec)
compose.ApplyBuildOverride(proj, primaryService, featureImage)
compose.ApplyRunOverride(proj, primaryService, workspaceMount, env, labels)
plan := compose.NewPlan(proj, projectName, runServices)
err := orch.Up(ctx, plan)
```

No more YAML round-trip, no more tmpfiles, no more `!reset` tag
compatibility checks. The override logic is the same; the serialization
boundary goes away.

## 9. Build & test discipline

### 9.1 Test layout

| Test type                            | Where                              | What it covers |
| ------------------------------------ | ---------------------------------- | -------------- |
| Pure unit: Plan validation           | `compose/plan_test.go`             | §2.2 refusal cases, topo-sort, cycle detection |
| Pure unit: config-hash determinism   | `compose/hash_test.go`             | Same input → same hash; field-order independence |
| Mock-runtime: orchestrator flow      | `compose/orchestrator_test.go`     | Up/Down call sequences against a fake `runtime.Runtime` |
| Integration: real Docker             | `compose/orchestrator_docker_test.go` (build tag `integration`) | PR12 fixture parity — 2-service compose, primary feature, sidecar DB. |
| Integration: real Apple container    | `compose/orchestrator_apple_test.go` (build tag `integration && darwin && arm64`) | Same fixture if §7 networking probe is green. |

The mock-runtime tests are the value: they exercise the orchestrator's
state machine without needing any backend installed, and they're the
backbone of refactoring confidence.

### 9.2 Parity acceptance

A run is "parity-acceptable" when, against the same compose project on
the same backend:

1. Container set after `Up` is identical (same images, same names, same
   network membership, same volume mounts).
2. `docker compose ps -a` shows our containers (Docker backend only,
   via the §3.1 interop labels).
3. `Down --rmi local --volumes` leaves no stragglers.
4. Re-running `Up` after `Down` succeeds with no manual cleanup.

The PR12 fixture is the canonical case. PR16 (§13) adds an apple-backend
counterpart conditional on §7 networking being viable.

### 9.3 CI

- Linux runners: docker-backend integration suite + all unit tests.
- macOS arm64 runner (added in M6 for `runtime/applecontainer`):
  apple-backend integration suite if §7 networking is viable.
- The `compose` package itself is platform-agnostic — no build tags;
  builds on every host.

## 10. Migration & rollout

A flag-day swap is risky; the existing compose path is the only one
shipped against real consumers since M4. Phased rollout:

1. **PR13 (this design): land orchestrator behind a feature flag.**
   `EngineOptions.ComposeBackend` enum: `Shellout` (current default) /
   `Native` (new). Wire native through `compose/orchestrator.go`; leave
   `runtime/docker/compose.go` and `runtime.ComposeRuntime` untouched.
2. **PR14: parity suite green for native on Docker.** Native flag
   tested against the PR12 fixture + a wider matrix of real-world
   compose projects (we mine the `examples/` directory in the
   devcontainers spec repo for candidates).
3. **PR15: apple-container primitives (§4 + §7 networking probe).**
   Adds `CreateNetwork` etc. to `runtime/applecontainer`. Native
   orchestrator runs against apple backend in an integration test
   if §11.1 probe is green; otherwise the apple compose path is
   feature-gated off with a typed error and §12.1 is escalated.
4. **PR16: flip the default.** `EngineOptions.ComposeBackend` defaults
   to `Native`. Shellout path stays available for one release.
5. **PR17 (next release): delete shellout.** Remove
   `runtime/docker/compose.go`, `runtime.ComposeRuntime`, the
   `EngineOptions.ComposeBackend` flag, the override-yaml writer in
   `compose/override.go`.

Each PR is independently shippable. PR13–14 are pre-M6 (Docker only,
no Apple risk). PR15–17 align with M6/M7 of the apple-container roadmap.

## 11. Spike findings — to run before PR13 starts

`runtime-applecontainer.md` §10 ran a two-day spike to validate
load-bearing assumptions before committing to the design. The same
discipline applies here. **Before** PR13 lands, run these probes;
record results here in a §11.1 subsection (same as the apple design).

### 11.1 Validation probes

Pattern from `runtime-applecontainer.md` §10.1: probes recorded inline,
results dated. Probes 3 + 4 run against `apple/container 0.12.3` on
macOS 15 / arm64 (2026-05-14).

1. **Determinism of `config-hash` input canonicalization —
   GREEN (2026-05-15).** Probe results (research artifact, not
   committed to the repo) — five tests, all pass:
   - 1000 iterations of `hash(baseService())` → identical sha256
     (`ac4987cf...316bb15a`). Validates Go's `encoding/json` sorts
     `map[string]T` keys deterministically — the entire premise the
     hash function rests on.
   - 500 trials with explicitly shuffled `Environment` and `Labels`
     map insertion order (using `math/rand` to defeat any incidental
     stability from Go's per-process randomization on small maps) →
     identical hash.
   - Distinct `*string` pointers with same string values
     (`MappingWithEquals = map[string]*string`) → identical hash.
   - Slice-order sensitivity confirmed: swapping `Volumes[0]`↔`[1]`
     or `Ports[0]`↔`[1]` changes the hash. Mount and port order are
     semantic; the hash correctly reflects that.
   - Field-change sensitivity confirmed: image ID, env value,
     command, label addition each change the hash.

   **Implementation note:** the hash function is one-liner-simple —
   `sha256(json.Marshal(struct{ImageID,Svc}))`. No custom
   canonicalization needed; `encoding/json` does the heavy lifting.
   The probe code is the reference implementation; PR13's
   `compose/hash.go` should keep it equally minimal. **Blocker
   discharged.**

2. **`compose-go` topo-sort or our own?** `compose-go/v2` exposes
   `types.Project.WithServicesEnvironmentResolved` but no public
   topological sort. Probe: write the 80-LOC topo-sort against the
   PR12 fixture's `depends_on` graph. If trivial, fine — we own it
   (compose.md §13.1 budgeted ~80 LOC). If `compose-go` has internal
   helpers worth surfacing via a small upstream PR, take that route
   instead. **Owner:** PR13 author. **Blocking:** no. **Status:** not
   yet run.

3. **Apple-container networking — service-name DNS — RED, with
   workaround (2026-05-14).** Two containers (`probe-a`, `probe-b`)
   started on user-defined network `probe-net` (subnet
   `192.168.66.0/24`). From `probe-a`:
   - `/etc/resolv.conf` → `nameserver 192.168.66.1` (network gateway)
   - `/etc/hosts` → only `127.0.0.1 localhost` and self
     (`192.168.66.3 probe-a`); **no entry for probe-b**
   - `getent ahosts probe-b` → empty
   - `nslookup probe-b` → `NXDOMAIN` from `192.168.66.1:53`
   - `ping probe-b` → `bad address 'probe-b'`
   - Same DNS gateway IP queried from `probe-a` itself: also NXDOMAIN
     — the gateway runs no DNS resolver for project containers.

   **L3 connectivity works**: `ping 192.168.66.2` (probe-b's IP) from
   probe-a → 1.5ms RTT, 0% loss. The gap is purely **name → IP
   resolution**, not network reachability.

   `container run` and `container create` in 0.12.3 expose **no
   `--add-host` / `--extra-hosts` equivalent**. The set of run-flags
   inspected: `--dns`, `--dns-domain`, `--dns-option`, `--dns-search`,
   `--no-dns`. There is no flag to seed `/etc/hosts` at create-time.

   IP discovery via `container inspect <id>` returns
   `networks[].ipv4Address` cleanly (e.g. `"192.168.66.2/24"`) — the
   orchestrator can read this post-start.

   **Workaround path for the orchestrator:**
   1. After all services on a level start, `InspectContainer` each
      one, harvest `ipv4Address`.
   2. For each service, `ExecContainer` to append the project's
      service→IP map to `/etc/hosts` inside the container
      (`echo "192.168.66.2 db" >> /etc/hosts`).
   3. Health gates run after the hosts patch.

   Cost: every service start gets one extra `Inspect` + one extra
   `Exec` for hosts-file patching. Race window: if service A starts,
   queries `db` before service B has been patched, the lookup fails.
   Mitigation: the level-based topo order plus the patch-before-health
   sequencing closes the window for `depends_on`-declared deps.
   Services within the same level that talk to each other (no
   `depends_on` edge) remain racy — document that intra-level peer
   discovery is not supported on the apple backend in M7.

   **Decision impact:** compose-on-apple is **viable with caveat**,
   not blocked. PR15 implements the hosts-file workaround. §14.10
   updated accordingly (apple backend implements compose iff the
   project's `depends_on` graph fully covers cross-service name
   references). The §12.1 open question downgrades from "hard
   blocker" to "documented limitation."

4. **Apple-container named volumes — GREEN (2026-05-14).** First-class
   support in 0.12.3:
   - `container volume create --label dc.test=true probe-vol` →
     created in <1s
   - `container volume inspect probe-vol` returns:
     ```json
     {
       "name": "probe-vol", "driver": "local", "format": "ext4",
       "labels": {"dc.test": "true"},
       "sizeInBytes": 549755813888,
       "source": ".../volumes/probe-vol/volume.img"
     }
     ```
   - Backed by a sparse 512 GiB ext4 disk image at
     `~/Library/Application Support/com.apple.container/volumes/`.
   - Mount works: `container run --volume probe-vol:/data alpine ...`
     wrote `marker.txt`; a subsequent (sequential) container reading
     the same volume saw the file. Persistence across container
     lifecycle: ✅.
   - Labels round-trip cleanly — load-bearing for our project
     scoping. ✅.

   **However: volumes are exclusively mounted.** Attempting to attach
   the same volume to two concurrently-running containers fails with:
   ```text
   Error: failed to bootstrap container ... (cause: "VZErrorDomain
   Code=2 'The storage device attachment is invalid.'")
   ```
   Root cause: virtio-block + ext4-on-disk-image; ext4 isn't a
   shared filesystem. This is a hard VM-level constraint, not a
   misconfiguration. Confirmed by repeating the test with one
   long-running container holding the volume and a second `--rm`
   container attempting attachment — second fails identically.

   **Decision impact:** §14.11 added — apple backend rejects
   project plans with one volume referenced by ≥2 services. Typed
   error `*VolumeSharedAcrossServicesError`. For devcontainer
   compose projects in the wild, shared volumes between services
   are rare (the workspace mount lives on the primary service only;
   sidecar databases mount their own data volumes). Acceptable
   constraint; documented at refusal time so users get a clear
   error rather than a confusing VZErrorDomain leak.

   Cleanup tested: `container volume rm probe-vol` removed the disk
   image cleanly. `container network rm probe-net` likewise.

5. **Recreation policy under iteration.** Write a service, run Up,
   change one env var, run Up again. Expected: the orchestrator
   detects the config-hash mismatch, stops + removes + recreates that
   service while leaving its dependencies alone. **Owner:** PR13
   author (via mock-runtime test, then again in integration).
   **Blocking:** no. **Status:** not yet run.

### 11.2 Upstream signal — Apple's plans (2026-05-14)

Reviewed before committing to PR13–17 to check whether either probe-3
(DNS) or probe-4 (shared volumes) might be resolved upstream on a
timeline that changes the calculus.

**On compose support (apple/container #230 — CLOSED, no Apple
intent to ship).** Filed 2026-02, closed without implementation.
Maintainer's position: "isn't in the scope of this GitHub project at
present" (same wording reused on the Docker-API issue below). Closure
points at community alternatives. Two community projects exist:
- `Mcrich23/Container-Compose` — a from-scratch compose runner against
  apple/container's CLI/API. Small scope, Vapor-developer driven.
- `socktainer/socktainer` (see below).

**On Docker Engine API compatibility (apple/container #1476/#1475 —
CLOSED).** Same maintainer response: out of scope, would be a separate
service plugin project. Points users at `socktainer`. No Apple-built
docker-API surface is on any roadmap.

**On internal container-name DNS (apple/container #856 — OPEN through
0.12.x).** This is exactly our probe-3 failure mode. Filed 2025-Q4
against 0.6.0; users in the thread report failures through 0.7.1,
0.11.0, and 0.12.3 (our probe version). Root cause discussion centers
on the host-level mDNSResponder occupying port 53 unreliably (Zscaler,
dnsmasq, etc. conflict). A related issue (`containerization #436`)
proposes a vsock-based DNS forwarder as the long-term fix; that issue
is also OPEN with no committed timeline. **Net: Apple intends DNS to
work but the implementation is flaky and has been for ~6 months. No
ship date.**

**On shared volume multi-attach (apple/container #889 — OPEN).**
Specifically requests read-only multi-attach (acknowledging RW must
stay exclusive). 2 comments, no Apple assignee, no milestone, no
priority signal. Filed 2026-05-01 — too new to expect movement. **Net:
The ext4-on-disk-image constraint we hit in probe 4 is acknowledged
upstream but unlikely to be resolved on M7's timeline.**

**Third-party Docker-API shim (`socktainer`) — considered, rejected
(2026-05-14).** A community project (`socktainer/socktainer`) exposes
a Docker Engine REST API over apple/container's stack and could, in
principle, let `docker compose` CLI drive the apple backend
unchanged. Not pursued: (a) inherits probe-3's DNS failure mode
unchanged (the broken layer is below socktainer in apple/container
itself), so the `/etc/hosts` workaround would still need to live
somewhere; (b) adds a second pre-1.0 dependency version-locked to
apple/container's minor releases, doubling our pinning surface;
(c) the runtime-agnostic orchestrator pays off across future backends
(podman, containerd) regardless of socktainer's evolution.

Recorded here so a future contributor doesn't re-open the question
without seeing the reasoning.

### 11.3 Pre-implementation risk discharge — extended probes (2026-05-14, 2026-05-15)

Probes 1–4 covered the load-bearing assumptions. Before committing to
PR13, a second pass exercised five more apple/container behaviors that
could force redesign rather than just implementation tweaks. Each
probe below ran against `apple/container 0.12.3` on macOS 15 / arm64.

**R1: Container labels round-trip — GREEN.**
`container run --label dev.containers.id=abc123 --label
com.docker.compose.project=dc-test ...` → `container inspect`
returned both labels intact under `configuration.labels`. Convergence
keys (§3.1), Down label-scan, and `Engine.Attach` lookups all work as
designed. **No design impact.**

**R1b: List filtering — CLIENT-SIDE ONLY.**
`container list` has no `--filter` flag in 0.12.3. The API may
support server-side filtering; the bridge / CLI does not surface it
yet. Implication: `ListContainers(filter LabelFilter)` is implemented
client-side on the apple backend, listing all containers and
filtering in Go. At our scale (single workspace, a handful of
services) the overhead is negligible. **No design impact;
implementation note.**

**R2: Healthchecks — RED on apple in 0.12.3.**
- `container run --help` has no `--health-cmd`, `--health-interval`,
  or related flags.
- `container inspect` output has no `health` key anywhere. Top-level
  keys: `networks`, `status`, `startedDate`, `configuration`.
  `status` is a plain string (`"running"`, `"stopped"`); no health
  state distinct from process state.
- Filed as `apple/container #1502` ("Reserve HealthStatus enum +
  health field on ContainerSnapshot") — acknowledged upstream, no
  ship date.

**Implication:** `depends_on: condition: service_healthy` cannot
function on the apple backend. Plan-time refusal required (§14.12
below). On the docker backend, healthchecks work as designed via
existing `InspectContainer` exposure of `State.Health.Status`.

**R2b: Exit code visibility on stopped containers — RED on apple in
0.12.3.**
- Started a container running `sh -c 'sleep 1; exit 42'`, polled
  inspect after exit.
- Inspect output for the stopped container shows `status: "stopped"`
  but **no exit code anywhere in the document**. Apple's `#1501`
  request (`Surface lastExitCode on ContainerSnapshot`) is exactly
  this gap, also unfixed.

**Implication:** `depends_on: condition: service_completed_successfully`
cannot function on the apple backend either — we can detect "exited"
but not whether the exit was clean. Plan-time refusal alongside R2
(§14.12).

Compound effect: on apple, `depends_on` effectively degrades to
`service_started` (the v1 semantics — "service exists, may or may not
be ready"). For most devcontainer compose projects this is workable
(devs usually want app to wait for db readiness anyway, and a retry
loop in the app code handles that). But projects that genuinely
depend on health-gating must run on the docker backend.

**R3: `/etc/hosts` writability for the probe-3 DNS workaround —
GREEN with caveat.**
- As root: `echo "192.168.66.99 testpeer" >> /etc/hosts` → succeeded;
  file is a plain bind-mount, not a read-only overlay.
- As `--user 1000`: write failed (`Permission denied`).
- **Critical recovery test:** start a container with `--user 1000`,
  then `container exec --user 0 ... echo >> /etc/hosts` → succeeded.

**Implication:** the probe-3 DNS workaround survives non-root service
defaults — we always issue the hosts-patch `Exec` with `--user 0`
regardless of the service's default user. Many real images (postgres,
nginx, node:*) run as non-root by default; this matters.

Secondary caveat: long-running processes inside services typically
cache name resolutions on first attempt. If service `app` makes its
first DNS query for `db` before the orchestrator's hosts patch lands,
the cached NXDOMAIN persists for the process's lifetime. Mitigation:
the orchestrator patches hosts **before** issuing the StartContainer
call. The container init may briefly see an empty hosts file, but
user processes inside don't run until after init completes the
config-pivot — by which time hosts is populated.

Implementation note: patching hosts pre-Start means the orchestrator
order is `RunContainer (create-not-start) → Inspect for IP → patch
hosts of dependents → StartContainer`. Already aligned with the §5.1
algorithm's create/start split.

**R4: Namespace sharing — architecturally impossible on apple.**
- `container run --help` has no `--pid`, `--ipc`, or `--uts` flags.
- `--network` accepts only a network name; no `container:<name>` or
  `none` modes.
- Root cause: VM-per-container model. Linux namespace sharing
  requires processes in the same kernel; Apple's containers run in
  separate Virtualization.framework VMs with separate kernels.

**Implication:** compose features that require namespace sharing —
`network_mode: service:<x>`, `pid: service:<x>`, `ipc: service:<x>`,
`network_mode: host`, `network_mode: none` — cannot work on apple at
the spec-architecture level (not just unimplemented). Plan-time
refusal on apple backend (§14.12). The §2.1 in-scope row for
`network_mode: service:<other>` is updated to **docker backend only**.

Devcontainer prevalence: sidecar patterns occasionally use
`network_mode: service:<primary>` to share network with the main
service (common for VPN sidecars, network debugging tools). Less
common in mainstream devcontainer compose projects.

**R5: `--restart` policy — not implemented in apple/container
0.12.3.**
- `container run --help` has no `--restart` flag.
- Filed upstream as `apple/container #286` (Open, no assignee, no
  milestone).

**Implication:** compose's `restart: always | unless-stopped |
on-failure` cannot be enforced on apple. Options:
(a) silently ignore — service crashes stay crashed
(b) refuse at Plan time with a typed error

**Decision (§14.13 below):** (a) — silently ignore on apple with a
single `EngineEvent` warning per Plan, not per service. Restart
policies are nice-to-have; refusing the entire Plan over them would
be heavy-handed. The warning code is new:
`WarnRestartPolicyIgnoredOnBackend`.

### 11.4 Updated risk register

| # | Risk | Backend | Status | Resolution |
|---|---|---|---|---|
| 1 | Labels round-trip | apple | GREEN | none |
| 1b | Server-side label filtering | apple | NO | client-side filter in `ListContainers` |
| 2 | `service_healthy` health gate | apple | RED | Plan-time refusal |
| 2b | `service_completed_successfully` | apple | RED | Plan-time refusal |
| 3 | `/etc/hosts` patch as root | apple | GREEN | always `exec --user 0` |
| 4 | Namespace-sharing modes | apple | RED | Plan-time refusal |
| 5 | `--restart` policy | apple | RED | Silently ignored + one-shot warning |
| 6 | `--add-host` flag | apple | NO | `/etc/hosts` patching workaround |
| 7 | compose-go `*types.Project` mutation safety | both | UNKNOWN | Probe in PR13 — fall back to YAML writer if hazardous |
| 8 | `BuildSpec` coverage of compose `build.*` | both | UNKNOWN | Field-by-field audit in PR13 |
| 9 | Anonymous volume handling | both | UNKNOWN | Treat as named volume with generated unique name |
| 10 | Cancellation mid-Up | both | UNKNOWN | Test in PR13 mock-runtime suite |

Risks 7–10 are implementation-discovery tasks, not redesign
triggers. They're listed so PR13 reviewers can verify the author
addressed them.

### 11.5 The capability-flag pattern

Risks 2, 2b, 4, 5 each gate a compose feature on a backend-specific
basis. Hardcoding `if backend == "applecontainer"` checks in the
Plan validator is exactly the coupling §14.10 / §14.11 already
called out. Concrete shape, refined by these new findings:

```go
// runtime/runtime.go addition

type Capabilities struct {
    // Healthchecks: backend can run HEALTHCHECK directives and the
    // orchestrator can read State.Health.Status via InspectContainer.
    Healthchecks bool

    // ExitCodes: InspectContainer returns the container's exit code
    // after Stop. Needed for condition: service_completed_successfully.
    ExitCodes bool

    // NamespaceSharing: backend supports network_mode/pid/ipc set to
    // service:<other> (Linux namespace sharing within one kernel).
    NamespaceSharing bool

    // RestartPolicies: backend enforces compose's restart: directive.
    RestartPolicies bool

    // SharedVolumes: a single named volume can be mounted into ≥2
    // running containers concurrently.
    SharedVolumes bool
}

// Runtime interface gains:
Capabilities() Capabilities
```

`runtime/docker` returns `{true, true, true, true, true}`.
`runtime/applecontainer` returns `{false, false, false, false,
false}` as of 0.12.3. Each capability flips to `true` independently
when Apple closes the corresponding upstream issue.

`compose.Plan.Validate(caps)` walks the `*types.Project` and emits
typed errors for any unsupported feature actually used by the
project. Backend-neutral; future runtimes self-describe via the same
struct.

### 11.6 Probe-derived decisions summary

- Probe 3 (DNS): RED on the original premise, GREEN on a workable
  variant via post-start `/etc/hosts` patching. Compose-on-apple
  ships with a documented limitation about intra-level peer
  discovery. Costs one extra Inspect + Exec per service start.
- Probe 4 (volumes): GREEN for orchestrator integration, RED for the
  shared-volume edge case. Reject shared volumes at Plan time on the
  apple backend with a clear typed error.
- Both probes confirm the §4 primitive surface is sufficient — no
  new primitive needed for the apple backend. `CreateNetwork` /
  `CreateVolume` map directly; `--add-host` is not a primitive but a
  post-start orchestrator step.

## 12. Open questions

These are integration details to resolve during PR13–17, not blockers
for the design itself (mirroring `runtime-applecontainer.md` §11):

1. **Long-term fix for Apple-container service-name DNS.** §7 + §11.1
   probe 3 resolved the v1 path: Apple has no built-in service-name
   DNS, but the post-start `/etc/hosts` patching workaround is viable
   and is what M7 ships (see §14.10). Open question is whether to
   pursue upstream support so we can drop the workaround later — a
   nice-to-have, not a blocker.

2. **`config-hash` interop with externally-run `docker compose`.** §3.3
   decision is to use our own label, not compose's. Confirm with one
   real workflow test: user runs our `Up`, then runs `docker compose
   ps` in the project dir — does it list our containers cleanly, or
   complain about config-hash format? If the latter, we may need to
   also stamp `com.docker.compose.config-hash` with a value compose
   accepts (even a bogus stable value), purely for read-side tooling.

3. **Per-service health timeout granularity.** Compose lets you set
   `healthcheck.timeout` per check, not per overall service. The
   orchestrator's "wait for healthy" timeout is per-dependency-edge.
   Decide whether to expose `compose.SpecCompose.HealthTimeout` as a
   global default + per-service override, or just a single global.
   Lean toward the simpler one; revisit if users ask.

4. **Build-time labelling for `--rmi local`.** §4.2 says we stamp
   built images with `dev.containers.built=true` + project label. Need
   to confirm the `BuildImage` SDK call accepts arbitrary labels on
   both backends; today the spec already has `BuildSpec.Labels` so
   this is likely just a documentation + plumbing item.

5. **Variable substitution timing.** §9 of `compose.md` says we pass
   user files unchanged. The override-merge step (§8 above) injects
   resolved values from `ResolvedConfig` — those must be substituted
   BEFORE merge into the `*types.Project`, not after, or compose-go
   will try to re-interpolate them and trip on literal `$` characters.
   Same hazard as the YAML path; just an implementation note.

## 13. M7 ship target

PR13–17 land sequentially; each gates on the previous. Detailed
breakdown lives in `design/status.md` once this design is approved.

In scope:

- `compose/orchestrator.go` + `compose/plan.go` + `compose/hash.go`
  + `compose/graph.go` (topo-sort + cycle detection) — the new
  runtime-agnostic Go package.
- `runtime.Runtime` interface additions (§4); migration of
  `runtime/docker` to implement them.
- Removal of `runtime.ComposeRuntime` sub-interface and
  `runtime/docker/compose.go` (PR17).
- `runtime/applecontainer` implementation of the §4 primitives,
  conditional on §11.1 probes 3 + 4.
- Parity integration suite re-running M4 PR12 fixture under both
  backends.
- `EngineOptions.ComposeBackend` migration flag (PR13–PR16);
  removed in PR17.

Out of scope for M7:

- Multiple named networks per project (compose's `networks:` map
  with multiple entries — still §2.2 refused).
- `external: true` resources.
- `secrets:` / `configs:`.
- `develop: watch:` (file sync). Cross-cutting with workspace mount
  semantics; separate design.
- Multi-replica services (`scale > 1`). Devcontainer use case absent.
- Compose features the Apple backend can't honor (health-gated
  `depends_on`, namespace-sharing modes, shared volumes) — refused at
  Plan time via typed errors per §14.11 / §14.12. The compose path
  itself ships on Apple via the `/etc/hosts` workaround (§14.10).

## 14. Decisions

Resolved during this design draft (2026-05-14):

1. **Runtime-agnostic orchestrator lives in `compose/`, not under
   any backend.** Compose semantics implement once; each backend
   implements the five §4 primitives. Justification: avoids per-backend
   compose duplication once we have ≥2 backends; Apple-container's
   M6 arrival makes ≥2 the imminent reality.

2. **`runtime.ComposeRuntime` sub-interface is deleted, not
   extended.** The sub-interface was a shape that fit shell-out; it
   doesn't fit the in-process model. Pre-1.0 breakage is acceptable;
   migration cost is one-PR-per-internal-consumer.

3. **Compose subset is empirical, not aspirational.** §2.1 is what we
   implement; §2.2 is what we refuse with typed errors; §2.3 is
   silently passed through where the backend supports it. Spec drift
   is tracked by failing integration tests on real-world fixtures, not
   by trying to anticipate features.

4. **compose-go stays as the parser.** Hybrid model from M4
   (`compose.md` §13.1) keeps compose-go for parse + interpolation +
   extends + profiles. We only stop using it for orchestration.

5. **Our own `dev.containers.config-hash`, not compose's.** §3.3 — we
   own the recreation policy; we accept that external `docker compose`
   sees our containers as "always stale." `com.docker.compose.*` labels
   still get written for `ps` / `logs` interop.

6. **`--rmi local` parity: stamp built images, prune by label.** §4.2
   — two new primitives (`ListImages`, `RemoveImage`), worth the cost
   to give users real `down --rmi local` semantics. The
   in-memory-track alternative is too narrow.

7. **Phased rollout via `EngineOptions.ComposeBackend` flag.** §10 —
   shellout stays default for one release, then deleted. No flag day;
   real users can fall back if a regression surfaces.

8. **Spike before commit.** §11.1 — three of the five probes (1, 3, 4)
   gate parts of PR13/PR15; do them before code lands. This is the
   same discipline `runtime-applecontainer.md` §10.1 used, and it paid
   off there (probe 3 changed decision §13.8 on UID handling).

9. **Failure handling: no automatic rollback.** §5.3 — partial Up
   leaves running services in place for debuggability. The user (or
   `Engine.Down`) cleans up. Matches `docker compose` behavior.

10. **Apple-container compose is conditional, not required.** §7 +
    §11.1 — if Apple's networking can't do service-name DNS, the
    apple-container backend explicitly refuses compose source via
    `ErrComposeUnsupportedOnBackend`. The orchestrator's existence
    is still justified (Docker-side cleanup, removing the shell-out)
    but the runtime-agnostic claim weakens to "agnostic across
    backends that meet the §4 primitive + service-name-DNS bar."
    **Probe 11.1 #3 (2026-05-14):** Apple has no service-name DNS,
    but the workaround (post-start `/etc/hosts` patching driven by
    `Inspect` + `Exec`) is viable. Compose-on-apple ships with a
    documented intra-level peer-discovery limitation; not blocked.

11. **Apple backend rejects shared volumes (probe-derived,
    2026-05-14).** §11.1 #4 confirmed Apple's volumes are
    exclusively mounted (ext4-on-disk-image, can't multi-attach).
    Plan-time check on the apple backend: if any volume is
    referenced by ≥2 services, return
    `*VolumeSharedAcrossServicesError` before any side effects.
    Docker backend has no such restriction. The orchestrator
    surfaces backend-specific constraints via a small
    `runtime.Capabilities()` method (concrete shape in §11.5) so
    the Plan validator can read them without hardcoding backend
    identity.

12. **Apple backend rejects health-gated `depends_on` and
    namespace-sharing modes (probe-derived, 2026-05-14).** §11.3
    probes R2, R2b, R4 confirmed that on apple/container 0.12.3:
    - `depends_on.<svc>.condition: service_healthy` cannot work
      (no healthcheck system).
    - `depends_on.<svc>.condition: service_completed_successfully`
      cannot work (no exit code in inspect output).
    - `network_mode: service:<x>`, `pid: service:<x>`,
      `ipc: service:<x>`, `network_mode: host`, `network_mode: none`
      cannot work (VM-per-container, no kernel-shared namespaces).
    Each is refused at Plan time on the apple backend via a typed
    `*UnsupportedFeatureOnBackendError` listing the capability flag
    name (`Healthchecks`, `ExitCodes`, `NamespaceSharing`). Docker
    backend is unaffected. The capability struct in §11.5 is the
    single point of truth.

13. **`restart:` policy silently ignored on apple, with one-shot
    warning (probe-derived, 2026-05-14).** §11.3 probe R5 confirmed
    no `--restart` flag in apple/container 0.12.3. Plan-time
    refusal would be heavy-handed (the project still runs
    correctly; restart-on-crash is a robustness feature, not a
    correctness one). Instead: the orchestrator emits a single
    `WarnRestartPolicyIgnoredOnBackend` event when at least one
    service in the project declares `restart:` and the active
    backend's `Capabilities().RestartPolicies` is false. One
    warning per Plan, not per service. Once Apple ships `--restart`
    (upstream `#286`), the capability flag flips to true and the
    warning self-suppresses.

---

## Appendix A: relationship to `compose.md`

`compose.md` is the shell-out design; this design supersedes its
§13 "future Go-native" sketch. Concrete deltas:

- `compose.md` §3 `ComposeRuntime` interface → deleted (this §4.3).
- `compose.md` §4 override-file generation → in-memory `*types.Project`
  mutation (this §8).
- `compose.md` §13.1 "~520 LOC" estimate → revised to ~800–1000 LOC
  including the convergence diff + health gating + label scheme +
  unsupported-field validator + cycle detection. Larger than the
  sketch because real-world correctness (recreation policy, partial
  failure, error typing) is more code than the algorithmic core.
- `compose.md` §13.3 stability table → applies unchanged. The
  Medium-stability rows (compose label set, config-hash) are
  acknowledged risks we sign up for.
- `compose.md` §13.6 revisit criteria → criterion #4 (packaging
  reason) is now met by Apple-container support. This design IS the
  revisit.

`compose.md` stays in the repo as the historical record. When PR17
deletes the shellout path, `compose.md` gets a status banner
("Superseded by `compose-native.md`; retained for context") rather
than deletion — the rationale captured there (especially §13's
"what it costs us forever") is still load-bearing on this design.
