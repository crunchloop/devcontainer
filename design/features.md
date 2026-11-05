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

```text
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

Security posture for the HTTPS client:

- TLS certificates validated against system root CAs; `EngineOptions.HTTPSRootCAs *x509.CertPool` lets callers add private CAs for internal mirrors. `InsecureSkipVerify` is never set.
- Redirects: follow up to 5 hops; reject `http://` targets even on redirect. Go's `net/http` strips `Authorization` / `Cookie` / `Proxy-Authorization` on cross-host redirects but forwards arbitrary custom headers unchanged — so we install a `CheckRedirect` that drops every caller-supplied `FeatureDownloadHeaders` entry when the redirect crosses hosts. Callers needing cross-host auth pass an explicit host allowlist via `EngineOptions.FeatureDownloadHeaderHosts`.
- Response size cap: 100 MB by default (`EngineOptions.FeatureDownloadMaxBytes`). Body is streamed with a `LimitReader`; overflow returns `*FeatureTooLargeError`.
- Timeouts: 30s connect, 5min overall (`EngineOptions.FeatureDownloadTimeout`). No retry — features are large enough that retries mostly mask deeper problems; callers can re-run.
- All network failures surface as `*FeatureFetchError{Ref, Cause}`; the cache write is atomic (extract to tmp dir, rename on success) so a partial download never poisons the cache.

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

We **validate enum/proposals at fetch time** — specifically, inside
`feature.Store.Resolve()` once `devcontainer-feature.json` has been
fetched and `Metadata` is populated (see §10 decision #5 for the
two-layer Resolve split). Devpod defers this to `install.sh`; rejecting
typos before image build is worth the small duplication.

`Engine.Up` performs the same check as a backstop for any feature whose
metadata wasn't resolved earlier (e.g. injected mutations). On the
pre-baked-image hot path, features marked `AlreadyInstalled` from the
base image's `devcontainer.metadata` label skip resolution entirely;
their options were validated when the image was originally built, so
re-validating without remote metadata would force unnecessary fetches.

## 5. DAG ordering

Ordered `[]ResolvedFeature` per spec rules:

1. Features named in `overrideFeatureInstallOrder` first, in declaration
   order. (Hard override; not topo-sorted.)
2. Remaining features: topo-sorted via leaf-extraction over a graph built
   from `installsAfter` and `dependsOn`.

`dependsOn` is a hard dependency: the engine installs it whether or not
`devcontainer.json` declares it. `installsAfter` is a soft "if both
listed, this one waits" hint. See §5.1 for how dependencies are pulled
in.

### 5.1 Pulling in `dependsOn`

The spec and the reference CLI agree on what counts as one feature. Two
features are equal when they have the same content (manifest digest for
OCI, body sha256 for HTTPS) **and** the same options. So one feature
requested with two sets of options is installed twice. `layerFeatures`
already fetches each feature in turn. That loop now also appends the
features a fetched feature's `dependsOn` requires, which the loop then
fetches like declared ones: pinned through the lockfile, with defaults
applied and options validated. No new graph structure is needed.

Duplicates are recognised at two points. Both look at the feature list
built so far:

- **Before fetching**, a dependency already requested with the same
  key and options is skipped. This is what stops a cycle and avoids
  refetching a shared dependency.
- **After fetching**, a feature whose content and merged options match
  an earlier one is dropped. This covers the same feature reached
  under a different reference (`:1` and `:1.0` resolving to the same
  digest).

`topoSort` maps each id to all of its entries. A `dependsOn` edge goes
to the entries installed with the options the dependency asks for.
`installsAfter` edges go to every entry for the id. Ties are broken by
ref, then by options.

Decisions:

1. **Options are compared after defaults are applied.** The spec
   compares "the options executed against the Feature"; the CLI
   compares the options as written. Comparing merged options means
   `{}` and `{"version": "latest"}` (when `latest` is the default) are
   one install, not two of the same thing. No file format depends on
   this, so following the spec costs no compatibility.
2. **Declared features are deduplicated by content too.** If
   `devcontainer.json` lists two references that resolve to the same
   digest with the same options, the feature is installed once.
   Previously it was installed twice. This is the CLI's behaviour as
   well.
3. **Local `dependsOn` paths resolve against `devcontainer.json`'s
   directory**, as declared local features do and as the CLI does. They
   are not resolved against the declaring feature's directory. They are
   also **confined**, because feature metadata, not the user, chooses
   them, and the directory they name is copied into the build and its
   `install.sh` run:
   - Only a local feature may declare one. A published feature cannot
     know the consumer's files; a local key from it is refused. This is
     stricter than the CLI, which follows it if it stays inside
     `.devcontainer/`.
   - The key must be relative and resolve, symlinks included, inside
     the directory of `devcontainer.json`, as the CLI requires of local
     features (`containerFeaturesConfiguration.ts`: no absolute paths,
     no escape from `.devcontainer/`).

   Declared local features keep their existing, unconfined resolution:
   the user wrote those paths, and confining them would break configs
   that point outside the config directory.
