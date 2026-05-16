# Design — Structured errors and failure codes

**Status:** Accepted
**Date:** 2026-05-13
**Scope:** the error return surface of every package that participates
in `Engine.Up` and `Engine.Down`. Companion to
`design/resolved-config.md` (where validation errors originate) and
`design/runtime.md` (where most subprocess failures surface).

This document does *not* introduce hint UX, alerting, or user-facing
strings — those belong to downstream consumers (e.g. a CLI hint emitter). The library's job is to expose enough structure for
those consumers to build hints without string-matching `err.Error()`.

---

## 1. Problem

The library does the right things on the happy path: the event bus
emits `BuildStartEvent`, `BuildCompletedEvent`,
`ContainerCreatingEvent`, `ConfigWarningEvent`, etc., each with typed
fields a caller can inspect.

The failure path doesn't have the same shape. Across the codebase
there are ~340 `fmt.Errorf("…: %w", err)` / `errors.New(…)` sites.
Two examples picked at random:

```go
// up.go:169
return nil, fmt.Errorf("find existing container: %w", err)

// useruid.go:84
return "", fmt.Errorf("create uid build context: %w", err)
```

A caller receives a wrapped string. To distinguish "registry
authentication denied" from "image not found" from "feature install
script exit 1" from "compose service crashloop", it must pattern-match
the `err.Error()` text or the wrapped Docker SDK error. That coupling
is brittle: each library refactor risks invalidating downstream
detectors that were never imported by any test.

Two small islands of structured errors already exist:

- `config/errors.go` — `ConfigParseError`, `ConfigInvalidError`.
- `runtime/errors.go` — `ImageNotFoundError`, `ExecFailedError` (with
  captured `Stderr`), `ComposeFailedError` (with `Stderr`),
  `DaemonUnavailableError`, `ComposeUnavailableError`.

The proposal generalises that pattern: every public failure path
returns a `*devcontainer.Error` carrying a stable `Code`, and the
existing runtime-level typed errors continue to live as `Cause`,
exposing captured stderr via a small interface.

---

## 2. Proposal overview

```
   caller (your application)              errors.As(err, &dcErr)
   ─────────────────────────            ─────────────────────────
            ▲                                     │
            │ error return                        ▼
   ┌────────┴────────────────┐         ┌──────────────────┐
   │  devcontainer library   │         │  devcontainer.   │
   │                         │         │  Error{          │
   │  every failure path     │  ───►   │    Code,         │
   │  wraps the underlying   │         │    Reason,       │
   │  error in a typed       │         │    Cause,        │
   │  *devcontainer.Error    │         │    Context,      │
   │                         │         │  }               │
   └─────────────────────────┘         └──────────────────┘
                                                │
                                                │ optional, via Cause
                                                ▼
                                       ┌──────────────────┐
                                       │ StderrCarrier    │
                                       │   Stderr() string│
                                       └──────────────────┘
```

Three additions:

1. A `devcontainer.Error` type with `Code`, `Reason`, `Cause`,
   `Context`. Returned from every public function that today returns
   an unstructured error.
2. `Code` defined as a flat, `string`-typed enum with exported
   constants. The library owns the codes that describe generic
   devcontainer failure modes; downstream consumers layer their own
   catalogs on top of these.
3. A `StderrCarrier` interface satisfied by existing runtime typed
   errors (`ExecFailedError`, `ComposeFailedError`). Callers fetch
   captured stderr from the `Cause` chain via `errors.As`; the
   wrapper carries no stderr field of its own.

The existing event bus is unchanged in this proposal. §6 discusses an
optional follow-up.

---

## 3. The `Error` type

