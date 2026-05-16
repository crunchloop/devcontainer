# Design — Features layer

**Status:** Draft for review
**Date:** 2026-05-06
**Scope:** the feature pipeline added in M3 — reference resolution
(OCI/HTTPS/Local), the `FeatureStore` interface, option processing, DAG
ordering, generated feature-dockerfile structure, the
`devcontainer.metadata` image label (write + read + pre-baked skip),
and the on-disk cache.

Companion to `design/resolved-config.md` (where `ResolvedFeature` is
typed) and `design/runtime.md` (where `BuildImage` lives). This doc owns
the spec-rich middle layer between them.

---

## 1. Pipeline overview

```
devcontainer.json (features map)
        │
        ▼
┌─────────────────────────────────────────┐
│ feature.Resolve(refs, options)          │
│   - process each ref → fetch → unpack   │
│   - parse devcontainer-feature.json     │
│   - apply user options + defaults       │
│   - return []ResolvedFeature            │
└─────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────┐
│ feature.Order(features, override)       │
│   - DAG: dependsOn + installsAfter      │
│   - topo sort                           │
│   - cycle detection                     │
│   - applies overrideFeatureInstallOrder │
└─────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────┐
│ feature.GenerateDockerfile(             │
│     baseImage, orderedFeatures)         │
│   - emit feature dockerfile + context   │
│   - include devcontainer.metadata LABEL │
│   - returns BuildSpec for runtime       │
└─────────────────────────────────────────┘
        │
        ▼
runtime.BuildImage  (Docker / future buildkit)
        │
        ▼
ResolvedConfig is then "feature-aware":
  - Features[i].AlreadyInstalled = true if base image label
    declared the feature was already baked in
  - the engine skips OCI fetch + dockerfile-gen entirely for
    those features (the pre-baked-image hot path)
```

## 2. Reference resolution

Three reference types, derived from the feature key in `devcontainer.json`:

| Form                                           | Kind                | Cache key             |
| ---------------------------------------------- | ------------------- | --------------------- |
| `ghcr.io/devcontainers/features/node:1`        | `FeatureSourceOCI`   | resolved digest        |
| `https://example.com/feature.tgz`              | `FeatureSourceHTTPS` | sha256 of body         |
| `./local-features/myfeature` or `../path`       | `FeatureSourceLocal` | absolute path (no copy) |

Resolution returns an extracted directory containing the feature's
`devcontainer-feature.json` and `install.sh`. Local refs return their
own path; OCI/HTTPS extract into the cache (see §8).

**OCI mechanics:** parse ref via `go-containerregistry/pkg/name`, fetch
via `remote.Image` with default keychain (`auth.NewMultiKeychain` over
ambient docker creds), require manifest config media type
`application/vnd.devcontainers`, download the first layer, extract.
Resolved digest is recorded on `ResolvedFeature.ResolvedRef` so an image
rebuild months later uses the exact same bytes even if the tag has moved.

**HTTPS mechanics:** GET the URL, validate filename matches
`^devcontainer-feature-[A-Za-z0-9_-]+\.tgz$`, stream body to disk, extract.
Cache key is the sha256 of the response body so the same content at two
URLs dedupes. Custom headers (auth) supplied by callers via
`EngineOptions.FeatureDownloadHeaders map[string]string`.

**Local mechanics:** `filepath.Abs(filepath.Join(configDir, ref))`. No
caching (the directory is the cache).

## 3. `FeatureStore` interface

The pluggable boundary between resolution and dockerfile generation.

```go
package feature

type Store interface {
    // Resolve fetches one feature reference and returns the path to a
    // directory containing devcontainer-feature.json + install.sh.
    // Pulled artifacts are cached; repeat calls for the same ref are
    // serviced from disk.
    Resolve(ctx context.Context, ref string) (*Resolved, error)
}

type Resolved struct {
    Ref         string            // as written in devcontainer.json
    ResolvedRef string            // pinned (digest for OCI, sha256 for HTTPS, abs path for Local)
    Dir         string            // extracted directory on disk
    Metadata    config.FeatureMetadata
    SourceKind  config.FeatureSourceKind
}
```

Default impl: `feature.NewDiskStore(dir, headers)`. In-memory impl for
tests: `feature.NewMemoryStore(map[string]Resolved)` so unit tests don't
hit a registry.

## 4. Option resolution

