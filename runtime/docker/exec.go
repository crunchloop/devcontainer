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
	createRes, err := r.api.ExecCreate(ctx, id, client.ExecCreateOptions{
		User:         opts.User,
		WorkingDir:   opts.WorkingDir,
		Env:          envMapToList(opts.Env),
		Cmd:          opts.Cmd,
		AttachStdin:  opts.Stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
		TTY:          opts.Tty,
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