```go
// devcontainer/error.go (new file)

package devcontainer

// Code identifies a specific failure mode. Codes are flat and globally
// unique across the library: a Code does not need a stage prefix to be
// disambiguated. Codes are stable within a major version; renaming or
// removing requires a major bump. Adding new codes is a minor.
//
// While the library is pre-1.0 the catalog is marked experimental and
// may churn freely (see §7.1).
type Code string

// Error is the library's structured failure type. Every package
// that today returns an unstructured error wraps it in *Error before
// returning across a public boundary.
//
// Callers should use errors.As to extract structured fields:
//
//   var dcErr *devcontainer.Error
//   if errors.As(err, &dcErr) {
//       switch dcErr.Code {
//       case devcontainer.CodeImageRegistryDenied:
//           ...
//       }
//   }
//
// Subprocess stderr (when applicable) is exposed via the Cause chain,
// not via Error fields. See StderrCarrier below.
type Error struct {
    // Code identifies the failure mode. Required.
    Code Code

    // Reason is a short human-readable explanation. The library does
    // not promise stability; downstream consumers should not rely on
    // exact wording. For user-facing copy, map Code to a string of
    // your own.
    Reason string

    // Cause is the wrapped underlying error (a Docker SDK error,
    // os/exec.ExitError, JSON unmarshal error, a runtime typed
    // error, etc.). May be nil for failures the library detects on
    // its own.
    Cause error

    // Context carries structured detail useful for diagnosis. Keys
    // and types are per-Code; see error_codes.go doc comments.
    Context map[string]any
}

func (e *Error) Error() string { /* "<code>: <reason>" */ }
func (e *Error) Unwrap() error { return e.Cause }

// StderrCarrier is implemented by error types that captured the
// stderr of a failed subprocess. Callers retrieve it from anywhere in
// the Cause chain:
//
//   var sc devcontainer.StderrCarrier
//   if errors.As(err, &sc) {
//       tail := sc.Stderr()
//   }
//
// The library does not bound or truncate Stderr() output; callers
// shipping the value over size-limited transports (AMQP, logs)
// truncate at their boundary.
type StderrCarrier interface {
    Stderr() string
}
```

### 3.1 Construction

A small exported helper keeps call sites uncluttered. It is exported
because call sites live in sub-packages (`runtime/`, `compose/`,
`feature/`, `config/`) as well as the root.

```go
// devcontainer/error.go

func New(code Code, cause error, reason string) *Error {
    return &Error{Code: code, Cause: cause, Reason: reason}
}

// Builder for Context entries:
func (e *Error) With(key string, val any) *Error {
    if e.Context == nil {
        e.Context = map[string]any{}
    }
    e.Context[key] = val
    return e
}
```

A typical call site becomes:

```go
if err := rt.PullImage(ctx, ref); err != nil {
    return "", devcontainer.New(codeForPullError(err), err,
        "pull base image").
        With("ref", ref)
}
```

The `codeForPullError(err)` helper centralises the "translate a raw
runtime error into our Code" decision in one place — see §3.3.

### 3.2 Code catalog

Codes are flat and globally unique. The implicit grouping by
lifecycle phase is just a convention for readability. New codes are
added when a known caller needs to branch on a distinction; until then
the discriminator lives in `Context` or `Cause`.