Per-feature options merged in three layers: feature defaults
(from `devcontainer-feature.json`) → user values (from
`devcontainer.json`'s features map) → final.

User value forms (per spec):
- `"feature": {}` → all defaults
- `"feature": { "version": "1.2.3" }` → object merge
- `"feature": "1.2.3"` → shorthand: applied as the `version` option if the feature declares it

After merge, options are emitted to a per-feature `devcontainer-features.env`
file as `UPPERCASED_KEY="value"`, sorted lexicographically. Key transform:
- non-word chars → `_`
- leading digit → prefix with `_`
- uppercase

We **validate enum/proposals at parse time** (devpod defers this to
`install.sh`). Rejecting typos before image build is worth the small
duplication.

## 5. DAG ordering

Ordered `[]ResolvedFeature` per spec rules:

1. Features named in `overrideFeatureInstallOrder` first, in declaration
   order. (Hard override; not topo-sorted.)
2. Remaining features: topo-sorted via leaf-extraction over a graph built
   from `installsAfter` and `dependsOn`.

`dependsOn` is recursive (transitively pulls in unlisted features);
`installsAfter` is a soft "if both listed, this one waits" hint.

Cycle detection at edge insertion: DFS for back-edge before adding.
Returns a typed `*FeatureCycleError{Path []string}` so callers can show
the cycle to users.

Implementation: a small `graph` package internal to `feature` (~150 LOC).
We do **not** vendor or copy devpod's `pkg/devcontainer/graph` — same
algorithm, our own implementation.

## 6. Generated feature dockerfile

The dockerfile prepended to the base image when features are present:

```dockerfile
# syntax=docker/dockerfile:1.4
ARG _DEV_CONTAINERS_BASE_IMAGE=placeholder
FROM $_DEV_CONTAINERS_BASE_IMAGE AS devcontainer_target

USER root

COPY ./build-context/ /tmp/dc-features/
RUN chmod -R 0755 /tmp/dc-features

# --- per-feature layers (in install order) ---
RUN \
  echo "_CONTAINER_USER_HOME=$(getent passwd root | cut -d: -f6)" \
    >> /tmp/dc-features/builtin.env && \
  echo "_REMOTE_USER_HOME=$(getent passwd $(whoami) | cut -d: -f6)" \
    >> /tmp/dc-features/builtin.env

# Feature 0: ghcr.io/devcontainers/features/node:1
ENV NODE_VERSION="lts" NVM_VERSION="0.39.5"
RUN cd /tmp/dc-features/0 \
  && chmod +x ./run.sh && ./run.sh

# Feature 1: ghcr.io/devcontainers/features/git:1
RUN cd /tmp/dc-features/1 \
  && chmod +x ./run.sh && ./run.sh

# --- accumulated metadata ---
LABEL devcontainer.metadata='[<merged JSON, see §7>]'

ARG _DEV_CONTAINERS_IMAGE_USER=root
USER $_DEV_CONTAINERS_IMAGE_USER
```

`run.sh` is a small wrapper we generate per feature:

```bash
#!/bin/sh
set -e
trap 'rc=$?; [ $rc -ne 0 ] && echo "ERROR: feature \"FEATURE_REF\" failed: exit $rc" >&2' EXIT
set -a
. ../builtin.env
. ./feature.env
set +a
chmod +x ./install.sh
./install.sh
```

Build context layout (passed as `BuildSpec.ContextPath`):

```
<engine-managed tmp dir>/
  Dockerfile                # the file above
  build-context/
    builtin.env             # _CONTAINER_USER_HOME etc. (filled at RUN time)
    0/                      # first feature in install order
      run.sh                # generated wrapper
      feature.env           # UPPERCASED_KEY="value" lines
      install.sh            # from the feature's tarball
      ...                   # everything else from the feature dir
    1/
      ...
```

`run.sh` (vs devpod's `devcontainer-features-install.sh`) and
`feature.env` (vs `devcontainer-features.env`) are intentionally shorter
filenames — easier to type when debugging from an `exec` shell.

## 7. The `devcontainer.metadata` label

The cornerstone of incremental rebuilds and the **pre-baked-image hot
path** that downstream consumers exercise.

Format: a JSON array on the image. Each element is a per-source layer of
config — base image's prior label (carried through), one entry per
feature, and the resolved devcontainer.json itself. Order matches build
order; merge semantics are "later overrides earlier" for scalars,
"concat + dedupe" for lists.

```json
[
  { "remoteUser": "node",
    "containerEnv": { "PATH": "/opt/node/bin:..." } },
  { "id": "ghcr.io/devcontainers/features/node",
    "version": "1.5.0",
    "containerEnv": { "NODE_OPTIONS": "..." },
    "mounts": [...] },
  { "id": "ghcr.io/devcontainers/features/git", "version": "1.2.0" },
  { "remoteUser": "node",
    "customizations": { "dap": {...} } }
]
```

**Write path:** assembled during dockerfile generation, marshalled,
emitted as the final `LABEL devcontainer.metadata=...` instruction.

**Read path:** when `Engine.Up` starts, we Inspect the base image (the
one named in `devcontainer.json`'s `image` field, or the result of
build) and parse its `devcontainer.metadata` label. For each feature in
the user's request, we check the array for a matching `id` + version-
compatible entry; if present, mark `ResolvedFeature.AlreadyInstalled =
true` and skip OCI fetch + dockerfile-gen entirely.

This is the observation that makes the consumer hot path cheap: a base image
is pre-built with all features installed; every workspace start is a
label read + run, with **zero feature work**.

**Version compatibility:** match by id, then require the cached version
to be `>=` the requested. (Spec is silent on this; we adopt
permissive-newer-wins as the friendliest default. Caller can opt into
strict pinning via `EngineOptions.StrictFeatureVersionMatch: true`.)

**Divergence from devpod:** we record the **resolved digest** on the
metadata entry, not just the version string. Lets `StrictFeatureVersionMatch`
do byte-level identity comparison without needing the registry to be
online. Devpod stores `Version` only; downstream tooling can't tell if
two builds tagged `:1` actually have the same bytes.

## 8. On-disk cache layout

`os.UserCacheDir()/devcontainer-go/features/` (overridable via
`EngineOptions.FeatureCacheDir`):

```
features/
  oci/
    sha256-<digest>/
      manifest.json         # OCI manifest (for verification on cache hit)
      blob.tgz              # the artifact
      extracted/
        devcontainer-feature.json
        install.sh
        ...
  https/
    sha256-<contenthash>/
      blob.tgz
      extracted/...
  index.json                # ref → digest map with TTL hints
```

Cache key is **content-addressed**: OCI digest or HTTPS body sha256. A
mutable tag (`:1`) resolves through `index.json` to the digest, then
hits the disk cache. This is intentionally different from devpod's
"hash-the-ref-string" scheme — content-addressed dedupes across mirror
URLs and prevents stale-ref bugs.

Concurrency: a small lockfile (`extracted/.lock`) per cache entry so
parallel `Resolve` calls for the same ref don't race.

GC: out of scope for v1. Document `rm -rf $cache` as the recovery path.

## 9. M3 ship target

Suggested PR breakdown:

- **PR6 — feature.Store + Local + DAG.** Interface, in-memory store,
  on-disk cache shell (no OCI yet), local-path resolver, DAG with cycle
  detection. Unit tests on synthetic features (no network).
- **PR7 — OCI + HTTPS resolvers.** Pull `go-containerregistry`. Real
  feature fetch, real tarball extract. Integration test pulls
  `ghcr.io/devcontainers/features/git:1` (small, stable) and verifies
  resolution + caching.
- **PR8 — Build-source path + dockerfile-gen.** `Engine.Up` for
  `*BuildSource` works (without features). Then layer feature
  dockerfile-gen on top. Integration test builds an image with one
  local feature, verifies install ran.
- **PR9 — `devcontainer.metadata` read + skip-already-installed.**
  Inspect base image label on Up, mark `AlreadyInstalled` features,
  skip dockerfile-gen for them. Integration test uses an image
  pre-built by PR8 and asserts a no-op rebuild.

After M3, build source is fully functional with features. Compose
remains M4.

## 10. Decisions

Resolved during feature design review (2026-05-06):

1. **OCI auth: default keychain + caller hook.** `authn.DefaultKeychain`
   used unless `EngineOptions.OCIKeychain authn.Keychain` is supplied.
   Hook lets a consumer plug in short-lived ECR token auth without writing
   creds to disk.

2. **Buildkit: prefer if available, fall back to classic.** Probe via
   `Info` at `docker.New()` and cache `daemonHasBuildkit`. Classic
   fallback is fine for v1's generated dockerfile (no cache mounts);
   revisit when we add buildkit-only constructs.

3. **Version match: permissive-newer-wins by default; opt-in strict.**
   Default: feature is "already installed" if `id` matches and baked
   version `>= ` requested via semver. Opt-in:
   `EngineOptions.StrictFeatureVersionMatch: true` requires byte-level
   `ResolvedRef` (digest) equality. Non-semver versions are **always**
   treated as strict-match-only — safer than guessing.

4. **DAG depth: warn at 16, error at 64.** Two-tier guardrail in
   `feature.Order`. Warning surfaces as `WarnDeepFeatureChain`; error
   surfaces as `*FeatureDAGTooDeepError{Depth, Path}`.

5. **`Resolve` populates partial `Features`; `Engine.Up` does fetch +
   full DAG.** `Resolve` parses the features map, applies
   `overrideFeatureInstallOrder`, merges user options with what's
   defaultable without fetching; leaves `Metadata`, `Dir`, `ResolvedRef`
   empty and `AlreadyInstalled` false. `Engine.Up` reads the base
   image's `devcontainer.metadata` label, marks `AlreadyInstalled` for
   matching features, and fetches only the rest — preserving the consumer's
   pre-baked-image hot path. M1's stubbed `WarnUnsupportedFeatureField`
   warning goes away in PR6.
