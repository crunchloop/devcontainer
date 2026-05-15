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

    if (!p_ac_version || !p_ac_ping || !p_ac_free
        || !p_ac_inspect_container || !p_ac_inspect_image
        || !p_ac_find_container_by_label) {
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
