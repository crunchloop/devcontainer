package devcontainer

import (
	"context"
	"errors"
)

// HostExecutor runs commands on the host. Callers supply one via
// EngineOptions.HostExecutor to enable host-side spec hooks like
// initializeCommand (and, in a follow-up, secretsCommand). The
// library does NOT ship a default implementation: host execution is
// security-sensitive (devcontainer.json can declare arbitrary
// commands), and the policy decisions — sandboxing, env filtering,
// timeouts, max output, working directory — belong to the embedding
// application.
//
// When EngineOptions.HostExecutor is nil and a hook would run, the
// engine returns a *LifecycleError wrapping ErrHostExecutorNotConfigured.
// Callers can detect this via errors.Is to surface a useful message
// ("set EngineOptions.HostExecutor to enable initializeCommand") or
// to skip silently in environments where host execution isn't
// permitted.
type HostExecutor interface {
	// ExecHost runs a host-side command. The executor is responsible
	// for the shell / exec dispatch (HostCommand.Shell vs
	// HostCommand.Exec — exactly one is set), environment merging,
	// and working-directory selection. Cancellation via ctx must
	// propagate to the spawned process.
	//
	// Return a non-nil error only for executor-internal failures
	// (process couldn't start, I/O error). A non-zero command exit
	// is reported via HostExecResult.ExitCode with a nil error so
	// the engine can wrap it consistently with container-side
	// execution.
	ExecHost(ctx context.Context, cmd HostCommand) (HostExecResult, error)
}

// HostCommand is the input to HostExecutor.ExecHost. Shape mirrors
// runtime.ExecOptions / config.Command so callers building both can
// reuse mental model.
type HostCommand struct {
	// Shell is a single shell command line (`sh -c <Shell>` style).
	// Mutually exclusive with Exec.
	Shell string

	// Exec is a literal argv invocation (no shell). Mutually
	// exclusive with Shell.
	Exec []string

	// Env is merged on top of the host process environment.
	// Implementations decide whether to filter the inherited host
	// env (e.g. drop secrets) or pass it through.
	Env map[string]string

	// WorkingDir is the host directory to run in. Empty leaves the
	// choice to the executor; the engine populates this with the
	// workspace's LocalWorkspaceFolder for spec hooks.
	WorkingDir string
}

// HostExecResult is the outcome of HostExecutor.ExecHost.
type HostExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// ErrHostExecutorNotConfigured is returned (wrapped in *LifecycleError)
// when a host-side hook is configured in devcontainer.json but the
// engine has no HostExecutor to dispatch it. Callers wanting to
// detect this specifically use errors.Is.
var ErrHostExecutorNotConfigured = errors.New("host executor not configured (set EngineOptions.HostExecutor)")
