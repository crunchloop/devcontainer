# Design — `ResolvedConfig`

**Status:** Draft for review
**Date:** 2026-05-06
**Scope:** the central data type produced by `devcontainer.Resolve` and consumed
by every downstream package (`runtime`, `feature`, `lifecycle`, `compose`).
Getting this right is the single most important early decision.

---

## 1. Lifecycle of a config

Four conceptual stages:

```text
┌─────────┐  parse   ┌─────────┐  merge   ┌─────────┐  substitute  ┌──────────┐
│  files  │ ───────▶ │   raw   │ ───────▶ │ merged  │ ───────────▶ │ resolved │
└─────────┘          └─────────┘          └─────────┘              └──────────┘
  *.json,             one Go               raw +                    host vars
  JSONC               struct per           image label              filled in;
                      file                 metadata +               container vars
                                           feature                  may remain
                                           metadata
```

After the container is created, the runtime re-substitutes `${containerEnv:*}`
and `${containerWorkspaceFolder}` against actual container state. That is **not**
a separate type — it happens internally when callers invoke `Exec`,
`RunLifecycle`, or read live values via accessors.

**Public API exposes only `ResolvedConfig`.** `RawConfig` and `MergedConfig`
exist in the `config` package as internal types for the parse/merge pipeline,
unexported. Callers that want raw access get the JSON file path back via
diagnostics and read it themselves.

## 2. Top-level shape

```go
package devcontainer

type ResolvedConfig struct {
    // Stable identifier derived from (LocalWorkspaceFolder, configPath).
    // Consumers use this for container naming, label values, etc.
    DevcontainerID string

    // Where the container comes from. Exactly one of Image/Build/Compose
    // is non-nil. See §3.
    Source Source

    // Identity & workspace
    Name              string
    LocalWorkspaceFolder    string  // host-side absolute path
    ContainerWorkspaceFolder string // resolved container path
    WorkspaceMount    *Mount  // nil => derived from LocalWorkspaceFolder

    // Users
    ContainerUser       string
    RemoteUser          string
    UpdateRemoteUserUID bool
    UserEnvProbe        UserEnvProbe // none | loginShell | interactiveShell | loginInteractiveShell

    // Environment
    ContainerEnv map[string]string
    RemoteEnv    map[string]string

    // Container creation
    Mounts          []Mount
    RunArgs         []string
    Init            bool
    Privileged      bool
    CapAdd          []string
    SecurityOpt     []string
    OverrideCommand bool
    ShutdownAction  ShutdownAction // none | stop | stopContainer | stopCompose

    // Features (already resolved & ordered by install order; see §5)
    Features []ResolvedFeature

    // Lifecycle (see §4)
    Lifecycle LifecycleCommands
    WaitFor   LifecyclePhase // which phase blocks "ready"; default postCreate

    // Ports (informational; not actuated by this library)
    ForwardPorts          []PortSpec
    PortsAttributes       map[string]PortAttributes
    OtherPortsAttributes  *PortAttributes

    // Host requirements (informational)
    HostRequirements *HostRequirements

    // Tool-namespaced pass-through. Caller decodes their own namespace.
    // e.g. `json.Unmarshal(cfg.Customizations["dap"], &dapCustom)`.
    Customizations map[string]json.RawMessage

    // Non-fatal warnings emitted during parse/merge/substitute.
    Warnings []Warning
}
```

Documented as **read-only after `Resolve` returns**. Not enforced (Go has no
ergonomic immutability); a `defensiveCopy()` helper is provided for callers
that want to mutate.

## 3. `Source` — image | build | compose

Sealed interface (unexported method prevents external impls):

```go
type Source interface {
    isSource()
    Kind() SourceKind
}

type SourceKind string
const (
    SourceImage   SourceKind = "image"
    SourceBuild   SourceKind = "build"
    SourceCompose SourceKind = "compose"
)

type ImageSource struct {
    Image string
}

type BuildSource struct {
    Dockerfile string             // path relative to context
    Context    string             // absolute, host-side
    Args       map[string]string
    Target     string
    CacheFrom  []string
    Options    map[string]string  // build options pass-through
}

type ComposeSource struct {
    Files       []string  // absolute paths to compose files
    Service     string    // primary service
    RunServices []string  // services to start (default: all)
}
```

