package devcontainer

import (
	"strings"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

// WorkspaceID is a stable identifier for a workspace, derived from
// (LocalWorkspaceFolder, ConfigPath). See config.DevcontainerID.
type WorkspaceID string

// Workspace is a live devcontainer: a resolved config plus a running
// container plus a substituter bound to the container's effective env.
//
// Workspace is safe for concurrent reads (Exec/Inspect/Logs) but not for
// concurrent mutation. Engine.Attach returns a fresh Workspace; callers
// should not share Workspace values across re-attaches.
type Workspace struct {
	ID        WorkspaceID
	Config    *config.ResolvedConfig
	Container *runtime.ContainerDetails

	subst *Substituter

	// probedEnv holds the environment captured by running a login/
	// interactive shell inside the container (per cfg.UserEnvProbe).
	// Engine.Exec merges it under opts.Env so callers see PATH, NVM,
	// asdf-style additions injected by the user's rc files. Populated
	// after Up's lifecycle phase (so probes reflect any rc-modifying
	// postCreate scripts) and on Attach. nil means the probe hasn't
	// run yet or userEnvProbe was "none".
	probedEnv map[string]string
}

// Substituter resolves devcontainer.json substitution placeholders against
// a fully-populated SubstitutionContext, including the live container's
// environment. Use it to rewrite Exec / RunLifecycle inputs at call time
// without mutating the underlying ResolvedConfig.
type Substituter struct {
	ctx config.SubstitutionContext
}

// String resolves placeholders in s. Returned warnings are accumulated
// non-fatal diagnostics; callers may discard them or surface as events.
func (s *Substituter) String(in string) (string, []config.Warning) {
	return config.ResolveString(in, s.ctx)
}

// Slice applies String to each element in place and concatenates the
// warnings.
func (s *Substituter) Slice(in []string) ([]string, []config.Warning) {
	if len(in) == 0 {
		return in, nil
	}
	var allWarnings []config.Warning
	out := make([]string, len(in))
	for i, v := range in {
		resolved, w := s.String(v)
		out[i] = resolved
		allWarnings = append(allWarnings, w...)
	}
	return out, allWarnings
}

// Map applies String to each value and returns a new map. Keys are
// not substituted.
func (s *Substituter) Map(in map[string]string) (map[string]string, []config.Warning) {
	if len(in) == 0 {
		return in, nil
	}
	var allWarnings []config.Warning
	out := make(map[string]string, len(in))
	for k, v := range in {
		resolved, w := s.String(v)
		out[k] = resolved
		allWarnings = append(allWarnings, w...)
	}
	return out, allWarnings
}

func newSubstituter(cfg *config.ResolvedConfig, container *runtime.ContainerDetails, localEnv map[string]string) *Substituter {
	return &Substituter{
		ctx: config.SubstitutionContext{
			LocalWorkspaceFolder:     cfg.LocalWorkspaceFolder,
			ContainerWorkspaceFolder: cfg.ContainerWorkspaceFolder,
			DevcontainerID:           cfg.DevcontainerID,
			LocalEnv:                 localEnv,
			ContainerEnv:             envListToMap(container.Env),
		},
	}
}

func envListToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}
