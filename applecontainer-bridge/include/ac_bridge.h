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
//                 modeled on this backend per design §8. The Go layer
//                 rejects callers that populate them with a typed
//                 UnsupportedOptionError before reaching this entry
//                 point; the wire shape doesn't carry those fields.
//                 Image must be pre-pulled.
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

// ---- PR-D: Exec (stdin/TTY + cancellation) -------------------------

// ac_exec_start launches an exec process inside a running container.
// stdio fds are caller-supplied via os.Pipe() on the Go side: caller
// passes the apiserver-facing end (read-end for stdin, write-end for
// stdout/stderr); -1 disables that stream. XPC dup's the fds when the
// XPCMessage is serialized, so the caller may close its passed fd
// immediately after this call returns (the process keeps the dup'd
// copy).
//
//   Ownership:    caller frees the returned JSON with ac_free.
//   Cancellation: returned `handle` is the cancellation token. Pass
//                 it to ac_exec_signal with SIGTERM (or any signal)
//                 to deliver to the in-VM process.
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo
//                 thread. The launched process runs on its own VM
//                 thread; this call returns once createProcess +
//                 start have settled at the apiserver.
//   Encoding:     opts_json = { "cmd":[..], "env":[..], "user":"",
//                                "workingDir":"", "tty":false }.
//                 Response on success:
//                   { "ok": true, "data": { "handle": uint64 } }
//                 On failure: { "ok": false, "err": "..." }. After
//                 this returns ok, the caller MUST eventually call
//                 ac_exec_release(handle) to free the registry slot.
//   Blocking:     up to 30s (createProcess XPC + start).
const char* ac_exec_start(
    const char* id,
    const char* opts_json,
    int32_t stdin_read_fd,
    int32_t stdout_write_fd,
    int32_t stderr_write_fd
);

// ac_exec_wait blocks until the exec process exits, returning its
// exit code. timeout_seconds <= 0 disables the timeout (capped to
// ~Int32.max/1000 internally to avoid Duration overflow).
//
//   Ownership:    caller frees the returned JSON with ac_free.
//   Cancellation: external — call ac_exec_signal from another thread
//                 to send SIGTERM; this wait then returns the
//                 resulting exit code naturally.
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo
//                 thread.
//   Encoding:     { "ok": true, "data": { "exitCode": int32 } } or
//                 { "ok": false, "err": "..." }.
//   Blocking:     unbounded by design (modulo timeout_seconds).
const char* ac_exec_wait(uint64_t handle, int32_t timeout_seconds);

// ac_exec_signal delivers a signal to the in-VM process. The
// cancellation contract: Go's ctx.Done() goroutine calls this with
// SIGTERM; ac_exec_wait then returns naturally as the process exits.
//
//   Ownership:    caller frees the returned JSON with ac_free.
//   Cancellation: n/a (this IS the cancellation primitive).
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo
//                 thread.
//   Encoding:     { "ok": true } or { "ok": false, "err": "..." }.
//   Blocking:     up to 5s (the apiserver's kill XPC is fast).
const char* ac_exec_signal(uint64_t handle, int32_t signal);

// ac_exec_release frees the handle's registry slot. Idempotent —
// calling on an unknown handle is a no-op. Must be called after
// ac_exec_wait returns to avoid leaking ClientProcess instances.
//
//   Ownership:    n/a; no return value.
//   Cancellation: n/a.
//   Threading:    safe from any thread.
//   Encoding:     n/a.
//   Blocking:     non-blocking; lock acquisition only.
void ac_exec_release(uint64_t handle);

// ---- PR-E: Logs streaming ------------------------------------------

// ac_logs_open returns a dup'd file descriptor for the container's
// stdio log. The fd is a regular file on disk; reads return 0 bytes
// at EOF. Callers implement follow mode by polling on EOF; ctx
// cancellation is signaled by closing the fd Go-side.
//
//   Ownership:    caller owns the returned fd and must close it with
//                 close(2). The JSON envelope itself is freed with
//                 ac_free as usual.
//   Cancellation: external — close the fd from another thread to
//                 unblock a Go read.
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo
//                 thread.
//   Encoding:     { "ok": true, "data": { "fd": int32 } } or
//                 { "ok": false, "err": "..." }.
//   Blocking:     up to 10s (one XPC round-trip + dup).
const char* ac_logs_open(const char* id);

