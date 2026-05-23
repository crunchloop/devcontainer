package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
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
			ttyState, err := setupTty(ctx, tty)
			if err != nil {
				return err
			}
			defer ttyState.restore()

			// Default cwd to the resolved container workspace folder
			// when --working-dir wasn't given, so `devcontainer exec ls`
			// lands inside the project rather than wherever the base
			// image's WORKDIR points.
			wd := workingDir
			if wd == "" {
				wd = cfg.ContainerWorkspaceFolder
			}

			res, err := eng.Exec(ctx, workspace, devcontainer.ExecOptions{
				Cmd:            args,
				Env:            env,
				User:           user,
				WorkingDir:     wd,
				Tty:            tty,
				Stdin:          os.Stdin,
				Stdout:         os.Stdout,
				Stderr:         os.Stderr,
				InitialTtySize: ttyState.initial,
				ResizeCh:       ttyState.resize,
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

// ttyState bundles the resources setupTty owns for a single exec
// invocation: the initial pty size at exec time, a channel that
// receives subsequent resizes triggered by SIGWINCH, and a restore
// function that tears it all down. restore is always safe to call,
// even when tty is false (it's a no-op in that case).
type ttyState struct {
	initial runtime.TtySize
	resize  <-chan runtime.TtySize
	restore func()
}

// setupTty puts stdin in raw mode when tty is true and starts a
// goroutine that translates SIGWINCH into resize-channel events for
// the lifetime of the exec. The returned ttyState carries the values
// to pass through devcontainer.ExecOptions.
func setupTty(ctx context.Context, tty bool) (ttyState, error) {
	if !tty {
		return ttyState{restore: func() {}}, nil
	}
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return ttyState{restore: func() {}}, fmt.Errorf("make raw: %w", err)
	}

	var initial runtime.TtySize
	if w, h, err := term.GetSize(fd); err == nil {
		initial = runtime.TtySize{Width: uint16(w), Height: uint16(h)}
	}

	resizeCh := make(chan runtime.TtySize, 1)
	sigwinch := make(chan os.Signal, 1)
	signal.Notify(sigwinch, syscall.SIGWINCH)
	winchCtx, cancelWinch := context.WithCancel(ctx)
	go func() {
		defer signal.Stop(sigwinch)
		for {
			select {
			case <-winchCtx.Done():
				return
			case <-sigwinch:
				w, h, err := term.GetSize(fd)
				if err != nil {
					continue
				}
				select {
				case resizeCh <- runtime.TtySize{Width: uint16(w), Height: uint16(h)}:
				case <-winchCtx.Done():
					return
				}
			}
		}
	}()

	return ttyState{
		initial: initial,
		resize:  resizeCh,
		restore: func() {
			cancelWinch()
			_ = term.Restore(fd, oldState)
		},
	}, nil
}

// silentExitError carries an exit code without a printed message.
// main.go recognizes it and exits with the given code.
type silentExitError struct{ code int }

func (s silentExitError) Error() string { return fmt.Sprintf("exit status %d", s.code) }
