package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/moby/moby/client"

	"github.com/crunchloop/devcontainer/runtime"
)

func (r *Runtime) ExecContainer(ctx context.Context, id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
	var consoleSize client.ConsoleSize
	if opts.Tty && opts.InitialTtySize.Width != 0 && opts.InitialTtySize.Height != 0 {
		consoleSize = client.ConsoleSize{
			Height: uint(opts.InitialTtySize.Height),
			Width:  uint(opts.InitialTtySize.Width),
		}
	}

	createRes, err := r.api.ExecCreate(ctx, id, client.ExecCreateOptions{
		User:         opts.User,
		WorkingDir:   opts.WorkingDir,
		Env:          envMapToList(opts.Env),
		Cmd:          opts.Cmd,
		AttachStdin:  opts.Stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
		TTY:          opts.Tty,
		ConsoleSize:  consoleSize,
	})
	if err != nil {
		if isContainerNotFound(err) {
			return runtime.ExecResult{}, &runtime.ContainerNotFoundError{ID: id, Err: err}
		}
		return runtime.ExecResult{}, fmt.Errorf("ExecCreate: %w", err)
	}

	attachRes, err := r.api.ExecAttach(ctx, createRes.ID, client.ExecAttachOptions{TTY: opts.Tty})
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("ExecAttach: %w", err)
	}
	defer attachRes.Close()

	// Forward pty resize events for the lifetime of this exec. The
	// goroutine exits when the caller stops sending and the parent
	// context is cancelled (which we always do via the deferred
	// cancel below, even on the happy path).
	if opts.Tty && opts.ResizeCh != nil {
		resizeCtx, cancelResize := context.WithCancel(ctx)
		defer cancelResize()
		go forwardExecResize(resizeCtx, r.api, createRes.ID, opts.ResizeCh)
	}

	// Pipe stdin in the background if provided. We close the write half
	// when the caller-supplied reader returns EOF so the daemon sees a
	// closed stdin and the exec can finish naturally.
	if opts.Stdin != nil {
		go func() {
			_, _ = io.Copy(attachRes.Conn, opts.Stdin)
			_ = attachRes.CloseWrite()
		}()
	}

	var (
		outBuf, errBuf bytes.Buffer
		outDst         = opts.Stdout
		errDst         = opts.Stderr
	)
	if outDst == nil {
		outDst = &outBuf
	}
	if errDst == nil {
		errDst = &errBuf
	}

	if opts.Tty {
		// TTY mode: no framing — single multiplexed stream goes to outDst.
		copyDone := make(chan error, 1)
		go func() { _, err := io.Copy(outDst, attachRes.Reader); copyDone <- err }()
		select {
		case err := <-copyDone:
			if err != nil && err != io.EOF {
				return runtime.ExecResult{}, fmt.Errorf("exec read: %w", err)
			}
		case <-ctx.Done():
			return runtime.ExecResult{}, ctx.Err()
		}
	} else {
		copyDone := make(chan error, 1)
		go func() { copyDone <- stdcopy(outDst, errDst, attachRes.Reader) }()
		select {
		case err := <-copyDone:
			if err != nil {
				return runtime.ExecResult{}, fmt.Errorf("exec stdcopy: %w", err)
			}
		case <-ctx.Done():
			return runtime.ExecResult{}, ctx.Err()
		}
	}

	inspect, err := r.api.ExecInspect(ctx, createRes.ID, client.ExecInspectOptions{})
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("ExecInspect: %w", err)
	}

	res := runtime.ExecResult{ExitCode: inspect.ExitCode}
	if opts.Stdout == nil {
		res.Stdout = outBuf.String()
	}
	if opts.Stderr == nil {
		res.Stderr = errBuf.String()
	}
	return res, nil
}

// forwardExecResize drains size updates and pushes them to the daemon
// for the given exec id. Coalescing isn't strictly needed (resize events
// are sparse compared to the speed of the API call), but we use a
// best-effort drain to avoid backing up if the user resizes furiously.
func forwardExecResize(ctx context.Context, api *client.Client, execID string, ch <-chan runtime.TtySize) {
	for {
		select {
		case <-ctx.Done():
			return
		case sz, ok := <-ch:
			if !ok {
				return
			}
			// Drain any pending updates and keep only the latest.
			drained := true
			for drained {
				select {
				case next, ok := <-ch:
					if !ok {
						return
					}
					sz = next
				default:
					drained = false
				}
			}
			if sz.Width == 0 || sz.Height == 0 {
				continue
			}
			_, _ = api.ExecResize(ctx, execID, client.ExecResizeOptions{
				Height: uint(sz.Height),
				Width:  uint(sz.Width),
			})
		}
	}
}
