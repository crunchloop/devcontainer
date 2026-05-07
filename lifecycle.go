package devcontainer

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

// orderedPhases is the spec-defined execution order. initialize is host-
// side and only runs when the caller opts in; the rest run inside the
// container.
var orderedPhases = []config.LifecyclePhase{
	config.LifecycleInitialize,
	config.LifecycleOnCreate,
	config.LifecycleUpdateContent,
	config.LifecyclePostCreate,
	config.LifecyclePostStart,
	config.LifecyclePostAttach,
}

// RunLifecycle executes a single named lifecycle phase against the
// workspace, applying idempotency markers. Phases run in-container
// except initialize, which runs on the host.
//
// The user's command goes through the workspace Substituter so
// ${containerEnv:*} resolves against the live container.
//
// Returns (nil, nil) if the phase has no command configured. Returns a
// *LifecycleError if the user command exited non-zero.
func (e *Engine) RunLifecycle(ctx context.Context, ws *Workspace, phase config.LifecyclePhase) error {
	if err := ctxIfDone(ctx); err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("Engine.RunLifecycle: Workspace is required")
	}
	cmd := lifecycleCommandFor(ws.Config, phase)
	return e.runPhase(ctx, ws, phase, cmd)
}

// runAllLifecycle invokes every configured phase in spec order. initialize
// is skipped unless runInitialize is true (caller opt-in via
// UpOptions.RunInitializeCommand). Stops at the first phase to return an
// error.
func (e *Engine) runAllLifecycle(ctx context.Context, ws *Workspace, runInitialize bool) error {
	for _, phase := range orderedPhases {
		if phase == config.LifecycleInitialize && !runInitialize {
			continue
		}
		cmd := lifecycleCommandFor(ws.Config, phase)
		if err := e.runPhase(ctx, ws, phase, cmd); err != nil {
			return err
		}
	}
	return nil
}

func lifecycleCommandFor(cfg *config.ResolvedConfig, phase config.LifecyclePhase) config.LifecycleCommand {
	switch phase {
	case config.LifecycleInitialize:
		return cfg.Lifecycle.Initialize
	case config.LifecycleOnCreate:
		return cfg.Lifecycle.OnCreate
	case config.LifecycleUpdateContent:
		return cfg.Lifecycle.UpdateContent
	case config.LifecyclePostCreate:
		return cfg.Lifecycle.PostCreate
	case config.LifecyclePostStart:
		return cfg.Lifecycle.PostStart
	case config.LifecyclePostAttach:
		return cfg.Lifecycle.PostAttach
	default:
		return config.LifecycleCommand{}
	}
}

// runPhase is the core: marker check → exec → marker write. No-op for
// empty commands. Marker writes only happen on success (exit=0); a
// failed phase will retry on next Up.
func (e *Engine) runPhase(ctx context.Context, ws *Workspace, phase config.LifecyclePhase, cmd config.LifecycleCommand) error {
	if cmd.IsEmpty() {
		return nil
	}

	// initialize runs on the host, not in the container, and is not
	// markered (caller opts in deliberately each invocation).
	if phase == config.LifecycleInitialize {
		return e.runInitialize(ctx, ws, cmd)
	}

	if phase != config.LifecyclePostAttach {
		m, err := e.readMarker(ctx, ws, phase)
		if err != nil {
			return fmt.Errorf("read marker for %s: %w", phase, err)
		}
		if !shouldRun(phase, m, ws.Container) {
			return nil
		}
	}

	start := time.Now()
	exitCode, stdout, stderr, err := e.execLifecycleCommand(ctx, ws, cmd)
	if err != nil {
		return &LifecycleError{Phase: phase, Cause: err}
	}
	if exitCode != 0 {
		return &LifecycleError{Phase: phase, ExitCode: exitCode, Stdout: stdout, Stderr: stderr}
	}

	if phase == config.LifecyclePostAttach {
		// No marker.
		return nil
	}

	if err := e.writeMarker(ctx, ws, marker{
		Version:      markerVersion,
		Phase:        string(phase),
		KeyTimestamp: keyTimestampFor(phase, ws.Container),
		RanAt:        start,
		DurationMs:   time.Since(start).Milliseconds(),
		ExitCode:     exitCode,
	}); err != nil {
		return fmt.Errorf("write marker for %s: %w", phase, err)
	}
	return nil
}

