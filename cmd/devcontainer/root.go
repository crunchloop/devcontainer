package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/applecontainer"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

type rootFlags struct {
	workspaceFolder string
	configPath      string
	runtimeName     string
	logLevel        string
}

func newRootCmd() *cobra.Command {
	f := &rootFlags{}

	cmd := &cobra.Command{
		Use:           "devcontainer",
		Short:         "Manage dev containers (containers.dev) from the command line",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pf := cmd.PersistentFlags()
	pf.StringVar(&f.workspaceFolder, "workspace-folder", "", "Path to the project workspace (defaults to current directory)")
	pf.StringVar(&f.configPath, "config", "", "Path to devcontainer.json (defaults to .devcontainer/devcontainer.json under the workspace)")
	pf.StringVar(&f.runtimeName, "runtime", "docker", "Container backend: docker | applecontainer")
	pf.StringVar(&f.logLevel, "log-level", "info", "Log verbosity: info | debug | trace")

	cmd.AddCommand(
		newUpCmd(f),
		newExecCmd(f),
		newDownCmd(f),
		newStopCmd(f),
		newReadConfigurationCmd(f),
		newRunUserCommandsCmd(f),
	)

	return cmd
}

// resolveWorkspaceFolder turns the --workspace-folder flag into an absolute
// path, defaulting to the current working directory when unset.
func (f *rootFlags) resolveWorkspaceFolder() (string, error) {
	ws := f.workspaceFolder
	if ws == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		ws = cwd
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	return abs, nil
}

// newRuntime constructs the configured container backend. The returned
// closer must be called once the runtime is no longer needed.
func (f *rootFlags) newRuntime(ctx context.Context) (runtime.Runtime, func(), error) {
	switch f.runtimeName {
	case "docker":
		rt, err := docker.New(ctx, docker.Options{})
		if err != nil {
			return nil, nil, fmt.Errorf("docker runtime: %w", err)
		}
		return rt, func() { _ = rt.Close() }, nil
	case "applecontainer":
		rt, err := applecontainer.New(ctx, applecontainer.Options{})
		if err != nil {
			return nil, nil, fmt.Errorf("applecontainer runtime: %w", err)
		}
		return rt, func() {}, nil
	default:
		return nil, nil, fmt.Errorf("unknown runtime %q (want docker | applecontainer)", f.runtimeName)
	}
}

// newEngine builds the engine wired to the configured backend.
func (f *rootFlags) newEngine(ctx context.Context) (*devcontainer.Engine, func(), error) {
	rt, closeRT, err := f.newRuntime(ctx)
	if err != nil {
		return nil, nil, err
	}
	eng, err := devcontainer.New(devcontainer.EngineOptions{Runtime: rt})
	if err != nil {
		closeRT()
		return nil, nil, fmt.Errorf("engine: %w", err)
	}
	return eng, closeRT, nil
}
