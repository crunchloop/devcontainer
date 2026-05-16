//go:build darwin && arm64

package applecontainer

/*
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/crunchloop/devcontainer/runtime"
)

// execOptsJSON mirrors lifecycle's RunSpecJSON pattern: explicit
// wire-only struct so the silently-dropped fields from
// runtime.ExecOptions are visible in code review.
type execOptsJSON struct {
	Cmd        []string `json:"cmd"`
	Env        []string `json:"env,omitempty"`
	User       string   `json:"user,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"`
	TTY        bool     `json:"tty,omitempty"`
}

type execStartData struct {
	Handle uint64 `json:"handle"`
}

type execWaitData struct {
	ExitCode int32 `json:"exitCode"`
}

// ExecContainer runs a command inside a running container.
//
// stdio plumbing: each requested stream gets an os.Pipe pair. The
// "apiserver-facing" end is passed to the bridge as an fd (XPC dup's
// it during the createProcess XPC), then closed locally. The
// "Go-facing" end stays open and is wired to the caller's
// io.Reader/Writer via goroutines.
//
// Cancellation: a ctx-watcher goroutine sends SIGTERM via the bridge
// when ctx.Done() fires. The wait then returns naturally (typically
// exit code 143 = 128 + SIGTERM). ExecContainer returns ctx.Err() in
// that case, regardless of the underlying exit code.
//
// When opts.Stdout/Stderr are nil, output is captured into the
// returned ExecResult.Stdout/Stderr. The bridge always opens a
// stdout pipe; stderr is suppressed in TTY mode (Apple merges
// stderr into stdout when terminal=true) and the captured/streamed
// stderr is empty in that case.
func (r *Runtime) ExecContainer(ctx context.Context, id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return runtime.ExecResult{}, err
	}
	if err := ensureLoaded(); err != nil {
		return runtime.ExecResult{}, err
	}
	if len(opts.Cmd) == 0 {
		return runtime.ExecResult{}, errors.New("applecontainer: ExecOptions.Cmd is empty")
	}

	// Build the pipes Go-side. Failure on any pipe is unrecoverable;
	// we close partial allocations before bailing.
	pipes, err := openExecPipes(opts)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	defer pipes.closeLocal() // belt-and-suspenders; goroutines also close

	// Marshal the per-call opts and hand fds to the bridge.
	wire := execOptsJSON{
		Cmd:        opts.Cmd,
		Env:        envMapToSlice(opts.Env),
		User:       opts.User,
		WorkingDir: opts.WorkingDir,
		TTY:        opts.Tty,
	}
	optsBytes, err := json.Marshal(wire)
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("applecontainer: marshal exec opts: %w", err)
	}

	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))
	cOpts := C.CString(string(optsBytes))
	defer C.free(unsafe.Pointer(cOpts))

	raw := goStringAndFree(C.ac_exec_start_p(
		cID, cOpts,
		C.int32_t(pipes.stdinReadFd()),
		C.int32_t(pipes.stdoutWriteFd()),
		C.int32_t(pipes.stderrWriteFd()),
	))
	if raw == "" {
		return runtime.ExecResult{}, errors.New("applecontainer: bridge returned nil for ExecStart")
	}
	startEnv, err := decodeEnvelope[execStartData](raw)
	if err != nil {
		return runtime.ExecResult{}, mapLifecycleErr(id, err)
	}
	handle := startEnv.decoded.Handle

	// The bridge (and through XPC, the apiserver) now owns dup'd
	// copies of the apiserver-facing fds. Close ours so the only
	// references are the ones we still want (Go-facing ends).
	pipes.closeRemoteEnds()

	// Spawn the stdio goroutines. We use a WaitGroup to ensure all
	// copies drain before we return — losing stdout because the
	// reader goroutine hadn't finished would be a silent footgun.
	var (
		wg         sync.WaitGroup
		stdoutBuf  strings.Builder
		stderrBuf  strings.Builder
		stdoutSink = pickWriter(opts.Stdout, &stdoutBuf)
		stderrSink = pickWriter(opts.Stderr, &stderrBuf)
		copyErrCh  = make(chan error, 3)
	)

	if pipes.stdinWriter() != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Write what the caller provided, then close so the
			// container sees EOF on stdin. Errors here are usually
			// "broken pipe" because the process exited; surface them
			// via copyErrCh but don't fail the exec.
			if opts.Stdin != nil {
				if _, err := io.Copy(pipes.stdinWriter(), opts.Stdin); err != nil && !isBrokenPipe(err) {
					copyErrCh <- fmt.Errorf("stdin copy: %w", err)
				}
			}
			pipes.stdinWriter().Close()
		}()
	}
	if pipes.stdoutReader() != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(stdoutSink, pipes.stdoutReader()); err != nil && !isBrokenPipe(err) {
				copyErrCh <- fmt.Errorf("stdout copy: %w", err)
			}
		}()
	}
	if pipes.stderrReader() != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(stderrSink, pipes.stderrReader()); err != nil && !isBrokenPipe(err) {
				copyErrCh <- fmt.Errorf("stderr copy: %w", err)
			}
		}()
	}

	// ctx-watcher: send SIGTERM on cancel. The watcher exits either
	// when ctx fires (cancelled path) or when we close `done` after
	// wait returns (clean path).
	var cancelled bool
	var cancelMu sync.Mutex
	doneSignal := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cancelMu.Lock()
			cancelled = true
			cancelMu.Unlock()
			C.ac_exec_signal_p(C.uint64_t(handle), C.int32_t(syscall.SIGTERM))
		case <-doneSignal:
		}
	}()

	// Wait for the in-VM process to exit. This is the long block.
	// timeout_seconds=0 means "use the bridge's internal max".
	waitRaw := goStringAndFree(C.ac_exec_wait_p(C.uint64_t(handle), 0))
	close(doneSignal)

	// Always release the handle before we leave the function.
	defer C.ac_exec_release_p(C.uint64_t(handle))

	// Drain copy goroutines. The apiserver closes its fd when the
	// process exits, which surfaces as EOF on our reader, so io.Copy
	// returns naturally without us having to close anything. Closing
	// the Go-facing ends BEFORE the drain (an earlier safety net)
	// risked truncating buffered output, so it was removed.
	wg.Wait()
	close(copyErrCh)

	var copyErr error
	for e := range copyErrCh {
		copyErr = errors.Join(copyErr, e)
	}

	cancelMu.Lock()
	wasCancelled := cancelled
	cancelMu.Unlock()
	if wasCancelled {
		// ctx.Err() (DeadlineExceeded / Canceled) takes precedence
		// over whatever exit code the SIGTERMed process surfaced.
		return runtime.ExecResult{}, ctx.Err()
	}

	if waitRaw == "" {
		return runtime.ExecResult{}, errors.New("applecontainer: bridge returned nil for ExecWait")
	}
	waitEnv, werr := decodeEnvelope[execWaitData](waitRaw)
	if werr != nil {
		return runtime.ExecResult{}, werr
	}

	result := runtime.ExecResult{
		ExitCode: int(waitEnv.decoded.ExitCode),
	}
	if opts.Stdout == nil {
		result.Stdout = stdoutBuf.String()
	}
	if opts.Stderr == nil {
		result.Stderr = stderrBuf.String()
	}
	return result, copyErr
}

// ---- pipe pair management ------------------------------------------

// execPipes holds the three pipe pairs we may need for an exec. Each
// entry is nil if that stream is unused (no stdin requested, or TTY
// mode for stderr). The "apiserver-facing" end is the one passed to
// the bridge and closed locally after start; the "Go-facing" end is
// the one we read/write.
type execPipes struct {
	stdinRead, stdinWrite   *os.File
	stdoutRead, stdoutWrite *os.File
	stderrRead, stderrWrite *os.File
}

func openExecPipes(opts runtime.ExecOptions) (*execPipes, error) {
	p := &execPipes{}
	cleanup := func() { p.closeLocal(); p.closeRemoteEnds() }

	if opts.Stdin != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("pipe(stdin): %w", err)
		}
		p.stdinRead, p.stdinWrite = r, w
	}

	// We always open stdout — capture or stream. The caller can't
	// opt out; suppression is up to the in-VM process.
	{
		r, w, err := os.Pipe()
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("pipe(stdout): %w", err)
		}
		p.stdoutRead, p.stdoutWrite = r, w
	}

	// Stderr is suppressed in TTY mode (Apple merges into stdout).
	if !opts.Tty {
		r, w, err := os.Pipe()
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("pipe(stderr): %w", err)
		}
		p.stderrRead, p.stderrWrite = r, w
	}

	return p, nil
}

func (p *execPipes) stdinReadFd() int   { return fdOrMinusOne(p.stdinRead) }
func (p *execPipes) stdoutWriteFd() int { return fdOrMinusOne(p.stdoutWrite) }
func (p *execPipes) stderrWriteFd() int { return fdOrMinusOne(p.stderrWrite) }

func (p *execPipes) stdinWriter() *os.File  { return p.stdinWrite }
func (p *execPipes) stdoutReader() *os.File { return p.stdoutRead }
func (p *execPipes) stderrReader() *os.File { return p.stderrRead }

// closeRemoteEnds closes the apiserver-facing ends. Called right
// after ac_exec_start succeeds, since XPC has dup'd those fds.
func (p *execPipes) closeRemoteEnds() {
	closeIf(p.stdinRead)
	p.stdinRead = nil
	closeIf(p.stdoutWrite)
	p.stdoutWrite = nil
	closeIf(p.stderrWrite)
	p.stderrWrite = nil
}

// closeLocal closes everything still open. Safe to call multiple
// times.
func (p *execPipes) closeLocal() {
	closeIf(p.stdinRead)
	closeIf(p.stdinWrite)
	closeIf(p.stdoutRead)
	closeIf(p.stdoutWrite)
	closeIf(p.stderrRead)
	closeIf(p.stderrWrite)
	p.stdinRead, p.stdinWrite = nil, nil
	p.stdoutRead, p.stdoutWrite = nil, nil
	p.stderrRead, p.stderrWrite = nil, nil
}

func closeIf(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

func fdOrMinusOne(f *os.File) int {
	if f == nil {
		return -1
	}
	return int(f.Fd())
}

func pickWriter(w io.Writer, fallback io.Writer) io.Writer {
	if w == nil {
		return fallback
	}
	return w
}

func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	// errors.Is matches syscall.EPIPE on the standard path; the
	// string fallback covers wrapped/wrapped-and-formatted variants
	// that some library boundaries produce.
	if errors.Is(err, syscall.EPIPE) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "file already closed")
}
