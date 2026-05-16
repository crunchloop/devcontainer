//go:build darwin && arm64

package applecontainer

/*
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unsafe"
)

type logsOpenData struct {
	FD int32 `json:"fd"`
}

// ContainerLogs streams the container's stdio log to w. The
// underlying log file is on disk; non-follow reads to EOF and
// returns. Follow polls after EOF for new data and keeps streaming
// until ctx is cancelled — closing the fd from a watcher goroutine
// unblocks the read.
//
// Boot logs (index 1 from Apple's logs API) are not surfaced.
func (r *Runtime) ContainerLogs(ctx context.Context, id string, w io.Writer, follow bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil {
		return errors.New("applecontainer: log writer is nil")
	}
	if err := ensureLoaded(); err != nil {
		return err
	}
	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))
	raw := goStringAndFree(C.ac_logs_open_p(cID))
	if raw == "" {
		return errors.New("applecontainer: bridge returned nil for ContainerLogs")
	}
	env, err := decodeEnvelope[logsOpenData](raw)
	if err != nil {
		return mapLifecycleErr(id, err)
	}
	if env.decoded.FD < 0 {
		return errors.New("applecontainer: bridge returned invalid log fd")
	}
	file := os.NewFile(uintptr(env.decoded.FD), "applecontainer-logs")
	if file == nil {
		return errors.New("applecontainer: os.NewFile failed for log fd")
	}
	defer file.Close()

	// ctx-watcher: closing the file unblocks any in-flight Read.
	// Use a Once so the file isn't closed twice (defer also runs).
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = file.Close()
		case <-stop:
		}
	}()
	defer close(stop)

	return copyLogs(ctx, file, w, follow)
}

// copyLogs reads from src to w. In follow mode, it sleeps briefly on
// zero-byte reads (EOF on a still-open regular file means "no new
// data yet"). Always returns ctx.Err() if ctx is cancelled, even if
// the underlying file close manifests as an EBADF / "file already
// closed" error.
func copyLogs(ctx context.Context, src *os.File, dst io.Writer, follow bool) error {
	buf := make([]byte, 32*1024)
	pollInterval := 200 * time.Millisecond
	for {
		n, err := src.Read(buf)
		if n > 0 {
			// io.Writer is allowed to short-write without returning
			// an error; loop until the chunk is fully drained so we
			// never silently drop log bytes.
			off := 0
			for off < n {
				written, werr := dst.Write(buf[off:n])
				if werr != nil {
					return fmt.Errorf("applecontainer: log writer: %w", werr)
				}
				if written == 0 {
					return fmt.Errorf("applecontainer: log writer: %w", io.ErrShortWrite)
				}
				off += written
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) || err == io.EOF {
			if !follow {
				return nil
			}
			// Wait for new data, but bail if ctx is done. We can't
			// use a single timer that resets per loop without making
			// the close-on-cancel race tricky; the sleep-poll is
			// simple and bounded.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval):
			}
			continue
		}
		// Read errored — either ctx cancellation triggered a close
		// or the apiserver did something unexpected. Prefer ctx.Err()
		// if cancelled; that's the contract caller wired up.
		if cErr := ctx.Err(); cErr != nil {
			return cErr
		}
		return fmt.Errorf("applecontainer: log read: %w", err)
	}
}
