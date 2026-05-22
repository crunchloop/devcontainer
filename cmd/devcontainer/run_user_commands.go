package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/config"
)

// Phases that run-user-commands iterates over by default. initialize is
// host-side and opt-in, mirroring Up's behavior.
var defaultLifecyclePhases = []config.LifecyclePhase{
	config.LifecycleOnCreate,
	config.LifecycleUpdateContent,
	config.LifecyclePostCreate,
	config.LifecyclePostStart,
	config.LifecyclePostAttach,
}

func newRunUserCommandsCmd(rf *rootFlags) *cobra.Command {
	var phaseFlag string

	cmd := &cobra.Command{
		Use:   "run-user-commands",
		Short: "Run devcontainer.json lifecycle commands against the running container",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			ws, err := rf.resolveWorkspaceFolder()
			if err != nil {
				return err
			}

			// Resolve again here (rather than relying on Attach's
			// minimal config) so we get the full lifecycle hooks
			// straight from devcontainer.json.
			cfg, err := devcontainer.Resolve(ctx, devcontainer.ResolveOptions{
				LocalWorkspaceFolder: ws,
				ConfigPath:           rf.configPath,
			})
			if err != nil {
				return err
			}

			eng, closeEng, err := rf.newEngine(ctx)
			if err != nil {
				return err
			}
			defer closeEng()

			workspace, err := eng.Attach(ctx, devcontainer.WorkspaceID(cfg.DevcontainerID))
			if err != nil {
				return err
			}
			// Replace Attach's minimal config with the freshly
			// resolved one so RunLifecycle sees declared hooks.
			workspace.Config = cfg

			phases := defaultLifecyclePhases
			if phaseFlag != "" {
				p := config.LifecyclePhase(phaseFlag)
				if !isKnownPhase(p) {
					return fmt.Errorf("unknown lifecycle phase %q", phaseFlag)
				}
				phases = []config.LifecyclePhase{p}
			}

			for _, phase := range phases {
				if err := eng.RunLifecycle(ctx, workspace, phase); err != nil {
					return err
				}
			}
			fmt.Fprintf(os.Stderr, "✓ lifecycle commands complete\n")
			return nil
		},
	}

	cmd.Flags().StringVar(&phaseFlag, "phase", "", "Run only this phase (onCreate|updateContent|postCreate|postStart|postAttach)")
	return cmd
}

func isKnownPhase(p config.LifecyclePhase) bool {
	for _, k := range defaultLifecyclePhases {
		if p == k {
			return true
		}
	}
	return false
}