// ---- PR-F: Pull ----------------------------------------------------

// ac_pull_image fetches an image from a remote registry into the
// local content store. Synchronous; returns when the image is fully
// pulled and unpacked.
//
//   Ownership:    caller frees the returned JSON with ac_free.
//   Cancellation: not yet wired. Apple's pull API doesn't expose a
//                 cancellation token; aborting cleanly would require
//                 deleting the partial image — left for a future PR.
//                 Documented in design §8.
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo
//                 thread.
//   Encoding:     { "ok": true, "data": { "reference": "...", "digest": "..." } }
//                 or { "ok": false, "err": "..." }.
//   Blocking:     up to 30 min (covers a cold pull of a multi-GB
//                 base image on a reasonable network). The bridge's
//                 timeout will trip before this in most realistic
//                 cases.
const char* ac_pull_image(const char* reference);

// ---- PR-G2: Build --------------------------------------------------

// ac_build_probe checks whether Apple's buildkit container is up.
// Callers use this to short-circuit with a typed
// BuilderUnavailableError before paying the cost of marshaling a
// full BuildSpec.
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired.
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo
//                 thread.
//   Encoding:     Success: { "ok": true }.
//                 Failure with stable code:
//                   { "ok": false,
//                     "code": "BUILDER_UNAVAILABLE",
//                     "err":  "<human-readable detail>" }
//                 Failure without a known code:
//                   { "ok": false, "err": "..." }
//                 The `code` field is the machine-readable contract
//                 the Go side keys typed errors off of; `err` is for
//                 diagnostics only.
//   Blocking:     up to 5s (one XPC round-trip).
const char* ac_build_probe(void);

// ac_build performs the actual BuildKit build. Dials the buildkit
// container over vsock (must be running; we surface a clear error
// otherwise), constructs a SwiftNIO-backed Builder, runs the build
// to an OCI tarball export, then loads + unpacks + tags the result
// in the local content store.
//
//   Ownership:    caller frees with ac_free.
//   Cancellation: not yet wired. Build is long-running; callers
//                 should treat it as best-effort uninterruptible
//                 until a follow-up PR adds streaming cancellation.
//   Threading:    Swift Task + DispatchSemaphore wait on the cgo
//                 thread. Internally spins up a NIO
//                 MultiThreadedEventLoopGroup for the duration of
//                 the build and shuts it down on the way out.
//   Encoding:     spec_json fields (omitempty unless noted):
//                   contextPath (required), dockerfile,
//                   tag, args (map[string]string), target,
//                   cacheFrom ([]string), noCache (bool),
//                   platform (single platform string, e.g. linux/arm64).
//                 Engine concepts not modeled on this backend
//                 (RunArgs, Privileged, SecurityOpt analogues) are
//                 rejected by the Go layer before reaching this
//                 entry point — same pattern as ac_run (design §8).
//                 Multi-platform builds are out of scope; pass a
//                 single platform.
//                 Success:
//                   { "ok": true, "data": { "reference": "...", "digest": "..." } }
//                 Failure with stable code (same contract as
//                 ac_build_probe — currently BUILDER_UNAVAILABLE):
//                   { "ok": false, "code": "...", "err": "..." }
//                 Failure without a known code:
//                   { "ok": false, "err": "..." }.
//                 BuildKit progress goes to FileHandle.standardError
//                 of the bridge process (which the Go process
//                 inherits). Typed BuildEvent streaming is a future
//                 PR — for now callers see raw output on stderr.
//   Blocking:     up to 30 min (covers a cold cache pull + multi-
//                 layer build). The bridge's internal timeout fires
//                 at the same horizon.
const char* ac_build(const char* spec_json);

#endif