4. **A dependency is never taken from the base image**, even when its
   label lists it: it is fetched and installed like any other feature.
   The pre-baked match compares id and version only, and a `dependsOn`
   entry's options are chosen by the feature's author, not the user, so
   an image built with other options would silently stand in. #154 adds
   option matching to the pre-baked match; dependencies can reuse the
   image once it lands. The cost until then: a pre-baked image whose
   features have dependencies reinstalls those dependencies on every
   build.

   A declared feature the image carries is still not reinstalled, but
   its dependencies are installed, so they must be known. Labels now
   record each installed feature's `dependsOn` (as `{}` when it has
   none), so for an image built from now on they come from the label
   without a fetch. For a label written before that, the baked
   feature's metadata is fetched at the artifact the label records, a
   digest reference the feature cache serves locally once seen.
5. **A dependency dropped as a duplicate is still locked.** When a
   `dependsOn` key resolves to the same content and options as a
   feature already in the set, it is not installed twice, but its key
   still gets a lockfile entry for that content, so the reference it was
   requested by stays pinned.
6. **OCI references are lowercased before parsing**, as the CLI's
   `getRef` does. Published `dependsOn` keys such as
   `ghcr.io/codspace/dependson/A` are written by feature authors, not
   the user, and go-containerregistry rejects uppercase repository
   names.

Cycles that run through a pulled-in feature are reported the same way
as any other cycle, as `*FeatureCycleError`.

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

ARG _DEV_CONTAINERS_IMAGE_USER=root

# --- per-feature layers (in install order) ---
RUN \
  echo "_CONTAINER_USER_HOME=$(getent passwd root | cut -d: -f6)" \
    >> /tmp/dc-features/builtin.env && \
  echo "_REMOTE_USER_HOME=$(getent passwd ${_DEV_CONTAINERS_IMAGE_USER:-root} | cut -d: -f6)" \
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

```text
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

```text
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
    tags/
      <sha256(ref)>.json    # {"ref": "<registry/repo:tag>", "digest": "sha256:…"}
