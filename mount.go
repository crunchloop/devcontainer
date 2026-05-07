package devcontainer

import (
	"runtime"

	"github.com/crunchloop/devcontainer/config"
	rt "github.com/crunchloop/devcontainer/runtime"
)

// defaultWorkspaceMount returns the workspace bind mount: a user-specified
// cfg.WorkspaceMount if non-nil, otherwise a bind from
// LocalWorkspaceFolder to ContainerWorkspaceFolder.
//
// On non-Linux hosts (macOS, Windows) the bind carries "consistent"
// propagation, matching devpod and the broader devcontainer ecosystem
// (no-op on modern Docker Desktop with VirtioFS, but kept for least-
// surprise on older daemons).
func defaultWorkspaceMount(cfg *config.ResolvedConfig) rt.MountSpec {
	if cfg.WorkspaceMount != nil {
		return rt.MountSpec{
			Type:     rt.MountType(cfg.WorkspaceMount.Type),
			Source:   cfg.WorkspaceMount.Source,
			Target:   cfg.WorkspaceMount.Target,
			ReadOnly: cfg.WorkspaceMount.ReadOnly,
		}
	}
	m := rt.MountSpec{
		Type:   rt.MountBind,
		Source: cfg.LocalWorkspaceFolder,
		Target: cfg.ContainerWorkspaceFolder,
	}
	if !isLinux() {
		m.Propagation = "consistent"
	}
	return m
}

// isLinux is a thin wrapper that lets tests override host OS detection.
// The standard library's runtime.GOOS is a build-time constant; this
// function is package-private so tests can swap it via go:linkname-style
// patterns if ever needed. For now it just returns the constant.
func isLinux() bool {
	return runtime.GOOS == "linux"
}