```go
// devcontainer/error_codes.go (new file)

// Config resolve
const (
    CodeConfigParseFailed      Code = "config_parse_failed"
    CodeConfigInvalid          Code = "config_invalid"
    CodeConfigUnsupportedField Code = "config_unsupported_field"
)

// Feature fetch
const (
    CodeFeatureNotFound        Code = "feature_not_found"
    CodeFeatureFetchDenied     Code = "feature_fetch_denied"
    CodeFeatureFetchFailed     Code = "feature_fetch_failed"
    CodeFeatureMetadataInvalid Code = "feature_metadata_invalid"
)

// Image pull
const (
    CodeImageNotFound       Code = "image_not_found"
    CodeImageRegistryDenied Code = "image_registry_denied"
    CodeImagePullFailed     Code = "image_pull_failed"
)

// Image build (source=image, dockerfile, feature-extended, uid_reconcile)
const (
    CodeImageBuildFailed         Code = "image_build_failed"
    CodeImageBuildContextInvalid Code = "image_build_context_invalid"
)

// UID reconcile
const (
    CodeUIDReconcileBuildFailed    Code = "uid_reconcile_build_failed"
    CodeUIDReconcileUserUnresolved Code = "uid_reconcile_user_unresolved"
)

// Compose up
const (
    CodeComposeLoadFailed       Code = "compose_load_failed"
    CodeComposeServiceUnhealthy Code = "compose_service_unhealthy"
    CodeComposeServiceExited    Code = "compose_service_exited"
)

// Container create
const (
    CodeContainerCreateFailed Code = "container_create_failed"
    CodeContainerStartFailed  Code = "container_start_failed"
)

// Lifecycle scripts
const (
    CodeLifecycleScriptExited   Code = "lifecycle_script_exited"
    CodeLifecycleScriptNotFound Code = "lifecycle_script_not_found"
)

// Down lifecycle (allocated up-front to keep the catalog stable;
// call sites migrate in a follow-up PR).
const (
    CodeContainerStopFailed   Code = "container_stop_failed"
    CodeContainerRemoveFailed Code = "container_remove_failed"
    CodeComposeDownFailed     Code = "compose_down_failed"
    CodeVolumeRemoveFailed    Code = "volume_remove_failed"
)
```

Per-code doc comments in `error_codes.go` document the conventional
`Context` keys for each Code (e.g. `CodeImagePullFailed` → `ref`
string, optional `registry` string).

#### Splitting policy

A Code is split into two only when **two known callers need to branch
on the distinction**. Until then, the discriminator stays in
`Context` (or is read from `Cause` via `errors.As`). This prevents
premature granularity and code churn driven by speculation.

### 3.3 Per-stage classifier helpers

Each lifecycle phase that translates from external errors (runtime
typed errors, exec failures, HTTP status codes) gets a small
classifier. Classifiers live in the **root `devcontainer` package**,
not in `runtime/` — `runtime/` cannot import `devcontainer` without a
cycle, and the mapping is library policy.

```go
// devcontainer/classify_pull.go
func codeForPullError(err error) Code {
    var nf *runtime.ImageNotFoundError
    if errors.As(err, &nf) {
        return CodeImageNotFound
    }
    if isRegistryAuth(err) { // registry-specific check
        return CodeImageRegistryDenied
    }
    return CodeImagePullFailed
}
```

When a new runtime backend lands (e.g. `runtime/podman/`), it defines
its own typed errors and the classifier learns to `errors.As` them.
Codes stay runtime-agnostic.

---

## 4. Subprocess stderr capture

For shell-out failures (Dockerfile RUN inside buildkit, feature
install scripts, post-create/post-start, compose service starts), the
raw exit code on its own is not actionable — the *why* is in stderr.

### 4.1 Capture lives on runtime typed errors

The existing `runtime.ExecFailedError{Stderr}` and
`runtime.ComposeFailedError{Stderr}` already capture stderr at the
point of subprocess invocation. Other subprocess paths (feature
install, lifecycle scripts, useruid build) should adopt the same
pattern: a typed error in their package with a `Stderr` field
populated by the capture.

Each of these typed errors implements `StderrCarrier`:

```go
func (e *ExecFailedError) Stderr() string    { return e.stderr }
func (e *ComposeFailedError) Stderr() string { return e.stderr }
```

### 4.2 No duplication on `*Error`

The wrapper `*Error` carries **no stderr field**. Callers retrieve
stderr via the Cause chain:

```go
var sc devcontainer.StderrCarrier
if errors.As(err, &sc) {
    tail := sc.Stderr()
}
```

Single source of truth. No nullable `Context["lastStderr"]`. No
risk of the wrapper and Cause disagreeing.

### 4.3 Truncation is the caller's job

The library does not bound `Stderr()`. The runtime-level capture
chooses some practical cap (driven by memory, not by the error
contract — current implementations stream-and-keep), but the contract
is "the stderr we captured." Callers shipping over AMQP / logging
sinks truncate at their boundary.

### 4.4 Stage coverage

