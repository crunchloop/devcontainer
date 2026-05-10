package devcontainer

import (
	"context"
	"sort"
	"strings"

	"github.com/crunchloop/devcontainer/config"
)

// collectSecretsEnv merges UpOptions.ExtraContainerEnv with any
// secretsCommand output and returns the result for use as
// `extraEnv` in buildRunSpec / compose overrides. Caller-supplied
// ExtraContainerEnv wins on key collision (explicit caller intent
// trumps spec hooks). Returns nil when neither source contributes.
//
// Returns (nil, *LifecycleError) if RunSecretsCommand was requested
// but the executor is missing or the host command failed.
func (e *Engine) collectSecretsEnv(ctx context.Context, cfg *config.ResolvedConfig, opts UpOptions) (map[string]string, error) {
	if !opts.RunSecretsCommand {
		return opts.ExtraContainerEnv, nil
	}
	secrets, err := e.runSecretsCommand(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if len(secrets) == 0 {
		return opts.ExtraContainerEnv, nil
	}
	if len(opts.ExtraContainerEnv) == 0 {
		return secrets, nil
	}
	out := make(map[string]string, len(secrets)+len(opts.ExtraContainerEnv))
	for k, v := range secrets {
		out[k] = v
	}
	for k, v := range opts.ExtraContainerEnv {
		out[k] = v
	}
	return out, nil
}

// runSecretsCommand executes the host-side secretsCommand via the
// engine's HostExecutor and returns its parsed key=value stdout as an
// env map. Returns (nil, nil) when no secretsCommand is configured.
//
// secretsCommand is conceptually parallel to initializeCommand:
// host-side, security-sensitive, opt-in. Where initializeCommand is
// fire-and-forget (exit code only), secretsCommand exists specifically
// to capture stdout and inject it into the container's environment
// before container start.
//
// On non-zero host exit we return a *LifecycleError so callers can
// surface stderr and the exit code via the same error shape used by
// initializeCommand failures. The phase tag is "secretsCommand"
// (synthetic — not a real LifecyclePhase, since it isn't an
// in-container phase and has no marker).
func (e *Engine) runSecretsCommand(ctx context.Context, cfg *config.ResolvedConfig) (map[string]string, error) {
	if cfg.SecretsCommand.IsEmpty() {
		return nil, nil
	}
	if e.opts.HostExecutor == nil {
		return nil, &LifecycleError{
			Phase: phaseSecretsCommand,
			Cause: ErrHostExecutorNotConfigured,
		}
	}
	if cfg.SecretsCommand.Single != nil {
		return e.execSecretsHost(ctx, cfg, *cfg.SecretsCommand.Single)
	}
	// Parallel form: run each named command (sequentially, in name
	// order) and merge results. Spec doesn't strictly bless parallel
	// for secretsCommand, but the command shape is shared with
	// initializeCommand so we accept it for symmetry. Duplicate keys
	// are resolved last-write-wins by name order (lexicographic,
	// matching execParallel's determinism).
	names := make([]string, 0, len(cfg.SecretsCommand.Parallel))
	for k := range cfg.SecretsCommand.Parallel {
		names = append(names, k)
	}
	sort.Strings(names)
	merged := map[string]string{}
	for _, name := range names {
		c := cfg.SecretsCommand.Parallel[name]
		env, err := e.execSecretsHost(ctx, cfg, c)
		if err != nil {
			return nil, err
		}
		for k, v := range env {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil, nil
	}
	return merged, nil
}

func (e *Engine) execSecretsHost(ctx context.Context, cfg *config.ResolvedConfig, c config.Command) (map[string]string, error) {
	res, err := e.opts.HostExecutor.ExecHost(ctx, HostCommand{
		Shell:      c.Shell,
		Exec:       c.Exec,
		WorkingDir: cfg.LocalWorkspaceFolder,
	})
	if err != nil {
		return nil, &LifecycleError{Phase: phaseSecretsCommand, Cause: err}
	}
	if res.ExitCode != 0 {
		return nil, &LifecycleError{
			Phase:    phaseSecretsCommand,
			ExitCode: res.ExitCode,
			Stdout:   res.Stdout,
			Stderr:   res.Stderr,
		}
	}
	return parseSecretsKV(res.Stdout), nil
}

// phaseSecretsCommand is the LifecycleError.Phase tag for
// secretsCommand failures. Not a real lifecycle phase — used only so
// callers can distinguish secretsCommand failures from
// initializeCommand failures via errors.As + Phase.
const phaseSecretsCommand config.LifecyclePhase = "secretsCommand"

// parseSecretsKV parses a stdout stream of `KEY=VALUE` lines into a
// map. Rules (intentionally narrow — secretsCommand stdout is
// program-generated, not human-edited):
//   - one entry per line
//   - blank lines and lines starting with `#` (after leading
//     whitespace) are ignored
//   - leading/trailing whitespace around the key is trimmed; values
//     are taken verbatim from after the first '=' (no quote
//     stripping, no escape processing)
//   - lines without `=`, or with an empty key after trimming, are
//     skipped silently
//
// Deliberately no quote stripping or `\n` escape interpretation:
// callers needing that should emit clean k=v from secretsCommand.
// Doing magic here would silently mangle values that contain a
// literal `"` or `\`.
func parseSecretsKV(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		i := strings.IndexByte(trimmed, '=')
		if i <= 0 {
			continue
		}
		key := strings.TrimRight(trimmed[:i], " \t")
		if key == "" {
			continue
		}
		out[key] = trimmed[i+1:]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
