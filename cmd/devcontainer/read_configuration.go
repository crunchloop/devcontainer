package main

import (
	"encoding/json"
	"os"

	"github.com/spf13/cobra"

	devcontainer "github.com/crunchloop/devcontainer"
)

func newReadConfigurationCmd(rf *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "read-configuration",
		Short: "Resolve devcontainer.json and print the resolved configuration as JSON",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			ws, err := rf.resolveWorkspaceFolder()
			if err != nil {
				return err
			}

			cfg, err := devcontainer.Resolve(ctx, devcontainer.ResolveOptions{
				LocalWorkspaceFolder: ws,
				ConfigPath:           rf.configPath,
			})
			if err != nil {
				return err
			}

			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(cfg)
		},
	}
}
