**Applies to:** every path in this repository.

This repository's review directives — the mechanism catalogue `crunchloop/devcontainer`
is reviewed against. The platform prompt owns the method (what a finding is, how it is
verified, how severity is derived) and a short dimension **label** set (`D1`-`D14`);
this file owns the **mechanisms** behind those labels for this codebase, under `R*` ids.

Every bullet names a mechanism, not a preference: something a reviewer can confirm or
kill by reading code. An observation that cannot be reduced to "this call reaches that
state without passing that guard" is not a finding — it is a design opinion, and it
belongs in SUSPICIONS or nowhere.

**How this file grows.** Every bug that reaches `main` and is later understood earns one
line, written as the **bug shape**, not the incident. The lines below were derived from
the `Fixed` entries in [`CHANGELOG.md`](../../CHANGELOG.md) — each one has already
escaped to `main` at least once. Lines that never fire in review should be deleted; a
checklist nobody reads is worse than a short one.

**Read these first**, from the base ref: [`PRD.md`](../../PRD.md) (scope and non-goals),
[`CONTRIBUTING.md`](../../CONTRIBUTING.md), and whichever record under
[`design/`](../../design) the diff touches. A change that contradicts its own design
record is a finding, and you cannot see that without opening the record.

## R1. Resolved but never applied

Refines `D11`. **The single most productive shape in this repository.**

Configuration flows `devcontainer.json` → `config` parsing → `ResolvedConfig` → a runtime
or compose call that actually sets it on a container. A field that lands in `ResolvedConfig`
and is never read at the boundary produces a devcontainer that silently ignores it.

- For any new or changed config field, name the **consumer**: the `runtime/` or `compose/`
  call site that puts it on the container. No consumer is the finding, and it is at least
  HIGH when the field is security-relevant.
