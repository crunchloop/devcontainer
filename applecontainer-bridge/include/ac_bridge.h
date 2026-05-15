#ifndef AC_BRIDGE_H
#define AC_BRIDGE_H

#include <stdint.h>

// ===== ac_bridge.h header-comment style guide ========================
//
// Every export below documents the following five contracts so a
// caller reading only this header knows how to use the function
// safely. Backfilled to ac_version / ac_ping / ac_free in PR-B.
//
//   * Ownership: who frees returned pointers (always the caller for
//     `const char*` returns; free via `ac_free`).
//   * Cancellation: whether `ctx.Done()` on the Go side can interrupt
//     a call mid-flight. PR-B exports are all sync from the Go view
//     (Swift `Task` + DispatchSemaphore wait); cancellation lands when
//     PR-D introduces the handle-table pattern.
//   * Threading: which Swift thread the underlying work runs on. PR-A
//     and PR-B exports run their work in a detached `Task { ... }`
//     and signal a DispatchSemaphore on completion; the @_cdecl
//     function itself runs on the cgo-thread Go gave it.
//   * Error encoding: every export returns a JSON string of shape
//     `{ "ok": bool, "err"?: string, "data"?: <export-specific> }`.
//     `err` is absent or empty on success; `data` is absent on
//     failure. Some PR-A exports return non-JSON payloads — those are
//     called out explicitly in their header comments.
//   * Blocking: whether the call blocks the calling cgo thread. PR-B
//     exports all block (sync from Go's view); fire-and-forget exports
//     with completion callbacks arrive in PR-D.
//
// =====================================================================

// ac_version returns a static descriptor of this bridge build —
// "ACBridge/<bridge-version> apple-container/<apple-version>". Useful
// for diagnostics and confirming the linked bridge matches what the
// caller expects.
//
//   Ownership:    caller frees the returned string with ac_free.
//   Cancellation: n/a; constant-time.
//   Threading:    runs on the cgo thread, no Task indirection.
//   Encoding:     plain UTF-8 string, NOT the JSON envelope.
//   Blocking:     non-blocking.
const char* ac_version(void);

// ac_ping probes the apple/container apiserver via
// ClientHealthCheck.ping. Returns a JSON envelope describing the
// daemon's SystemHealth on success or the underlying error on failure.
// `timeout_seconds <= 0` uses the bridge default (5s).
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired; PR-D adds a handle-table mechanism.
//                 The bridge guards its internal semaphore wait with a
//                 `timeout_seconds + 2` cap so a stuck Task can't hang
//                 the cgo caller indefinitely.
//   Threading:    work runs on a Swift Task; the cgo thread blocks on
//                 a DispatchSemaphore until that Task signals.
//   Encoding:     JSON envelope. On success:
//                   { "ok": true, "apiServerVersion": "...", ... }
//                 On failure:
//                   { "ok": false, "err": "..." }
//                 The success shape is not wrapped under a "data" key
//                 (predates the style guide; left as-is for PR-A
//                 stability — see PR-B for the canonical shape on
//                 inspect/find exports).
//   Blocking:     blocks the cgo thread until the daemon responds or
//                 the internal timeout fires (timeout_seconds + 2).
const char* ac_ping(int32_t timeout_seconds);

// ac_free releases a pointer previously returned by another bridge
// export. Maps to `free()` inside the dylib so allocations and frees
// stay on the same libc; passing a non-bridge pointer is undefined
// behavior.
//
//   Ownership:    transfers ownership back to the dylib for freeing.
//   Cancellation: n/a.
//   Threading:    safe to call from any thread.
//   Encoding:     n/a.
//   Blocking:     non-blocking; same cost as free().
void ac_free(void* p);

// ---- PR-B: Inspect + Find -----------------------------------------

