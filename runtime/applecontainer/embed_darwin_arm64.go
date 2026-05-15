//go:build darwin && arm64

package applecontainer

/*
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"crypto/sha256"
	"encoding/hex"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"
)

// bridgeDylib is the libACBridge.dylib produced by `make bridge`.
// The embed expects the file to exist at build time; on a fresh
// checkout you must run `make bridge` once before the package will
// compile on darwin/arm64.
//
//go:embed embed/libACBridge.dylib
var bridgeDylib []byte

var (
	loadOnce sync.Once
	loadErr  error
)

// ensureLoaded extracts the embedded dylib to a per-user cache path
// (keyed by the dylib's content hash so multiple bridge versions
// coexist) and dlopens it via the C shim. Idempotent — subsequent
// calls are no-ops.
func ensureLoaded() error {
	loadOnce.Do(func() { loadErr = loadBridge() })
	return loadErr
}

func loadBridge() error {
	if len(bridgeDylib) == 0 {
		return fmt.Errorf("applecontainer: embedded dylib is empty (run `make bridge`)")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("applecontainer: UserCacheDir: %w", err)
	}
	cacheDir = filepath.Join(cacheDir, "devcontainer-go", "applecontainer")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("applecontainer: mkdir cache: %w", err)
	}

	sum := sha256.Sum256(bridgeDylib)
	hashed := hex.EncodeToString(sum[:])
	dylibPath := filepath.Join(cacheDir, hashed+".dylib")

	if _, err := os.Stat(dylibPath); os.IsNotExist(err) {
		// Write to a temp file then rename, so a partial write from a
		// crashed process can't be picked up by a concurrent reader.
		tmp, err := os.CreateTemp(cacheDir, hashed+".dylib.*")
		if err != nil {
			return fmt.Errorf("applecontainer: create tmp dylib: %w", err)
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(bridgeDylib); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("applecontainer: write dylib: %w", err)
		}
		if err := tmp.Chmod(0o755); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("applecontainer: chmod dylib: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("applecontainer: close dylib: %w", err)
		}
		if err := os.Rename(tmpPath, dylibPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("applecontainer: rename dylib: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("applecontainer: stat dylib: %w", err)
	}

	cPath := C.CString(dylibPath)
	defer C.free(unsafe.Pointer(cPath))
	var errbuf [512]C.char
	if rc := C.ac_load(cPath, &errbuf[0], C.size_t(len(errbuf))); rc != 0 {
		return fmt.Errorf("applecontainer: dlopen %s: %s", dylibPath, C.GoString(&errbuf[0]))
	}
	return nil
}
