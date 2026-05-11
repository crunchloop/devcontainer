package devcontainer

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
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
// A phase may have multiple LifecycleCommand hooks (one per metadata
// layer that contributed); they run in order [base image label entries
// → each feature → user devcontainer.json] per spec. The marker covers
// the whole phase: any one hook's non-zero exit aborts and re-runs on
// next Up.
//
// User commands go through the workspace Substituter so ${containerEnv:*}
// resolves against the live container.
//
// Returns nil if the phase has no commands configured. Returns a
// *LifecycleError if any hook exited non-zero.
func (e *Engine) RunLifecycle(ctx context.Context, ws *Workspace, phase config.LifecyclePhase) error {
	if err := ctxIfDone(ctx); err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("Engine.RunLifecycle: Workspace is required")
	}
	cmds := lifecycleCommandsFor(ws.Config, phase)
	return e.runPhase(ctx, ws, phase, cmds, nil)
}

// runAllLifecycle invokes every configured phase in spec order. initialize
// is skipped unless runInitialize is true (caller opt-in via
// UpOptions.RunInitializeCommand). Stops at the first phase to return an
// error.
func (e *Engine) runAllLifecycle(ctx context.Context, ws *Workspace, runInitialize bool, bus *eventBus) error {
	for _, phase := range orderedPhases {
		if phase == config.LifecycleInitialize && !runInitialize {
			continue
		}
		cmds := lifecycleCommandsFor(ws.Config, phase)
		if err := e.runPhase(ctx, ws, phase, cmds, bus); err != nil {
			return err
		}
	}
	return nil
}

func lifecycleCommandsFor(cfg *config.ResolvedConfig, phase config.LifecyclePhase) []config.LifecycleCommand {
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
		return nil
	}
}

// runPhase is the core: marker check → exec each hook in order →
// marker write. No-op for empty hook list. Marker writes only happen on
// success (every hook exit=0); a failed phase will retry the whole list
// on next Up. We don't currently record per-hook progress within a
// phase — re-running is cheap enough for typical hooks (idempotent rc
// edits, package installs guarded by the script).
func (e *Engine) runPhase(ctx context.Context, ws *Workspace, phase config.LifecyclePhase, cmds []config.LifecycleCommand, bus *eventBus) error {
	if len(cmds) == 0 {
		return nil
	}

	// initialize runs on the host, not in the container, and is not
	// markered (caller opts in deliberately each invocation).
	if phase == config.LifecycleInitialize {
		for _, c := range cmds {
			bus.Emit(events.LifecycleStartEvent{Phase: string(phase), Command: lifecycleCommandDisplay(c)})
			start := time.Now()
			if err := e.runInitialize(ctx, ws, c); err != nil {
				bus.Emit(events.LifecycleCompletedEvent{
					Phase:      string(phase),
					ExitCode:   exitCodeFromErr(err),
					DurationMs: time.Since(start).Milliseconds(),
				})
				return err
			}
			bus.Emit(events.LifecycleCompletedEvent{Phase: string(phase), DurationMs: time.Since(start).Milliseconds()})
		}
		return nil
	}

	if phase != config.LifecyclePostAttach {
		m, err := e.readMarker(ctx, ws, phase)
		if err != nil {
			return fmt.Errorf("read marker for %s: %w", phase, err)
		}
		if !shouldRun(phase, m, ws.Container) {
			bus.Emit(events.LifecycleSkippedEvent{Phase: string(phase), Reason: "marker_present"})
			return nil
		}
	}

	start := time.Now()
	for _, c := range cmds {
		bus.Emit(events.LifecycleStartEvent{Phase: string(phase), Command: lifecycleCommandDisplay(c)})
		hookStart := time.Now()
		exitCode, stdout, stderr, err := e.execLifecycleCommand(ctx, ws, c)
		if err != nil {
			bus.Emit(events.LifecycleCompletedEvent{
				Phase:      string(phase),
				ExitCode:   -1,
				DurationMs: time.Since(hookStart).Milliseconds(),
			})
			return &LifecycleError{Phase: phase, Cause: err}
		}
		bus.Emit(events.LifecycleCompletedEvent{
			Phase:      string(phase),
			ExitCode:   exitCode,
			DurationMs: time.Since(hookStart).Milliseconds(),
		})
		if exitCode != 0 {
			return &LifecycleError{Phase: phase, ExitCode: exitCode, Stdout: stdout, Stderr: stderr}
		}
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
		ExitCode:     0,
	}); err != nil {
		return fmt.Errorf("write marker for %s: %w", phase, err)
	}
	return nil
}

