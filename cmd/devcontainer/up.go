package main

import (
	"github.com/spf13/cobra"

	devcontainer "github.com/crunchloop/devcontainer"
)

func newUpCmd(rf *rootFlags) *cobra.Command {
	var (
		recreate             bool
		runInitializeCommand bool
		runSecretsCommand    bool
		frozenLockfile       bool
		noLockfile           bool
	)

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create and start the dev container for a workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			level, err := rf.level()
			if err != nil {
				return err
			}

			ws, err := rf.resolveWorkspaceFolder()
			if err != nil {
				return err
			}

			eng, closeEng, err := rf.newEngine(ctx)
			if err != nil {
				return err
			}
			defer closeEng()

			evCh, stop := startEventPrinter(level)
			lockfile := devcontainer.LockfileDefault
			switch {
			case frozenLockfile:
				lockfile = devcontainer.LockfileFrozen
			case noLockfile:
				lockfile = devcontainer.LockfileDisabled
			}
			workspace, upErr := eng.Up(ctx, devcontainer.UpOptions{
				LocalWorkspaceFolder: ws,
				ConfigPath:           rf.configPath,
				Recreate:             recreate,
				RunInitializeCommand: runInitializeCommand,
				RunSecretsCommand:    runSecretsCommand,
				Lockfile:             lockfile,
				Events:               evCh,
			})
			stop()

			if upErr != nil {
				return upErr
			}

			stderrf("✓ workspace %s ready\n", workspace.ID)
			if workspace.Container != nil {
				stderrf("  container: %s\n", workspace.Container.ID)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&recreate, "remove-existing-container", false, "Stop and remove any existing container before creating a fresh one")
	cmd.Flags().BoolVar(&recreate, "recreate", false, "Alias for --remove-existing-container")
	cmd.Flags().BoolVar(&runInitializeCommand, "run-initialize-command", false, "Run devcontainer.json initializeCommand on the host before container creation")
	cmd.Flags().BoolVar(&runSecretsCommand, "run-secrets-command", false, "Run devcontainer.json secretsCommand on the host and inject its output as container env")
	cmd.Flags().BoolVar(&frozenLockfile, "frozen-lockfile", false, "Fail if devcontainer-lock.json is missing or out of date; never write it")
	cmd.Flags().BoolVar(&noLockfile, "no-lockfile", false, "Neither read nor write devcontainer-lock.json")
	cmd.MarkFlagsMutuallyExclusive("frozen-lockfile", "no-lockfile")

	return cmd
}
