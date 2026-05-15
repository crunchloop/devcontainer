//go:build darwin && arm64

#include "shim.h"

#include <dlfcn.h>
#include <stdio.h>
#include <string.h>

static const char* (*p_ac_version)(void) = NULL;
static const char* (*p_ac_ping)(int32_t) = NULL;
static void (*p_ac_free)(void*) = NULL;

static const char* (*p_ac_inspect_container)(const char*) = NULL;
static const char* (*p_ac_inspect_image)(const char*) = NULL;
static const char* (*p_ac_find_container_by_label)(const char*, const char*) = NULL;

static const char* (*p_ac_run)(const char*) = NULL;
static const char* (*p_ac_start)(const char*) = NULL;
static const char* (*p_ac_stop)(const char*, int32_t) = NULL;
static const char* (*p_ac_delete)(const char*, int32_t) = NULL;

static const char* (*p_ac_exec_start)(const char*, const char*, int32_t, int32_t, int32_t) = NULL;
static const char* (*p_ac_exec_wait)(uint64_t, int32_t) = NULL;
static const char* (*p_ac_exec_signal)(uint64_t, int32_t) = NULL;
static void (*p_ac_exec_release)(uint64_t) = NULL;

static const char* (*p_ac_logs_open)(const char*) = NULL;
static const char* (*p_ac_pull_image)(const char*) = NULL;
static const char* (*p_ac_build_probe)(void) = NULL;

static void copy_err(char* errbuf, size_t errlen, const char* msg) {
    if (!errbuf || errlen == 0) {
        return;
    }
    if (!msg) {
        msg = "unknown";
    }
    size_t n = strnlen(msg, errlen - 1);
    memcpy(errbuf, msg, n);
    errbuf[n] = '\0';
}

int ac_load(const char* path, char* errbuf, size_t errlen) {
    if (!path) {
        copy_err(errbuf, errlen, "null path");
        return -1;
    }
    // RTLD_LOCAL keeps the bridge's symbols isolated from the global
    // namespace so we don't collide with anything else the Go binary
    // might dlopen.
    void* h = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    if (!h) {
        copy_err(errbuf, errlen, dlerror());
        return -1;
    }

    // Cast through a generic function pointer to silence the warning
    // about converting void* (data) to a function-pointer type.
    p_ac_version                 = (const char* (*)(void)) dlsym(h, "ac_version");
    p_ac_ping                    = (const char* (*)(int32_t)) dlsym(h, "ac_ping");
    p_ac_free                    = (void (*)(void*)) dlsym(h, "ac_free");
    p_ac_inspect_container       = (const char* (*)(const char*)) dlsym(h, "ac_inspect_container");
    p_ac_inspect_image           = (const char* (*)(const char*)) dlsym(h, "ac_inspect_image");
    p_ac_find_container_by_label = (const char* (*)(const char*, const char*)) dlsym(h, "ac_find_container_by_label");
    p_ac_run                     = (const char* (*)(const char*)) dlsym(h, "ac_run");
    p_ac_start                   = (const char* (*)(const char*)) dlsym(h, "ac_start");
    p_ac_stop                    = (const char* (*)(const char*, int32_t)) dlsym(h, "ac_stop");
    p_ac_delete                  = (const char* (*)(const char*, int32_t)) dlsym(h, "ac_delete");
    p_ac_exec_start              = (const char* (*)(const char*, const char*, int32_t, int32_t, int32_t)) dlsym(h, "ac_exec_start");
    p_ac_exec_wait               = (const char* (*)(uint64_t, int32_t)) dlsym(h, "ac_exec_wait");
    p_ac_exec_signal             = (const char* (*)(uint64_t, int32_t)) dlsym(h, "ac_exec_signal");
    p_ac_exec_release            = (void (*)(uint64_t)) dlsym(h, "ac_exec_release");
    p_ac_logs_open               = (const char* (*)(const char*)) dlsym(h, "ac_logs_open");
    p_ac_pull_image              = (const char* (*)(const char*)) dlsym(h, "ac_pull_image");
    p_ac_build_probe             = (const char* (*)(void)) dlsym(h, "ac_build_probe");

    if (!p_ac_version || !p_ac_ping || !p_ac_free
        || !p_ac_inspect_container || !p_ac_inspect_image
        || !p_ac_find_container_by_label
        || !p_ac_run || !p_ac_start || !p_ac_stop || !p_ac_delete
        || !p_ac_exec_start || !p_ac_exec_wait
        || !p_ac_exec_signal || !p_ac_exec_release
        || !p_ac_logs_open || !p_ac_pull_image
        || !p_ac_build_probe) {
        const char* err = dlerror();
        copy_err(errbuf, errlen, err ? err : "dlsym returned null");
        // Reset any partial resolutions so a future retry sees a clean
        // slate, and release the dlopen handle so the dylib refcount
        // drops back to zero.
        p_ac_version = NULL;
        p_ac_ping = NULL;
        p_ac_free = NULL;
        p_ac_inspect_container = NULL;
        p_ac_inspect_image = NULL;
        p_ac_find_container_by_label = NULL;
        p_ac_run = NULL;
        p_ac_start = NULL;
        p_ac_stop = NULL;
        p_ac_delete = NULL;
        p_ac_exec_start = NULL;
        p_ac_exec_wait = NULL;
        p_ac_exec_signal = NULL;
        p_ac_exec_release = NULL;
        p_ac_logs_open = NULL;
        p_ac_pull_image = NULL;
        p_ac_build_probe = NULL;
        dlclose(h);
        return -1;
    }
    return 0;
}

