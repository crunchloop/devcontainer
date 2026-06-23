# Design — Orchestrator-driven health probing (Podman)

**Status:** Draft for review
**Date:** 2026-06-23
**Scope:** Make the native compose orchestrator (`compose/orchestrator.go`)
run healthcheck probes itself via `Exec` on backends whose native
HEALTHCHECK is unsafe, instead of configuring the backend's native
healthcheck. Fixes RabbitMQ (and any privilege-dropping image) failing to
boot under the Podman backend.

Companion to `design/compose-native.md` (the orchestrator) and
`design/podman-backend.md` (the Podman runtime).

---

## 1. Problem

A `rabbitmq` service in a multi-service compose devcontainer exits 1 under
the Podman backend:

```
Error when reading /var/lib/rabbitmq/.erlang.cookie: eacces
```

Root cause (reproduced deterministically, incl. bare `podman run
--health-cmd ...` — i.e. a Podman behavior, not a bug in our translation):

1. The rabbitmq image runs as root; its entrypoint `gosu`-drops
   `rabbitmq-server` to uid 999.
2. **Podman runs the container HEALTHCHECK as root and fires the first
   probe ~1-2s after start** (it does *not* defer the first probe for
   `start_period`).
3. rabbitmq's healthcheck `rabbitmq-diagnostics -q ping` is an Erlang
   escript. Run as root with `HOME=/var/lib/rabbitmq` and no cookie yet,
   it **creates `/var/lib/rabbitmq/.erlang.cookie` owned root:root,
   mode 400**.
4. The uid-999 server then cannot read root's cookie → `eacces` → exit 1.

Docker is unaffected because it defers the first probe past the interval,
so the 999 server writes the `999:999` cookie first; a later root probe
just reads it (root bypasses DAC). Only Erlang-cookie-style images are hit
— `pg_isready` / `curl` probes don't create root-owned files the main
process must read.

Evidence: no healthcheck → works; `CMD true` healthcheck → works; real
healthcheck (even with `start_period: 120s`) → fails; first probe deferred
until after server init → works.

## 2. Decision

On backends that opt in, the orchestrator **owns health probing**:

- **Do not** set `RunSpec.HealthCheck` (the backend never runs a native
  HEALTHCHECK, so no eager root probe).
- For `depends_on.<svc>.condition: service_healthy` gates, the orchestrator
  runs the service's compose `test` command itself via `ExecContainer`,
  polling until it exits 0 (healthy) or `HealthTimeout` (→
  `HealthTimeoutError`).

Because probing is driven by the gate loop — which runs only *after* the
level's containers have reached running — the first probe lands after the
service has begun initializing, mirroring Docker's deferred-first-probe
behavior. `start_period` is honored as a grace delay before the first
probe.

Services that declare a healthcheck but are **not** gated by any dependent
(e.g. rabbitmq, when the dependent depends on it with the list/`service_started`
form) are simply never probed — they run without a health status, which is
correct: nothing consumes it, and the eager-root-probe crash is gone.

### 2.1 Backend opt-in

A backend signals the preference through an optional, consumer-defined
interface (Go idiom — the orchestrator declares what it needs):

```go
// in compose/
type selfHealthProber interface { PreferSelfProbedHealth() bool }
```

`runtime/podman.Runtime` implements it returning `true`. Docker and
Apple do not implement it → unchanged behavior (Docker keeps native
healthchecks + `InspectContainer().Health` gating; Apple keeps its
`Healthchecks: false` fallback).

Rejected alternatives:
- A `runtime.Capabilities` bit — `Capabilities` models compose *feature*
  support; "native healthcheck runs eagerly as root" is a backend quirk,
  not a feature. Keeping it out of `Capabilities` avoids conflating the two.
- Type-asserting `*podman.Runtime` in the engine — couples the engine to a
  concrete backend and risks an import cycle.

### 2.2 Probe semantics

