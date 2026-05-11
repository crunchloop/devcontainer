package devcontainer

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/runtime"
)

// ExecOptions configures Engine.Exec. Cmd, Env, User, and WorkingDir all
// pass through Workspace.Substituter so ${containerEnv:*} placeholders
// resolve against the live container.
type ExecOptions struct {
	Cmd        []string
	Env        map[string]string
	User       string
	WorkingDir string
	Tty        bool
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer

	// SkipUserEnvProbe, when true, makes Exec bypass the merge of
	// probedEnv AND cfg.RemoteEnv into the process environment.
	// Only opts.Env (after substitution) plus whatever the runtime
	// inherits from the container reaches the exec'd process.
	//
	// Default false: every Exec inherits both probedEnv (so PATH and
	// other vars set by the user's rc files are visible — nvm/asdf/
	// feature-installed tools just work) and RemoteEnv (the
	// devcontainer.json author's declared env).
	//
	// Set this for callers that need a clean, deterministic
	// environment — internal probes, low-level fs operations,
	// DiscoverPath-style helpers that read raw container env.
	SkipUserEnvProbe bool

	// EmitEvents, when true and Events is non-nil, sends ExecStartEvent
	// and ExecCompletedEvent for this call. Default false: hot-loop
	// callers (readiness probes that run hundreds of execs per minute)
	// would otherwise drown the events channel.
	EmitEvents bool

	// Events optionally receives ExecStartEvent / ExecCompletedEvent for
	// this call. Used only when EmitEvents is true. See package events
	// (experimental until v1.0.0).
	//
	// Ownership: the caller owns the channel. The engine only writes —
	// it never closes the channel. The caller MUST NOT close it before
	// Exec returns.
	Events chan<- events.Event
}

// ExecResult is the outcome of Engine.Exec.
type ExecResult struct {
	ExitCode int
	Stdout   string // populated only if ExecOptions.Stdout was nil
	Stderr   string // populated only if ExecOptions.Stderr was nil
}

// Exec runs a command inside the workspace's container. Strings in
// opts.Cmd, opts.Env values, opts.User, and opts.WorkingDir are
// substituted against the live container's environment before being
// handed to the runtime. ${containerEnv:VAR} resolves to the container's
// actual value; missing entries substitute to empty string with a
// (discarded) warning, matching VS Code semantics.
func (e *Engine) Exec(ctx context.Context, ws *Workspace, opts ExecOptions) (ExecResult, error) {
	if err := ctxIfDone(ctx); err != nil {
		return ExecResult{}, err
	}
	if ws == nil {
		return ExecResult{}, fmt.Errorf("Engine.Exec: Workspace is required")
	}

	cmd, _ := ws.subst.Slice(opts.Cmd)
	env, _ := ws.subst.Map(opts.Env)
	user, _ := ws.subst.String(opts.User)
	wd, _ := ws.subst.String(opts.WorkingDir)

	// Fall back to the spec's effective user (remoteUser → containerUser)
	// when callers leave User unset — matches lifecycle.go and
	// userenvprobe.go, and is required for image-metadata-baked
	// remoteUser to take effect on Exec callers that don't pass User
	// explicitly. Caller-supplied opts.User (substitution-aware) still
	// wins; this is a *fallback*, not an override.
	if user == "" {
		user = effectiveUser(ws.Config)
	}

	if !opts.SkipUserEnvProbe {
		remoteEnv, _ := ws.subst.Map(ws.Config.RemoteEnv)
		env = mergeProbedEnv(ws.probedEnv, remoteEnv, env, effectiveUser(ws.Config))
	}

	var bus *eventBus
	if opts.EmitEvents && opts.Events != nil {
		bus = newEventBus(e.emitter, opts.Events)
		defer bus.Close()
		bus.Emit(events.ExecStartEvent{ContainerID: ws.Container.ID, Cmd: cmd})
	}
	start := time.Now()

	res, err := e.runtime.ExecContainer(ctx, ws.Container.ID, runtime.ExecOptions{
		Cmd:        cmd,
		Env:        env,
		User:       user,
		WorkingDir: wd,
		Tty:        opts.Tty,
		Stdin:      opts.Stdin,
		Stdout:     opts.Stdout,
		Stderr:     opts.Stderr,
	})
	if err != nil {
		bus.Emit(events.ExecCompletedEvent{
			ContainerID: ws.Container.ID,
			ExitCode:    -1,
			DurationMs:  time.Since(start).Milliseconds(),
		})
		return ExecResult{}, err
	}
	bus.Emit(events.ExecCompletedEvent{
		ContainerID: ws.Container.ID,
		ExitCode:    res.ExitCode,
		DurationMs:  time.Since(start).Milliseconds(),
	})
	return ExecResult{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
	}, nil
}

// ExecByID is a convenience wrapper around Attach + Exec for callers that
// hold only a WorkspaceID. Hot-loop callers that hold a *Workspace from
// Up should call Exec directly to avoid re-inspecting the container on
// every invocation.
func (e *Engine) ExecByID(ctx context.Context, id WorkspaceID, opts ExecOptions) (ExecResult, error) {
	ws, err := e.Attach(ctx, id)
	if err != nil {
		return ExecResult{}, err
	}
	return e.Exec(ctx, ws, opts)
}