Why interface over `Kind`+pointer-fields struct:
- Type switches read more naturally: `switch s := cfg.Source.(type) { case *ImageSource: ... }`.
- Zero risk of "Kind says image but Image is nil" inconsistency.
- Sealed via unexported method = no third-party `Source` impls leak in.

## 4. Lifecycle commands

The spec accepts three forms per phase: a single string, a string array, or a
map of named commands run in parallel. Each command can itself be a string
(shell-form) or array (exec-form).

```go
type LifecycleCommands struct {
    Initialize    LifecycleCommand // host-side; opt-in via EngineOptions
    OnCreate      LifecycleCommand
    UpdateContent LifecycleCommand
    PostCreate    LifecycleCommand
    PostStart     LifecycleCommand
    PostAttach    LifecycleCommand
}

type LifecycleCommand struct {
    // At most one of Single / Parallel is populated.
    Single   *Command
    Parallel map[string]Command // ordered iteration via sorted keys
}

type Command struct {
    // Exactly one of Shell / Exec is populated.
    Shell string   // run as `sh -c "<shell>"`
    Exec  []string // run via exec without a shell
}
```

We **preserve the spec distinction** between shell and exec form. Callers and
the runtime need it: shell-form gets quoting; exec-form doesn't and is safer
for argument injection. Normalizing both to "shell" was tempting but loses
information.

`Parallel` deliberately uses `map[string]Command`. Iteration order is sorted by
key for deterministic logging; concrete execution is concurrent.

A nil `*Command` / nil-map means the phase is unconfigured (skip).

## 5. Features

```go
type ResolvedFeature struct {
    // The reference as written in devcontainer.json,
    // e.g. "ghcr.io/devcontainers/features/node:1".
    Ref string

    // Pinned reference after registry resolution.
    // For OCI: "ghcr.io/devcontainers/features/node@sha256:<digest>".
    // For HTTPS: original URL (digest not meaningful).
    // For Local: absolute path.
    ResolvedRef string

    // Parsed devcontainer-feature.json.
    Metadata FeatureMetadata

    // Caller-supplied options merged with feature defaults.
    Options map[string]any

    SourceKind FeatureSourceKind // OCI | HTTPS | Local

    // True if this feature was found in the base image's
    // `devcontainer.metadata` label and does NOT need re-installation.
    AlreadyInstalled bool
}
```

`Features` slice is **ordered by install order** — the topo-sorted result of
`dependsOn` / `installsAfter` / `overrideFeatureInstallOrder`. Even when
`AlreadyInstalled` is true, the entry stays in the slice so consumers can
introspect what's present.

Cycle detection happens during resolution; cycles return a typed
`*FeatureCycleError` from `Resolve`, not a warning.

## 6. Mounts

Spec allows two encodings: a CSV string (`"type=bind,source=...,target=..."`)
or an object. We **parse both, normalize to one struct**:

```go
type Mount struct {
    Type     MountType // bind | volume | tmpfs
    Source   string    // absolute path (bind) or volume name (volume); empty for tmpfs
    Target   string
    ReadOnly bool

    BindOptions   *BindOptions   // populated only when Type == bind
    VolumeOptions *VolumeOptions
    TmpfsOptions  *TmpfsOptions
}
```

Round-tripping (object → CSV) lives in the runtime layer when calling Docker,
not on `Mount` itself.

## 7. Diagnostics

```go
type Warning struct {
    Code    WarningCode // e.g. "unknown_field", "deprecated_key", "unsupported_feature_field"
    Message string
    Path    string      // JSON pointer into the source devcontainer.json
    Source  string      // file path or "image:<ref>" or "feature:<id>"
}
```

Non-fatal only. Things that prevent producing a `ResolvedConfig` are typed
errors:

- `*ConfigParseError` — JSON syntax / schema violation
- `*ConfigMergeError` — incompatible merges (rare; mostly internal-bug)
- `*FeatureResolutionError` — pull / parse / cycle

