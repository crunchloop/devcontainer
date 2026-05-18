package docker

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/crunchloop/devcontainer/runtime"
)

// composeCmd is the binary we exec to drive Docker Compose. We require
// v2 (the `docker compose` subcommand of the docker CLI), per PRD
// §12.3 — legacy `docker-compose` is not supported.
const (
	composeBin    = "docker"
	composeSubCmd = "compose"
)

// composeProbeOnce wraps the lazy version probe so we run it at most
// once per Runtime regardless of how many compose calls happen.
type composeState struct {
	once sync.Once
	err  error
}

// probeCompose checks `docker compose version` and caches the result.
// The first compose call (Up/Down/ContainerID) triggers the probe; if
// it fails, all subsequent calls short-circuit with the same error.
func (r *Runtime) probeCompose(ctx context.Context) error {
	r.composeState.once.Do(func() {
		cmd := exec.CommandContext(ctx, composeBin, composeSubCmd, "version")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			r.composeState.err = &runtime.ComposeUnavailableError{
				Err: fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String())),
			}
		}
	})
	return r.composeState.err
}

// composeReady is set on a successful probe. Some hot paths (e.g. a
// caller invoking ComposeUp + ComposeContainerID + ComposeDown in a
// row) don't need the per-call once.Do bookkeeping after the first
// success.
//
//nolint:unused // kept for future fast-path
var _ = (*Runtime)(nil)

// composeArgs builds the leading args common to every compose command:
// `compose -p <project> -f file1 -f file2 ...`. Returns a fresh slice
// so caller-side appends don't alias.
func composeArgs(projectName string, files []string) []string {
	out := make([]string, 0, 3+2*len(files))
	out = append(out, composeSubCmd)
	if projectName != "" {
		out = append(out, "--project-name", projectName)
	}
	for _, f := range files {
		out = append(out, "-f", f)
	}
	return out
}

// buildUpArgs renders the full argv for a `docker compose up -d` call.
// We do not pass `--no-build`: compose's default is "rebuild only when
// service config changed", and our generated dc-build.yaml override
// pins the primary service's image to a tag we already built so
// compose doesn't rebuild it. Devpod takes the same approach.
func buildUpArgs(spec runtime.ComposeUpSpec) []string {
	args := composeArgs(spec.ProjectName, spec.Files)
	args = append(args, "up", "-d")
	if spec.NoRecreate {
		args = append(args, "--no-recreate")
	}
	args = append(args, spec.Services...)
	return args
}

func buildDownArgs(spec runtime.ComposeDownSpec) []string {
	args := composeArgs(spec.ProjectName, spec.Files)
	args = append(args, "down")
	if spec.RemoveImages {
		args = append(args, "--rmi", "local")
	}
	if spec.RemoveVolumes {
		args = append(args, "--volumes")
	}
	return args
}

func buildPsArgs(spec runtime.ComposePsSpec, service string) []string {
	args := composeArgs(spec.ProjectName, spec.Files)
	args = append(args, "ps", "-q", service)
	return args
}

// ComposeUp brings the project up in detached mode.
func (r *Runtime) ComposeUp(ctx context.Context, spec runtime.ComposeUpSpec, events chan<- runtime.BuildEvent) error {
	if err := r.probeCompose(ctx); err != nil {
		return err
	}
	args := buildUpArgs(spec)
	return r.runCompose(ctx, args, spec.WorkingDir, events)
}

// ComposeDown stops and (optionally) cleans the project.
func (r *Runtime) ComposeDown(ctx context.Context, spec runtime.ComposeDownSpec) error {
	if err := r.probeCompose(ctx); err != nil {
		return err
	}
	args := buildDownArgs(spec)
	return r.runCompose(ctx, args, spec.WorkingDir, nil)
}

// ComposeContainerID resolves the container id for a service via
// `docker compose ps -q <service>`. Returns "" if the service isn't
// running (compose returns empty stdout, exit 0).
func (r *Runtime) ComposeContainerID(ctx context.Context, spec runtime.ComposePsSpec, service string) (string, error) {
	if err := r.probeCompose(ctx); err != nil {
		return "", err
	}
	args := buildPsArgs(spec, service)
	cmd := exec.CommandContext(ctx, composeBin, args...)
	if spec.WorkingDir != "" {
		cmd.Dir = spec.WorkingDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", composeFailedFromExitErr(args, err, &stderr)
	}
	// `compose ps -q` returns one ID per line for matching containers.
	// We only ask for one service, so a single ID at most.
	id := strings.TrimSpace(stdout.String())
	if i := strings.IndexByte(id, '\n'); i >= 0 {
		id = id[:i]
	}
	return id, nil
}

// runCompose executes the compose subcommand and streams stdout/stderr
// lines as BuildEvents (best-effort, drop-on-full). Non-zero exit
// becomes a *ComposeFailedError carrying captured stderr.
func (r *Runtime) runCompose(ctx context.Context, args []string, workingDir string, events chan<- runtime.BuildEvent) error {
	cmd := exec.CommandContext(ctx, composeBin, args...)
	if workingDir != "" {
		cmd.Dir = workingDir
	}

	// Capture stderr in full for error reporting; tee stdout into events.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("compose stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("compose start: %w", err)
	}

	// Stream stdout lines to events while the process runs.
	doneStdout := make(chan struct{})
	var emitted atomic.Int32
	go func() {
		defer close(doneStdout)
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 1024)
		for {
			n, rerr := stdoutPipe.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				for {
					nl := bytes.IndexByte(buf, '\n')
					if nl < 0 {
						break
					}
					line := strings.TrimRight(string(buf[:nl]), "\r")
					buf = buf[nl+1:]
					emitComposeEvent(events, line)
					emitted.Add(1)
				}
			}
			if rerr != nil {
				break
			}
		}
		if len(buf) > 0 {
			emitComposeEvent(events, strings.TrimRight(string(buf), "\r"))
		}
	}()

	waitErr := cmd.Wait()
	<-doneStdout

	if waitErr != nil {
		return composeFailedFromExitErr(args, waitErr, &stderr)
	}
	return nil
}

func emitComposeEvent(events chan<- runtime.BuildEvent, line string) {
	if events == nil || line == "" {
		return
	}
	select {
	case events <- runtime.BuildEvent{Kind: runtime.BuildEventLog, Message: line}:
	default:
	}
}

func composeFailedFromExitErr(args []string, err error, stderr *bytes.Buffer) error {
	exit := -1
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	}
	return &runtime.ComposeFailedError{
		Args:     append([]string(nil), args...),
		ExitCode: exit,
		Stderr:   strings.TrimSpace(stderr.String()),
	}
}