- `test` is compose-normalized to `["CMD", arg...]`, `["CMD-SHELL", str]`,
  or `["NONE"]`. CMD → `ExecOptions.Cmd = test[1:]`; CMD-SHELL →
  `["/bin/sh", "-c", test[1]]`; NONE / empty / `Disable` → treated as "no
  healthcheck" → fall back to `State == Running` (same as today's
  `HealthNone`).
- Exec runs as the container's default user (`ExecOptions.User=""`). Safe
  because probing is deferred: by the time we probe, the main process has
  initialized. (Residual edge: a *gated* Erlang-cookie service with no
  `start_period` could still be probed before its server writes the cookie;
  `start_period` closes that. The motivating project does not hit this —
  its rabbitmq is not gated.)
- Exit 0 → healthy → gate satisfied. Non-zero → keep polling until deadline.

## 3. Scope of change

- `runtime/podman/podman.go`: add `PreferSelfProbedHealth() bool`.
- `compose/orchestrator.go`: detect opt-in; omit `HealthCheck` from the
  RunSpec when opted in; in `waitFor`, self-probe `service_healthy` gates
  via Exec using the service's healthcheck spec.
- Tests: unit (test→exec translation; self-probe gate satisfied/timeout via
  a fake runtime). Validated manually end-to-end against a real
  multi-service compose devcontainer on Podman (OrbStack) — rabbitmq boots
  clean; a Podman integration test is follow-up work.

No change to the Docker or Apple paths.

## 3.1 Two coexisting health-check paths (the divergence)

This change introduces a **second** health mechanism rather than replacing
the first, so the orchestrator now has two, selected per-backend by
`selfProbe`:

| | Native path (Docker, Apple) | Self-probe path (Podman) |
|---|---|---|
| Healthcheck on container | `RunSpec.HealthCheck` set; runtime runs it | omitted |
| Who probes | the runtime, as the container's user | the orchestrator, via `ExecContainer` |
| `service_healthy` gate reads | `InspectContainer().Health` | exit code of the exec'd `test` |
| Selected when | `selfProbe == false` | `selfProbe == true` |

**Behavioral divergence to be aware of**, beyond two code paths to
maintain: on the native path *every* service with a `healthcheck:` gets a
persistent health status surfaced by the runtime (`podman ps` / `docker ps`
HEALTH column). On the self-probe path the native healthcheck is omitted
entirely, so (a) only services actually gated by a `service_healthy`
dependency are ever probed, and (b) the probe result is transient — used to
satisfy the gate, never surfaced as the container's reported health. For
the motivating project this is invisible (nothing consumes rabbitmq's
health; a `service → postgres` `service_healthy` gate still works via the
self-probe), but the HEALTH column differs
between backends.

This fork is deliberate — Podman's eager-root healthcheck forced it — not
accidental. Convergence options, to revisit when the `compose-native`
rollout flips the default backend (`design/compose-native.md` §10):
1. Unify on self-probe for all backends — one path, Docker parity, but lose
   the runtime's native HEALTH surface.
2. Keep the fork but also set a native healthcheck for non-gated services on
   the self-probe path where safe — narrows the divergence, more complex.
3. Leave as-is, documented (current choice).

## 4. Out of scope — rootless Podman + gosu

A *distinct* failure exists for the same image class under **rootless**
Podman: `gosu`'s setuid/setgid privilege drop is denied by the user
namespace (containers/podman#6816), so `rabbitmq-server` can't drop to uid
999 at all. That is a different mechanism from the healthcheck race fixed
here and this change does **not** address it. Our validation is on
**rootful** Podman (the C/R target — root socket), where gosu works; if a
rootless deployment is ever targeted, rabbitmq needs a separate remedy
(e.g. `user:` pinning + uid-mapped volumes). Confirmed via #20893 that
`--health-start-period` is doc-only and never defers the first probe, so it
is not a viable alternative.
