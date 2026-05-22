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
	)

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create and start the dev container for a workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			ws, err := rf.resolveWorkspaceFolder()
			if err != nil {
				return err
			}

			eng, closeEng, err := rf.newEngine(ctx)
			if err != nil {
				return err
			}
			defer closeEng()

			evCh, stop := startEventPrinter()
			workspace, upErr := eng.Up(ctx, devcontainer.UpOptions{
				LocalWorkspaceFolder: ws,
				ConfigPath:           rf.configPath,
				Recreate:             recreate,
				RunInitializeCommand: runInitializeCommand,
				RunSecretsCommand:    runSecretsCommand,
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
	cmd.Flags().BoolVar(&runInitializeCommand, "run-initialize-command", false, "Run devcontainer.json initializeCommand on the host before container creation")
	cmd.Flags().BoolVar(&runSecretsCommand, "run-secrets-command", false, "Run devcontainer.json secretsCommand on the host and inject its output as container env")

	return cmd
}