Substitution failures are **not** errors. Undefined `${localEnv:X}` with no
default substitutes to empty string and emits `WarnUnresolvedLocalEnv`,
matching VS Code / devpod / `@devcontainers/cli` behavior (shell-style env
semantics; ref: VS Code issue #46436). Callers wanting strict mode can
promote warnings to errors after `Resolve` returns.

Unknown variables (`${notARealVar}`) are left as literal placeholders and
emit `WarnUnknownVariable`.

`Warnings` accumulate across all stages and are returned even on success.

## 8. Substitution semantics

`Resolve` runs **only the host-context pass**:

| Variable                                    | Resolved by `Resolve`? |
| ------------------------------------------- | ---------------------- |
| `${localWorkspaceFolder}`                   | yes                    |
| `${localWorkspaceFolderBasename}`           | yes                    |
| `${localEnv:VAR[:default]}`                 | yes                    |
| `${devcontainerId}`                         | yes                    |
| `${containerWorkspaceFolder}`               | partial (set after merge fixes the path; see below) |
| `${containerWorkspaceFolderBasename}`       | partial                |
| `${containerEnv:VAR[:default]}`             | **no** — left as-is    |

`${containerWorkspaceFolder}` is technically derivable pre-creation
(`/workspaces/<basename>` by default, overridable via `workspaceFolder`), so
`Resolve` fills it in. `${containerEnv:*}` requires the live container, so it
stays as a literal placeholder until the runtime executes commands.

Helper for callers (and internal use):

```go
// HasPendingSubstitutions returns true if any string field still contains
// unresolved ${containerEnv:*} placeholders.
func (c *ResolvedConfig) HasPendingSubstitutions() bool
```

The runtime layer holds a `Substituter` bound to a live container that
re-walks specific subtrees (lifecycle commands, exec env) on demand. We never
mutate the `ResolvedConfig`.

## 9. `devcontainerId`

Stable hash of `(LocalWorkspaceFolder, configPath)`:

```text
devcontainerId = base32(sha256(localWorkspaceFolder + "\x00" + configPath))[:16]
```

Lowercase, hyphen-friendly. 16 chars (80 bits) — collision-resistant for any
practical workspace count, short enough for container/network naming.

Overridable via `EngineOptions.DevcontainerIDOverride func(LocalPath, ConfigPath) string`
for cases like ephemeral CI runs that need predictable names.

## 10. JSON / YAML mapping

`devcontainer.json` field names use camelCase; Go fields use PascalCase. JSON
tags are added on the **internal** `RawConfig`. The public `ResolvedConfig`
has no JSON tags — it's not a wire format and we don't promise round-trip
serialization. Marshalling a `ResolvedConfig` to JSON for debugging works via
default Go field names but is for humans only.

## 11. Decisions

Resolved during design review (2026-05-06):

1. **Env: `map[string]string`.** No insertion-order preservation. Spec models
   env as a JSON object (set semantics). A `SortedKeys` helper is provided for
   callers that want deterministic iteration in logs.

2. **No raw-config exposure.** `ResolvedConfig` is the contract. Tool-specific
   data flows through `Customizations map[string]json.RawMessage`. Source file
   paths surface via `Warning.Source` for diagnostics. New top-level fields are
   added to `ResolvedConfig` as typed fields when needs arise.

3. **`WaitFor`: spec-correct default.** When `devcontainer.json` omits
   `waitFor`, `Resolve` defaults to `updateContent` if any resolved feature or
   the merged `updateContentCommand` does work, otherwise `postCreate`. Explicit
   user values are honored as-set.

4. **`HostRequirements`: surface only in v1.** Parsed into a typed struct;
   no validation, no enforcement. Future opt-in
   `engine.CheckHostRequirements(ctx, cfg) ([]Warning, error)` if callers ask.

5. **Compose project name: `dc-<devcontainerId>` by default.** Stable across
   runs, charset-compliant, collision-resistant. Overridable via
   `EngineOptions.ComposeProjectName func(*ResolvedConfig) string`; empty
   return falls back to the default.

## 12. What goes in M1

- `config/raw.go` — internal `RawConfig` + JSONC parsing.
- `config/merge.go` — internal `MergedConfig` + spec-correct merge of raw +
  image-label-metadata + feature-metadata. (Image label and feature metadata
  paths are stubbed in M1; real impls land in M3.)
- `config/substitute.go` — host-context substitution pass.
- `config/resolved.go` — public `ResolvedConfig` + supporting types.
- `devcontainer.go` — `Resolve(ctx, ResolveOptions) (*ResolvedConfig, error)`.
- Golden-file unit tests under `config/testdata/` covering every spec field
  with both valid and warning-producing inputs.

What's **not** in M1:
- Real OCI feature resolution (mocked `FeatureStore` produces metadata from
  fixtures).
- Real image-label reading (test fixtures embed pre-computed merged metadata).
- Compose file parsing (compose-spec/compose-go integration is M4).
