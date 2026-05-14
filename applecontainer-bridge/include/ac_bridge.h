#ifndef AC_BRIDGE_H
#define AC_BRIDGE_H

#include <stdint.h>

const char* ac_version(void);

const char* ac_ping(int32_t timeout_seconds);

void ac_free(void* p);

#endif