// runInitialize executes a host-side initializeCommand via the
// caller-supplied HostExecutor. Returns ErrHostExecutorNotConfigured
// (wrapped in *LifecycleError) when the engine has no executor — this
// is the explicit "host execution requires opt-in" stance, not a
// silent skip, so consumers see the misconfiguration.
//
// Single and Parallel command forms are both routed through the
// executor; for Parallel, named entries run concurrently and the
// first non-zero exit aggregates per-name stderr (mirrors
// execParallel for in-container hooks).
func (e *Engine) runInitialize(ctx context.Context, ws *Workspace, cmd config.LifecycleCommand) error {
	if e.opts.HostExecutor == nil {
		return &LifecycleError{
			Phase: config.LifecycleInitialize,
			Cause: ErrHostExecutorNotConfigured,
		}
	}
	if cmd.Single != nil {
		return e.runInitializeSingle(ctx, ws, *cmd.Single)
	}
	if len(cmd.Parallel) > 0 {
		return e.runInitializeParallel(ctx, ws, cmd.Parallel)
	}
	return nil
}

func (e *Engine) runInitializeSingle(ctx context.Context, ws *Workspace, c config.Command) error {
	hc := HostCommand{
		Shell:      c.Shell,
		Exec:       c.Exec,
		WorkingDir: ws.Config.LocalWorkspaceFolder,
	}
	res, err := e.opts.HostExecutor.ExecHost(ctx, hc)
	if err != nil {
		return &LifecycleError{Phase: config.LifecycleInitialize, Cause: err}
	}
	if res.ExitCode != 0 {
		return &LifecycleError{
			Phase:    config.LifecycleInitialize,
			ExitCode: res.ExitCode,
			Stdout:   res.Stdout,
			Stderr:   res.Stderr,
		}
	}
	return nil
}

func (e *Engine) runInitializeParallel(ctx context.Context, ws *Workspace, parallel map[string]config.Command) error {
	names := make([]string, 0, len(parallel))
	for k := range parallel {
		names = append(names, k)
	}
	sort.Strings(names)

	type result struct {
		exit   int
		stdout string
		stderr string
		err    error
	}
	results := make([]result, len(names))

	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, c config.Command) {
			defer wg.Done()
			res, err := e.opts.HostExecutor.ExecHost(ctx, HostCommand{
				Shell:      c.Shell,
				Exec:       c.Exec,
				WorkingDir: ws.Config.LocalWorkspaceFolder,
			})
			if err != nil {
				results[i] = result{err: err}
				return
			}
			results[i] = result{exit: res.ExitCode, stdout: res.Stdout, stderr: res.Stderr}
		}(i, parallel[name])
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			return &LifecycleError{Phase: config.LifecycleInitialize, Cause: r.err}
		}
	}
	for _, r := range results {
		if r.exit != 0 {
			return &LifecycleError{
				Phase:    config.LifecycleInitialize,
				ExitCode: r.exit,
				Stdout:   r.stdout,
				Stderr:   r.stderr,
			}
		}
	}
	return nil
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
		firstExit               int
		aggOut, aggErr          string
		firstNonZeroSet, anyErr bool
		firstErr                error
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

// lifecycleCommandDisplay returns a human-readable rendering of c for the
// LifecycleStartEvent.Command field. Single shell → the shell string;
// single exec → space-joined argv; parallel → "<name1>,<name2>,..."
// (deterministic by sort order).
func lifecycleCommandDisplay(c config.LifecycleCommand) string {
	if c.Single != nil {
		if c.Single.Shell != "" {
			return c.Single.Shell
		}
		if len(c.Single.Exec) > 0 {
			out := ""
			for i, s := range c.Single.Exec {
				if i > 0 {
					out += " "
				}
				out += s
			}
			return out
		}
	}
	if len(c.Parallel) > 0 {
		names := make([]string, 0, len(c.Parallel))
		for k := range c.Parallel {
			names = append(names, k)
		}
		sort.Strings(names)
		out := ""
		for i, n := range names {
			if i > 0 {
				out += ","
			}
			out += n
		}
		return out
	}
	return ""
}

// exitCodeFromErr extracts a *LifecycleError's ExitCode, falling back to
// -1 when err is not a LifecycleError or carries no exit code.
func exitCodeFromErr(err error) int {
	var le *LifecycleError
	if err != nil {
		if le2, ok := err.(*LifecycleError); ok {
			le = le2
		}
	}
	if le != nil && le.ExitCode != 0 {
		return le.ExitCode
	}
	return -1
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
