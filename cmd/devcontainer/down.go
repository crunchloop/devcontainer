package main

import (
	"github.com/spf13/cobra"

	devcontainer "github.com/crunchloop/devcontainer"
)

func newDownCmd(rf *rootFlags) *cobra.Command {
	var removeVolumes bool
	return newDownLikeCmd(rf, downLikeOpts{
		use:    "down",
		short:  "Stop and remove the workspace's dev container",
		remove: true,
		extraFlags: func(c *cobra.Command) {
			c.Flags().BoolVar(&removeVolumes, "remove-volumes", false, "Also remove anonymous volumes created by the container")
		},
		removeVolumes: &removeVolumes,
	})
}

func newStopCmd(rf *rootFlags) *cobra.Command {
	return newDownLikeCmd(rf, downLikeOpts{
		use:    "stop",
		short:  "Stop the workspace's dev container without removing it",
		remove: false,
	})
}

type downLikeOpts struct {
	use           string
	short         string
	remove        bool
	removeVolumes *bool
	extraFlags    func(*cobra.Command)
}

func newDownLikeCmd(rf *rootFlags, o downLikeOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   o.use,
		Short: o.short,
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

			eng, closeEng, err := rf.newEngine(ctx)
			if err != nil {
				return err
			}
			defer closeEng()

			workspace, err := eng.Attach(ctx, devcontainer.WorkspaceID(cfg.DevcontainerID))
			if err != nil {
				return err
			}

			evCh, stop := startEventPrinter()
			downOpts := devcontainer.DownOptions{
				Remove: o.remove,
				Events: evCh,
			}
			if o.removeVolumes != nil {
				downOpts.RemoveVolumes = *o.removeVolumes
			}
			err = eng.Down(ctx, workspace, downOpts)
			stop()
			if err != nil {
				return err
			}

			if o.remove {
				stderrf("✓ workspace %s down\n", workspace.ID)
			} else {
				stderrf("✓ workspace %s stopped\n", workspace.ID)
			}
			return nil
		},
	}
	if o.extraFlags != nil {
		o.extraFlags(cmd)
	}
	return cmd
}
