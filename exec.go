package devcontainer

import (
	"context"
	"fmt"
	"io"

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

	// SkipUserEnvProbe, when true, suppresses injection of the
	// workspace's probed shell environment and devcontainer.json
	// remoteEnv into this exec. Default false: every Exec inherits
	// PATH and other vars set by the user's rc files (so callers
	// see nvm/asdf/feature-installed tools without shell-wrapping
	// per-call).
	//
	// Set this for callers that want a clean, deterministic
	// environment — internal probes, low-level fs operations,
	// callers reading raw container env. Lifecycle scripts run
	// before the probe, so for them the field is functionally a
	// no-op (probedEnv is empty); RemoteEnv still merges, which
	// matches devcontainer.json semantics.
	SkipUserEnvProbe bool
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

	if !opts.SkipUserEnvProbe {
		remoteEnv, _ := ws.subst.Map(ws.Config.RemoteEnv)
		env = mergeProbedEnv(ws.probedEnv, remoteEnv, env, effectiveUser(ws.Config))
	}

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
		return ExecResult{}, err
	}
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
