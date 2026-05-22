package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	devcontainer "github.com/crunchloop/devcontainer"
)

func newExecCmd(rf *rootFlags) *cobra.Command {
	var (
		user       string
		workingDir string
		noTty      bool
		envFlags   []string
	)

	cmd := &cobra.Command{
		Use:   "exec <cmd> [args...]",
		Short: "Run a command inside the workspace's dev container",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			env, err := parseEnvFlags(envFlags)
			if err != nil {
				return err
			}

			tty := !noTty && term.IsTerminal(int(os.Stdin.Fd()))
			restore, err := setupTty(tty)
			if err != nil {
				return err
			}
			defer restore()

			// NOTE: window-size forwarding (SIGWINCH → resize) is not
			// wired here yet — the runtime ExecOptions surface for it
			// is still in-flight on main. Once it lands we can plumb
			// term.GetSize + signal.Notify(SIGWINCH) through.
			res, err := eng.Exec(ctx, workspace, devcontainer.ExecOptions{
				Cmd:        args,
				Env:        env,
				User:       user,
				WorkingDir: workingDir,
				Tty:        tty,
				Stdin:      os.Stdin,
				Stdout:     os.Stdout,
				Stderr:     os.Stderr,
			})
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				// Propagate non-zero exit without printing an error
				// banner — the exec'd command speaks for itself.
				return silentExitError{code: res.ExitCode}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&user, "user", "", "Override remoteUser/containerUser for this exec")
	cmd.Flags().StringVar(&workingDir, "working-dir", "", "Working directory inside the container")
	cmd.Flags().BoolVar(&noTty, "no-tty", false, "Do not allocate a TTY even if stdin is a terminal")
	cmd.Flags().StringArrayVarP(&envFlags, "env", "e", nil, "Set an environment variable (KEY=VALUE). Repeatable.")

	return cmd
}

// parseEnvFlags turns each "KEY=VALUE" into a map entry. The first '='
// is the separator, so values containing '=' (URLs, base64) survive.
// Entries without '=' are rejected.
func parseEnvFlags(flags []string) (map[string]string, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(flags))
	for _, e := range flags {
		i := strings.IndexByte(e, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid --env %q: expected KEY=VALUE", e)
		}
		out[e[:i]] = e[i+1:]
	}
	return out, nil
}

// setupTty puts the terminal in raw mode when tty is true and returns a
// restore func that's always safe to call (no-op when tty was false).
func setupTty(tty bool) (func(), error) {
	if !tty {
		return func() {}, nil
	}
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}, fmt.Errorf("make raw: %w", err)
	}
	return func() { _ = term.Restore(fd, oldState) }, nil
}

// silentExitError carries an exit code without a printed message.
// main.go recognizes it and exits with the given code.
type silentExitError struct{ code int }

func (s silentExitError) Error() string { return fmt.Sprintf("exit status %d", s.code) }
