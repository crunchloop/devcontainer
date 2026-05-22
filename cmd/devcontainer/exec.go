package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
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

			tty := !noTty && term.IsTerminal(int(os.Stdin.Fd()))
			initialSize, resizeCh, restore, err := setupTty(ctx, tty)
			if err != nil {
				return err
			}
			defer restore()

			execOpts := devcontainer.ExecOptions{
				Cmd:            args,
				User:           user,
				WorkingDir:     workingDir,
				Tty:            tty,
				Stdin:          os.Stdin,
				Stdout:         os.Stdout,
				Stderr:         os.Stderr,
				InitialTtySize: initialSize,
				ResizeCh:       resizeCh,
			}

			res, err := eng.Exec(ctx, workspace, execOpts)
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

	return cmd
}

// setupTty puts the terminal in raw mode and wires SIGWINCH to a resize
// channel when tty is true. Returns a restore func that's always safe
// to call (no-op when tty was false).
func setupTty(ctx context.Context, tty bool) (runtime.TtySize, <-chan runtime.TtySize, func(), error) {
	if !tty {
		return runtime.TtySize{}, nil, func() {}, nil
	}
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return runtime.TtySize{}, nil, func() {}, fmt.Errorf("make raw: %w", err)
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

	restore := func() {
		cancelWinch()
		_ = term.Restore(fd, oldState)
	}
	return initial, resizeCh, restore, nil
}

// silentExitError carries an exit code without a printed message.
// main.go recognizes it and exits with the given code.
type silentExitError struct{ code int }

func (s silentExitError) Error() string { return fmt.Sprintf("exit status %d", s.code) }

func exitCodeFor(err error) int {
	var s silentExitError
	if errors.As(err, &s) {
		return s.code
	}
	return 1
}