- Precedent: feature security metadata (`privileged`, `init`, `capAdd`, `securityOpt`) and
  feature `entrypoint` scripts were merged into `ResolvedConfig` but never carried onto
  docker-compose services, so **docker-in-docker silently failed** on compose-source
  devcontainers — the daemon came up unprivileged and its entrypoint never ran (#103).
- The inverse counts too: a value read at the boundary that no parsing path can ever set.

## R2. Path parity — native, shell-out, and each backend

Refines `D1`. This repository implements the same behaviour more than once by design.

- **compose** has a native orchestrator (`compose/orchestrator.go`) and a shell-out path
  (`docker compose`). A fix, guard, or flag added to one and not the other is a finding —
  name the sibling call site and say what it does instead. Both paths carried the same
  recreate bug (#71, #72) and the same entrypoint gap (#103).
- **runtime** has `docker` and `applecontainer` backends behind one interface.
  A change to shared orchestration must state what each backend does with it; a change
  inside one backend must say whether the other needs the same. Apple diverges from
  Docker in ways that have already broken workspaces (below).
- A capability flag on `Capabilities()` (`ServiceNameDNS`, for instance) is the
  legitimate way to encode divergence. A silent assumption that all backends behave like
  Docker is not.

## R3. Destructive recreate

Refines `D10`. Severity floor **HIGH**: the failure destroys user data with no recovery.

Recreating a container discards its writable layer — `$HOME`, shell history, install
caches, agent state. Anything that widens the "recreate" predicate is a data-loss change.

- Any change to the config-hash / drift comparison (`compose/hash.go` and callers), or to
  the decision between `StartContainer` and stop+remove+create, must be argued against
  **spurious** drift: a temporarily-stopped container after a daemon restart, or a hash
  input that is not actually part of the container's identity.
- Precedent: both compose paths treated a stopped-but-matching container as a recreate
  signal and destroyed the writable layer on every daemon restart (#71, #72).
- A new field added to a hash input is a recreate trigger for every existing workspace.
  Say so, and say whether that is intended.

## R4. Lifecycle and probe ordering

Refines `D10`. The lifecycle is a fixed sequence and steps read state earlier steps produce.

- A step that consumes state produced later in the chain sees the zero value. Precedent:
  `probedEnv` was empty for the whole lifecycle chain, so `postCreateCommand` ran without
  rc-derived `PATH` and nvm/asdf/pnpm binaries were invisible — a divergence from
  `@devcontainers/cli` that presented as `command not found` (v0.1.4).
- For a new or moved lifecycle step, state where it sits in the sequence and which state it
  reads. For a change to what a step populates, name every consumer downstream of it.

## R5. Reference-CLI divergence

Refines `D11`. The reference implementation is `devcontainers/cli`, and matching it is the
product. A behavioural difference is a finding even when the local behaviour is defensible.

- When the diff changes observable behaviour (flag semantics, defaults, ordering, what ends
  up on the container), say what the reference CLI does. If the change deliberately diverges,
  the divergence belongs in the design record and the CHANGELOG, and its absence is the
  finding.
- The devcontainer spec — feature merge order, metadata precedence, variable substitution —
  is the same kind of contract. Cite the spec behaviour you are checking against.

## R6. Build-context and cache determinism

Refines `D10`/`D12`. Cache misses here are measured in gigabytes, not milliseconds.

- Anything that writes into a build context or tar stream must produce byte-identical
  output for identical inputs: no wall-clock `ModTime`, no host uid/gid, no map iteration
  order. Precedent: `tarDirectory` stamped wall-clock mtimes and host uid/gid into every
  entry, so BuildKit's `COPY` digest changed between runs and downstream snapshot/restore
  pipelines re-extracted whole images, pushing 30Gi volumes toward ENOSPC (#86).
- A synthesized file (`useruid`'s generated `Dockerfile`/scripts, generated compose
  overrides, entrypoint wrappers) must pin its own timestamps — reproducibility has to be a
  local property of the synthesizer, not a property of the caller.

## R7. Go mechanics that have teeth here

Refines `D10`. Only the ones that actually bite in this codebase — `golangci-lint` owns style.

- **Context**: a long-running operation that ignores `ctx` cannot be cancelled, and `Up`
  paths are cancelled routinely. Name the call that drops it. See `runtime/cancellable.go`
  for the intended shape.
- **Goroutine and channel lifetime**: every goroutine started needs a termination path, and
  every channel handed out (`ExecOptions.ResizeCh`, event-bus subscriptions in
  `eventbus.go`) needs a documented closer. A send on a channel nobody drains after an
  early return is a hang, not a leak.
- **Error handling**: errors must be wrapped with `%w` and typed where `runtime/errors.go`,
  `compose/errors.go`, and [`design/structured-errors.md`](../../design/structured-errors.md)
  define a type. An error swallowed into a bool, or a `WarnEvent` where the caller needed a
  failure (or the reverse), changes what the operator sees — that is `D12`, not cosmetics.
- **Process invocation**: arguments assembled into `exec.Command` from config values are
  untrusted input (`D9`). Interpolation into a generated shell wrapper — the entrypoint
  chaining path does this — needs quoting stated, not assumed.
- Concurrent map access and captured loop variables in spawned goroutines, where the diff
  actually spawns them.

## R8. Testing

Refines `D13`. The layout is in `CONTRIBUTING.md`: unit tests beside the code, integration
tests under `test/integration/` behind `//go:build integration`.

- A behaviour change whose only new test asserts against a fake runtime, when the behaviour
  is *about* how a real backend responds, is a coverage gap — say which integration test
  would have caught it.
- A test that would still pass with the change reverted is not coverage. For each new test,
  name the invariant it pins.
- Backend-specific behaviour (R2) needs a test per backend it claims to support, or an
  explicit statement of which backend is untested.

## Out of bounds

Do not file these here:

- Style, formatting, naming, and lint-adjacent nits — `golangci-lint` runs in CI and owns
  them. A finding that `make lint` would have produced is noise.
- Scope objections already settled by [`PRD.md`](../../PRD.md) §4 Non-goals, and the
  documented `Known limitations` in the CHANGELOG. Absence of a non-goal is not a defect.
- Dependency version bumps with no code change, beyond an actual incompatibility you can
  point at in the diff.
- The Swift bridge under `applecontainer-bridge/` unless the diff touches it.