```

Cache key is **content-addressed**: OCI digest or HTTPS body sha256. A
tag resolves to a digest, then hits the disk cache. This is
intentionally different from devpod's "hash-the-ref-string" scheme —
content-addressed dedupes across mirror URLs and prevents stale-ref
bugs. Extraction goes to a temporary directory in the entry and is
renamed into place, so a partial extraction is never taken for a hit.

### 8.1 OCI resolution policy (v0.5.2)

The cache is only useful offline if the registry is not asked first.
`DiskStore` decides per reference form:

| Reference | Policy |
| --- | --- |
| `name@sha256:…` | Local first: a cached extraction of the digest is used with no registry call. |
| `name:X.Y.Z` (exact version) | Local first: the digest recorded under `tags/` the last time this tag was resolved is used with no registry call, if its extraction is cached. Published versions are immutable by the publishing convention (the reference CLI publishes `X.Y.Z` once and moves only `X`, `X.Y` and `latest`). |
| `name:X`, `name:X.Y`, `name:latest`, any other tag | Online first, so a moved tag is picked up. If the registry is **unreachable** (no HTTP response: DNS, connect, TLS, timeout), the recorded resolution is used if its extraction is cached. |

A registry that answers with an error (404, 401, 403, …) is
authoritative: its error is returned even when a resolution is
recorded. With nothing recorded or the content evicted, an unreachable
registry fails the fetch as before. Every successful online fetch of a
tag rewrites its record (write to a temporary file, then rename). HTTPS
features are unchanged: their content hash is only known after the
download.

### 8.2 Lockfile

The spec's [`devcontainer-lock.json`](https://github.com/devcontainers/spec/blob/main/docs/specs/devcontainer-lockfile.md)
records, beside `devcontainer.json`, the exact artifact every OCI and
HTTPS feature resolved to. `UpOptions.Lockfile` / `BuildOptions.Lockfile`
select the mode. These follow the reference CLI as of 0.87, where the
lockfile is on by default:

| Mode | Reads pins | Writes | Fails when |
| --- | --- | --- | --- |
| `LockfileDefault` | yes | when what resolved differs (created if missing) | a pinned HTTPS download has a different digest |
| `LockfileFrozen` | yes | never | the lockfile is missing (before any fetch), or what resolved differs from it |
| `LockfileUpgrade` | no | when what resolved differs | — |
| `LockfileDisabled` | no | never | — |

`DiskStore` already returns the two values the lockfile records as
`Fetched.ResolvedRef`: the OCI manifest digest and the HTTPS body
sha256. So the lockfile needs no store change:

- **OCI pin:** `layerFeatures` fetches `repo@<integrity>` instead of the
  tag. That is a digest reference, so §8.1 serves it from cache without
  a registry call: a locked workspace builds offline.
- **HTTPS pin:** the URL is fetched as before and its digest is compared
  with `integrity` (`*feature.FeatureIntegrityError` on mismatch).
- **Write:** after the fetch loop and before the build, as the CLI does.
  The lockfile is regenerated from scratch, so stale entries drop,
  including when the last feature is removed (frozen mode reports them;
  a config without features and without a lockfile gets none). It is
  compared by content, not bytes, and written only when entries changed.

Decisions:

1. **CLI over spec where they differ.** Keys are the feature reference
   verbatim (the spec says lowercase; the CLI doesn't), and a
   `.devcontainer.json` gets a `.devcontainer-lock.json`. The output
   format is byte-identical to the CLI's (2-space indent, sorted keys,
   trailing newline; checked against the CLI's own generated test
   fixtures). The one divergence is key order for keys where bytewise
   order and `localeCompare` disagree. Comparing by content keeps either
   tool from rewriting the other's file over it.
2. **A baked feature stands in for a fetch only when the lockfile
   accepts its artifact.** The base image's label records each
   feature's `resolvedRef` (this library has written it since v0.1.0),
   and the label match additionally requires that artifact to be the
   pinned one, or, with no pin, a known one that is then locked. A
   baked feature of a different or unrecorded artifact is fetched at
   its pin and installed over the base image, so frozen mode never
   accepts a base image the lockfile does not describe. An accepted
   feature keeps its pin rather than being re-locked from the label,
   which does not record `dependsOn`. Upgrade mode accepts no baked
   feature: it resolves every reference afresh, and locking the baked
   artifact could replace a newer pin with an older one. With the
   lockfile disabled the label match alone decides, as before.

   Labels written by other tools record no artifact. The reference
   CLI's never matched here anyway: their `id` is the full reference,
   not the bare id the match compares.
3. **Compose reattach neither reads nor writes the lockfile, and the
   lockfile is not part of the compose inputs hash.** Reattach resolves
   nothing, matching the CLI, which does not re-resolve features when it
   starts an existing container. Hashing the lockfile looks tidier, but
   the first `Up` writes it, so the second `Up` would see a new hash and
   recreate every compose project once.
4. **`Up` and `Build` fix the config path before resolving.** With
   `ConfigPath` empty they used to drop the discovered path and assume
   `.devcontainer/`. That put the lockfile, local features and the
   compose working directory in the wrong place for a root
   `.devcontainer.json`.

5. **Features pulled in through `dependsOn` (§5.1) are locked and
   pinned like declared ones.** Their key is the `dependsOn` key as
   written, as in the CLI. A frozen lockfile that lacks one is out of
   date. Verified against the CLI's own `lockfile-dependson` fixture.
6. **The compose inputs hash stays declared-only, with one
   exception.** A pulled-in feature is implied by the declaring
   feature's entry: a local feature's hashed content holds its
   `dependsOn` keys and options, and an OCI feature is identified by
   its reference as written. The exception is a *local* dependency's
   own content, which nothing else covers, so it is hashed
   transitively from the declared local features. That reaches every
   local dependency, since only a local feature may declare one
   (§5.1 decision 3). Projects without one hash exactly as before, so
   there is no recreate on upgrade.

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
   version `>=` requested via semver. Opt-in:
   `EngineOptions.StrictFeatureVersionMatch: true` requires byte-level
   `ResolvedRef` (digest) equality. Non-semver versions are **always**
   treated as strict-match-only — safer than guessing.

4. **DAG depth: warn at 16, error at 64.** Two-tier guardrail in
   `feature.Order`. Warning surfaces as `WarnDeepFeatureChain`; error
   surfaces as `*FeatureDAGTooDeepError{Depth, Path}`.

5. **Two layers of "Resolve"; `Engine.Up` orchestrates them.** Two
   distinct operations share the name and should not be conflated:
   - **Config-level** `devcontainer.Resolve` parses the features map,
     applies `overrideFeatureInstallOrder`, and merges user options with
     what's defaultable without fetching. It intentionally leaves each
     `ResolvedFeature`'s `Metadata`, `Dir`, `ResolvedRef` empty and
     `AlreadyInstalled` false.
   - **Feature-level** `feature.Store.Resolve` (§3) performs the actual
     fetch for one ref and returns a `*Resolved` with `Metadata`, `Dir`,
     and `ResolvedRef` populated.

   `Engine.Up` reads the base image's `devcontainer.metadata` label,
   marks `AlreadyInstalled` for matching features, and then calls
   `feature.Store.Resolve` only for the rest — preserving the consumer's
   pre-baked-image hot path. M1's stubbed `WarnUnsupportedFeatureField`
   warning goes away in PR6.