// ac_inspect_container fetches the snapshot for a container by id.
// Wraps ContainerClient.get(id:).
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired (PR-D).
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo thread.
//   Encoding:     { "ok": true, "data": <ContainerSnapshot JSON> } or
//                 { "ok": false, "err": "..." }. The snapshot keys
//                 follow Apple's Codable representation:
//                 configuration{id,image,initProcess,labels,mounts,...},
//                 status, networks, startedDate (optional, RFC3339).
//   Blocking:     blocks until the XPC round-trip completes.
const char* ac_inspect_container(const char* id);

// ac_inspect_image fetches the OCI image config for a local image by
// reference. Wraps ClientImage.get + image.config(for: .current).
// Critical path: the `devcontainer.metadata` label lives here.
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired (PR-D).
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo thread.
//   Encoding:     { "ok": true, "data": {
//                     "reference": "...", "digest": "...",
//                     "labels": {...}, "env": ["..."],
//                     "user": "...", "architecture": "...", "os": "..."
//                   } } or { "ok": false, "err": "..." }. Fields are
//                 flattened from the OCI Image + ImageConfig + the
//                 owning ImageDescription so the Go side gets a single
//                 object to unmarshal.
//   Blocking:     blocks until the local content store lookup completes.
//                 Does NOT pull from a remote registry.
const char* ac_inspect_image(const char* reference);

// ac_find_container_by_label lists running and stopped containers and
// returns the most-recently-started one whose
// `configuration.labels[key] == value`. Matches our
// runtime.FindContainerByLabel contract.
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired (PR-D).
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo thread.
//   Encoding:     { "ok": true, "data": <ContainerSnapshot JSON | null> }
//                 on success — null when no container matches. On
//                 failure: { "ok": false, "err": "..." }.
//   Blocking:     blocks until ContainerClient.list() returns.
const char* ac_find_container_by_label(const char* key, const char* value);

// ---- PR-C: Run / Start / Stop / Delete -----------------------------

// ac_run creates a container from a JSON-encoded RunSpec. Wraps
// ContainerClient.create + ClientKernel.getDefaultKernel. Image must
// already be in the local content store (PullImage handled by PR-F).
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired (PR-D).
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo thread.
//   Encoding:     spec_json is the canonical RunSpec wire shape (see
//                 applecontainer/lifecycle_darwin_arm64.go for the Go
//                 marshaller). Response:
//                   { "ok": true, "data": { "id": "<container-id>" } }
//                 or { "ok": false, "err": "..." }.
//                 RunSpec.RunArgs / Privileged / SecurityOpt are not
//                 modeled on this backend per design §8; the bridge
//                 silently drops them. Image must be pre-pulled.
//   Blocking:     up to 60s (covers cold kernel + init-image fetch on
//                 first run; cached after that).
const char* ac_run(const char* spec_json);

// ac_start bootstraps and starts a previously created container in
// detached mode (no stdio attachment). Idempotent: a running
// container is a no-op success. Wraps ContainerClient.bootstrap +
// ClientProcess.start.
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired (PR-D).
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo thread.
//   Encoding:     { "ok": true } | { "ok": false, "err": "..." }.
//   Blocking:     up to 60s; bootstrap is fast but `process.start()`
//                 spawns the in-VM init.
const char* ac_start(const char* id);

// ac_stop stops a running container. Wraps ContainerClient.stop with
// the given grace-period; timeout_seconds <= 0 uses Apple's default.
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired (PR-D).
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo thread.
//   Encoding:     { "ok": true } | { "ok": false, "err": "..." }.
//   Blocking:     up to 60s (covers the grace period + SIGKILL fallback).
const char* ac_stop(const char* id, int32_t timeout_seconds);

// ac_delete removes a container. `force != 0` deletes even if the
// container is running. Wraps ContainerClient.delete.
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired (PR-D).
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo thread.
//   Encoding:     { "ok": true } | { "ok": false, "err": "..." }.
//   Blocking:     up to 60s.
const char* ac_delete(const char* id, int32_t force);

#endif
