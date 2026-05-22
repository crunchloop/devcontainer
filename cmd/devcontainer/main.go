// Command devcontainer is a CLI on top of the devcontainer Go library.
//
// Unlike @devcontainers/cli, this binary is meant for humans at a
// terminal: logs go to stderr, results to stdout, non-zero exit on
// failure. Tools that want programmatic access should import the
// library directly.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := newRootCmd().ExecuteContext(ctx)
	if err == nil {
		return
	}
	var silent silentExitError
	if errors.As(err, &silent) {
		os.Exit(silent.code)
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