const char* ac_version_p(void) {
    return p_ac_version ? p_ac_version() : NULL;
}

const char* ac_ping_p(int32_t t) {
    return p_ac_ping ? p_ac_ping(t) : NULL;
}

void ac_free_p(void* p) {
    if (p_ac_free) {
        p_ac_free(p);
    }
}

const char* ac_inspect_container_p(const char* id) {
    return p_ac_inspect_container ? p_ac_inspect_container(id) : NULL;
}

const char* ac_inspect_image_p(const char* reference) {
    return p_ac_inspect_image ? p_ac_inspect_image(reference) : NULL;
}

const char* ac_find_container_by_label_p(const char* key, const char* value) {
    return p_ac_find_container_by_label ? p_ac_find_container_by_label(key, value) : NULL;
}

const char* ac_run_p(const char* spec_json) {
    return p_ac_run ? p_ac_run(spec_json) : NULL;
}

const char* ac_start_p(const char* id) {
    return p_ac_start ? p_ac_start(id) : NULL;
}

const char* ac_stop_p(const char* id, int32_t timeout_seconds) {
    return p_ac_stop ? p_ac_stop(id, timeout_seconds) : NULL;
}

const char* ac_delete_p(const char* id, int32_t force) {
    return p_ac_delete ? p_ac_delete(id, force) : NULL;
}

const char* ac_exec_start_p(
    const char* id,
    const char* opts_json,
    int32_t stdin_read_fd,
    int32_t stdout_write_fd,
    int32_t stderr_write_fd
) {
    return p_ac_exec_start
        ? p_ac_exec_start(id, opts_json, stdin_read_fd, stdout_write_fd, stderr_write_fd)
        : NULL;
}

const char* ac_exec_wait_p(uint64_t handle, int32_t timeout_seconds) {
    return p_ac_exec_wait ? p_ac_exec_wait(handle, timeout_seconds) : NULL;
}

const char* ac_exec_signal_p(uint64_t handle, int32_t signal) {
    return p_ac_exec_signal ? p_ac_exec_signal(handle, signal) : NULL;
}

void ac_exec_release_p(uint64_t handle) {
    if (p_ac_exec_release) {
        p_ac_exec_release(handle);
    }
}

const char* ac_logs_open_p(const char* id) {
    return p_ac_logs_open ? p_ac_logs_open(id) : NULL;
}

const char* ac_pull_image_p(const char* reference) {
    return p_ac_pull_image ? p_ac_pull_image(reference) : NULL;
}

const char* ac_build_probe_p(void) {
    return p_ac_build_probe ? p_ac_build_probe() : NULL;
}