Add typed errors carrying `Stderr` in:

- `feature/` — feature install / Dockerfile build path.
- `useruid.go` — build invocation.
- `lifecycle.go` — `postCreate`/`postStart`/`postAttach` runners.
- Any direct `cmd.Run()` in `host_executor.go` / runtime `exec.go`.

Compose (`runtime/docker/compose.go`) and exec already do this.

---

## 5. Catalog ownership (library vs. downstream)

The library owns codes for failures it can detect — anything tied to
the spec (image pull, feature install, uid reconcile, compose service
crashloop, script exit code).

Downstream consumers (e.g. a workspace platform built on this library)
maintain their own catalog of codes for failures invisible to the
library — security plugin denials, egress proxy unreachability, PVC
bind delays, init-container failures.

The user-facing hint catalog is the **union** of both:

```
┌────────────────────────────────────────────────┐
│        downstream hint catalog                 │
│                                                │
│  hint_code  │  source_code     │  suggested_fix│
│  ───────────┼──────────────────┼───────────────│
│  HINT_001   │  CodeImagePullFa │  "Verify…"    │  ← library code
│  HINT_002   │  CodeUIDReconc…  │  "Move large…"│  ← library code
│  HINT_050   │  AUTHZ_REGISTRY  │  "Check…"     │  ← consumer code
│  HINT_051   │  EDGED_CREDS_EXP │  "Rotate…"    │  ← consumer code
└────────────────────────────────────────────────┘
```

The library does not export suggested-fix strings, documentation
links, or severity levels. Those are display concerns the consumer
owns. The library's responsibility ends at "here is what failed and
how."

---

## 6. Event bus (optional follow-up)

Out of scope for the initial implementation, but worth considering as
a v2:

A `FailedEvent` on the existing bus would let consumers observe
failures live (mid-Up) rather than only on the error return at the
end. Today the only failure signal on the bus is `ConfigWarningEvent`
(non-fatal) and the absence of a `*CompletedEvent` (inferred from
context).

Shape:

```go
type FailedEvent struct {
    Base
    Code    Code
    Reason  string
    Context map[string]any
}
```

Emit immediately before the error return at any structured-error site.
Decoupled from the return value: a consumer subscribed to the bus sees
failure codes as they happen; a caller using only the return value
gets the same structured information at the boundary.

Rationale for deferring: most callers will read the return error
anyway, so the bus emission is redundant for them. Worth adding when a
consumer concretely needs mid-flight failure signals.

---

## 7. Migration plan

The change is mechanically broad (~340 call sites) but each site is
local and reviewable. Recommended order:

1. **Land the types.** Add `Error`, `Code`, the constants (including
   Down codes), `StderrCarrier`, `New`, `With`. Add `Stderr()` methods
   to existing `runtime/errors.go` typed errors. No call site changes
   yet. This commit is purely additive; no caller is affected.

2. **Convert one package as a reference implementation.**
   `useruid.go` first — small, clear failure modes, existing test
   coverage (`useruid_test.go`, `useruid_uid_test.go`) that can extend
   to assert `Error.Code` values.

3. **Convert remaining packages, one PR per phase.** Each PR converts
   the call sites for one logical phase (image pull, image build,
   feature fetch, compose, lifecycle, ...) and adds tests that inject
   failures and assert `Error.Code`.

4. **Add stderr capture to remaining shell-outs.** Where a phase's
   subprocess path doesn't yet expose a typed error with `Stderr`,
   add one. Separate from the type conversion to keep reviews tight.

5. **Optionally: emit `FailedEvent`.** Only if §6 picks up a concrete
   consumer.

### 7.1 Backwards compatibility & stability

Existing callers using `err.Error()` continue to work — `*Error`
satisfies the `error` interface, and `Error()` produces
`"<code>: <reason>"`. Callers wrapping our errors with
`fmt.Errorf("%w", err)` continue to work via `Unwrap`. New callers
that want structure use `errors.As`.

