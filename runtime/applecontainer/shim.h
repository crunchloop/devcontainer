#ifndef AC_SHIM_H
#define AC_SHIM_H

#include <stddef.h>
#include <stdint.h>

// ac_load opens the bridge dylib via dlopen and resolves every exported
// symbol via dlsym. Returns 0 on success, non-zero on failure.
//
// `path` is the absolute filesystem path to libACBridge.dylib (the
// caller is expected to have extracted the embedded bytes to a stable
// location first — see embed_darwin_arm64.go).
//
// On failure the shim writes a null-terminated error message into
// `errbuf` (truncated to `errlen-1` chars). On success errbuf is left
// untouched.
//
// Idempotency: calling ac_load more than once is undefined; the Go
// side guards via sync.Once.
int ac_load(const char* path, char* errbuf, size_t errlen);

// Wrappers that call through the resolved function pointers. Returning
// NULL means either (a) the dylib has not been loaded yet (programmer
// error — must call ac_load first) or (b) the underlying export
// returned NULL.
//
// Contract / encoding / blocking semantics for each wrapped export
// live in applecontainer-bridge/include/ac_bridge.h. The `_p` suffix
// is purely a Go-side reminder that these go through the dlsym
// indirection.
const char* ac_version_p(void);
const char* ac_ping_p(int32_t timeout_seconds);
void ac_free_p(void* p);

const char* ac_inspect_container_p(const char* id);
const char* ac_inspect_image_p(const char* reference);
const char* ac_find_container_by_label_p(const char* key, const char* value);

const char* ac_run_p(const char* spec_json);
const char* ac_start_p(const char* id);
const char* ac_stop_p(const char* id, int32_t timeout_seconds);
const char* ac_delete_p(const char* id, int32_t force);

const char* ac_exec_start_p(
    const char* id,
    const char* opts_json,
    int32_t stdin_read_fd,
    int32_t stdout_write_fd,
    int32_t stderr_write_fd
);
const char* ac_exec_wait_p(uint64_t handle, int32_t timeout_seconds);
const char* ac_exec_signal_p(uint64_t handle, int32_t signal);
void ac_exec_release_p(uint64_t handle);

const char* ac_logs_open_p(const char* id);

#endif