// runInitialize executes a host-side initializeCommand. Currently only
// the Single form is supported; parallel-named initialize commands fall
// through with a TODO. We intentionally do NOT pass the host environment
// or working directory verbatim — caller is responsible for providing a
// safe context via UpOptions if needed.
func (e *Engine) runInitialize(ctx context.Context, ws *Workspace, cmd config.LifecycleCommand) error {
	// Host-side execution is deliberately minimal in v1 — we don't shell
	// out from inside the engine at all to avoid making the library look
	// like a host CLI. A future opt-in HostExecutor field on
	// EngineOptions can take this on; for now, log a warning and skip.
	return &LifecycleError{
		Phase: config.LifecycleInitialize,
		Cause: fmt.Errorf("initializeCommand not supported in v1 (host-side execution requires explicit caller wiring)"),
	}
}

// execLifecycleCommand routes a single LifecycleCommand through
// Engine.Exec, supporting both Single (shell or exec) and Parallel
// (named, run concurrently) forms. Returns aggregate exit code: 0 if
// all succeeded, otherwise the first non-zero exit observed.
func (e *Engine) execLifecycleCommand(ctx context.Context, ws *Workspace, cmd config.LifecycleCommand) (int, string, string, error) {
	if cmd.Single != nil {
		return e.execCommand(ctx, ws, *cmd.Single)
	}
	if len(cmd.Parallel) > 0 {
		return e.execParallel(ctx, ws, cmd.Parallel)
	}
	return 0, "", "", nil
}

func (e *Engine) execCommand(ctx context.Context, ws *Workspace, c config.Command) (int, string, string, error) {
	opts := ExecOptions{
		WorkingDir: ws.Config.ContainerWorkspaceFolder,
		User:       effectiveUser(ws.Config),
	}
	if c.Shell != "" {
		opts.Cmd = []string{"sh", "-c", c.Shell}
	} else {
		opts.Cmd = c.Exec
	}
	res, err := e.Exec(ctx, ws, opts)
	if err != nil {
		return 0, "", "", err
	}
	return res.ExitCode, res.Stdout, res.Stderr, nil
}

// execParallel runs every named command concurrently. Returns the first
// non-zero exit code observed (deterministic by sorting names) and
// concatenated stdout/stderr from all named commands.
func (e *Engine) execParallel(ctx context.Context, ws *Workspace, parallel map[string]config.Command) (int, string, string, error) {
	names := make([]string, 0, len(parallel))
	for k := range parallel {
		names = append(names, k)
	}
	sort.Strings(names)

	type res struct {
		name           string
		exitCode       int
		stdout, stderr string
		err            error
	}
	results := make([]res, len(names))

	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string, c config.Command) {
			defer wg.Done()
			ec, out, errOut, err := e.execCommand(ctx, ws, c)
			results[i] = res{name: name, exitCode: ec, stdout: out, stderr: errOut, err: err}
		}(i, name, parallel[name])
	}
	wg.Wait()

	var (
		firstExit             int
		aggOut, aggErr        string
		firstNonZeroSet, anyErr bool
		firstErr              error
	)
	for _, r := range results {
		if r.err != nil && !anyErr {
			firstErr = r.err
			anyErr = true
		}
		if r.exitCode != 0 && !firstNonZeroSet {
			firstExit = r.exitCode
			firstNonZeroSet = true
		}
		if r.stdout != "" {
			aggOut += "[" + r.name + "]\n" + r.stdout + "\n"
		}
		if r.stderr != "" {
			aggErr += "[" + r.name + "]\n" + r.stderr + "\n"
		}
	}
	if anyErr {
		return 0, aggOut, aggErr, firstErr
	}
	return firstExit, aggOut, aggErr, nil
}

// effectiveUser is the user lifecycle commands run as: remoteUser,
// falling back to containerUser, falling back to image default ("").
func effectiveUser(cfg *config.ResolvedConfig) string {
	if cfg.RemoteUser != "" {
		return cfg.RemoteUser
	}
	return cfg.ContainerUser
}

// Verify referenced symbols compile by accessing them.
var _ = runtime.ErrNotImplemented