**The Code catalog is experimental while the library is pre-1.0.**
Codes may be renamed or removed without a major bump until the
catalog has been exercised by real consumers. Once stabilised
(targeted at v1.0), the rules in §3 apply: renames/removals require a
major bump, additions are minor.

---

## 8. Testing

Two patterns cover this well:

### 8.1 Per-classifier unit tests

Each `codeFor*Error` helper is pure (Go error in, Code out). Cover the
known external-error shapes:

```go
func TestCodeForPullError(t *testing.T) {
    cases := map[string]struct {
        err  error
        want Code
    }{
        "not found": {&runtime.ImageNotFoundError{Ref: "x"}, CodeImageNotFound},
        "denied":    {newAuthError(), CodeImageRegistryDenied},
        "other":     {errors.New("boom"), CodeImagePullFailed},
    }
    ...
}
```

### 8.2 Integration-level error assertions

Existing `*_test.go` files that exercise Up/Down with synthetic
runtimes get one more assertion: when the test injects a failure, the
returned error unwraps to `*Error` with the expected Code:

```go
var dcErr *devcontainer.Error
if !errors.As(err, &dcErr) {
    t.Fatalf("expected *Error, got %T", err)
}
if dcErr.Code != devcontainer.CodeUIDReconcileBuildFailed {
    t.Errorf("got code %q", dcErr.Code)
}
```

For stderr assertions:

```go
var sc devcontainer.StderrCarrier
if errors.As(err, &sc) {
    if !strings.Contains(sc.Stderr(), "expected text") { ... }
}
```

No new mocking infrastructure — existing test runtimes already
exercise the failure paths.

---

## 9. Decisions

1. **`Code` is a `string` type** (not iota). JSON-friendly, survives
   over the wire, matches the existing `BuildSource` pattern.

2. **Flat codes, no Stage field.** A Code is globally unique; the
   lifecycle-phase grouping is a documentation convention. Splitting
   policy: split a Code only when two known callers need to branch on
   the distinction.

3. **`Context` is `map[string]any`** with documented conventions per
   Code in `error_codes.go`. Typed structs not worth the surface area
   given how small `Context` is once stderr lives on Cause.

4. **Stderr lives on the Cause chain only.** `StderrCarrier`
   interface; no `Context["lastStderr"]`; library does not truncate.

5. **Classifiers live in the root `devcontainer` package**
   (`classify_*.go`), not in `runtime/`. `runtime/` cannot import
   `devcontainer` without a cycle, and the mapping is library policy.

6. **Files:** `error.go` (type + constructor + `StderrCarrier`),
   `error_codes.go` (all Code constants with doc-commented Context
   keys), `classify_*.go` (one per logical phase).

7. **Existing typed errors stay.** `config/errors.go` and
   `runtime/errors.go` types continue to exist as Cause carriers.
   Runtime typed errors gain `Stderr()` methods to satisfy
   `StderrCarrier`. They are not replaced by the wrapper.

8. **Catalog stability:** experimental until v1.0; stable thereafter.

9. **`WarnEvent` / `ConfigWarningEvent` unchanged.** Separate concern;
   revisit if a consumer asks to bucket warnings by code.

---

## 10. Summary

- `devcontainer.Error{Code, Reason, Cause, Context}` becomes the
  return type for every failure path. Existing typed errors in
  `config/errors.go` and `runtime/errors.go` stay as `Cause` carriers.
- `Code` is a flat, `string`-typed exported enum. No Stage field. The
  library owns generic devcontainer codes; downstream consumers layer
  their own.
- Subprocess stderr is exposed via a `StderrCarrier` interface on the
  Cause chain. No duplication on the wrapper, no truncation in the
  library.
- Classifiers live in the root `devcontainer` package, one file per
  logical phase.
- Down codes are allocated up-front; call-site migration is a
  follow-up PR.
- `FailedEvent` on the bus is deferred until a concrete consumer
  needs mid-flight signals.
- Migration is per-phase, additive; backwards-compatible at the
  `error` interface. Catalog is experimental pre-1.0.
