// Package devcontainer is the top-level entry point for the devcontainer
// runtime library. The high-level engine API will land in subsequent
// milestones; this milestone exposes the configuration resolution surface.
package devcontainer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/crunchloop/devcontainer/config"
)

// ResolvedConfig is re-exported from the config package for caller
// convenience. See config.ResolvedConfig.
type ResolvedConfig = config.ResolvedConfig

// ResolveOptions controls how Resolve loads and resolves a devcontainer.json.
type ResolveOptions struct {
	// LocalWorkspaceFolder is the absolute host path containing the project.
	// Required.
	LocalWorkspaceFolder string

	// ConfigPath is the absolute path to the devcontainer.json file. If
	// empty, Resolve looks for:
	//   <LocalWorkspaceFolder>/.devcontainer/devcontainer.json
	//   <LocalWorkspaceFolder>/.devcontainer.json
	// in that order.
	ConfigPath string

	// LocalEnv overrides os.Environ() for ${localEnv:*} resolution.
	// Nil means use the current process environment.
	LocalEnv map[string]string

	// DevcontainerIDFunc lets callers customize the workspace id derivation.
	// Nil uses config.DevcontainerID(LocalWorkspaceFolder, ConfigPath).
	DevcontainerIDFunc func(localWorkspaceFolder, configPath string) string
}

// Resolve loads and resolves a devcontainer.json document.
//
// Image-label metadata merging and feature OCI resolution are stubbed in
// this milestone (see PRD §13). Build and compose source kinds are parsed
// and surfaced; actuating them requires the runtime layer landing in M2/M4.
func Resolve(ctx context.Context, opts ResolveOptions) (*ResolvedConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.LocalWorkspaceFolder == "" {
		return nil, errors.New("LocalWorkspaceFolder is required")
	}
	if !filepath.IsAbs(opts.LocalWorkspaceFolder) {
		return nil, fmt.Errorf("LocalWorkspaceFolder must be absolute: %q", opts.LocalWorkspaceFolder)
	}

	configPath := opts.ConfigPath
	if configPath == "" {
		p, err := lookupConfigPath(opts.LocalWorkspaceFolder)
		if err != nil {
			return nil, err
		}
		configPath = p
	}
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(opts.LocalWorkspaceFolder, configPath)
	}

	src, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", configPath, err)
	}

	idFn := opts.DevcontainerIDFunc
	if idFn == nil {
		idFn = config.DevcontainerID
	}

	localEnv := opts.LocalEnv
	if localEnv == nil {
		localEnv = environAsMap(os.Environ())
	}

	return config.ResolveBytes(src, config.ResolveInput{
		LocalWorkspaceFolder: opts.LocalWorkspaceFolder,
		ConfigPath:           configPath,
		DevcontainerID:       idFn(opts.LocalWorkspaceFolder, configPath),
		LocalEnv:             localEnv,
	})
}

func lookupConfigPath(workspaceFolder string) (string, error) {
	candidates := []string{
		filepath.Join(workspaceFolder, ".devcontainer", "devcontainer.json"),
		filepath.Join(workspaceFolder, ".devcontainer.json"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no devcontainer.json found under %s", workspaceFolder)
}

func environAsMap(env []string) map[string]string {
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
