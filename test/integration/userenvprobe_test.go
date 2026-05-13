//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// bashImage is a tiny image with bash available so the userEnvProbe's
// `bash -lic` invocation succeeds. plain alpine:3.20 ships only ash;
// the probe falls back to printenv there, exercised in
// TestUserEnvProbe_NoBashFallback.
const bashImage = "bash:5.2-alpine3.20"

// TestUserEnvProbe_PathFromBashrc covers the original bug: a
// postCreateCommand-installed tool writes its PATH addition to ~/.bashrc;
// later Exec calls must see that PATH without per-call shell wrapping.
func TestUserEnvProbe_PathFromBashrc(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{
		"image": "`+bashImage+`",
		"postCreateCommand": "echo 'export EXTRA_PATH=/from/bashrc' > /etc/profile.d/dc-go-test.sh"
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"printenv", "EXTRA_PATH"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("printenv EXTRA_PATH exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != "/from/bashrc" {
		t.Errorf("EXTRA_PATH = %q, want %q (probedEnv didn't inject from .bashrc)", got, "/from/bashrc")
	}
}

// TestUserEnvProbe_LifecycleSeesBashrcPath covers the original divergence
// from @devcontainers/cli: lifecycle hooks (postCreateCommand here) must
// run with the probed shell env merged in, so a tool whose PATH entry
// only lands via an rc-sourced snippet is visible during the hook itself
// — not just to later Exec calls.
func TestUserEnvProbe_LifecycleSeesBashrcPath(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newEngine(t)
	defer rt.Close()

	// /etc/profile.d/*.sh is sourced by bash -l. The fake "mytool" lives
	// at /opt/mytool/bin/mytool; without the probe injecting the rc-
	// contributed PATH, `command -v mytool` from inside postCreateCommand
	// would fail. The Dockerfile bakes both the snippet and the binary
	// before any lifecycle phase runs.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "Dockerfile"), `FROM `+bashImage+`
RUN mkdir -p /opt/mytool/bin /etc/profile.d \
 && printf '#!/bin/sh\necho hello-from-mytool\n' > /opt/mytool/bin/mytool \
 && chmod +x /opt/mytool/bin/mytool \
 && printf 'export PATH=/opt/mytool/bin:$PATH\n' > /etc/profile.d/mytool.sh
`)
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"build": { "dockerfile": "Dockerfile", "context": ".." },
		"postCreateCommand": "command -v mytool && mytool > /tmp/lifecycle-out"
	}`)
	ws := dir

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v (postCreateCommand likely couldn't find mytool — probe not merged into lifecycle env)", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"cat", "/tmp/lifecycle-out"},
	})
	if err != nil {
		t.Fatalf("read lifecycle output: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "hello-from-mytool" {
		t.Errorf("/tmp/lifecycle-out = %q, want %q", got, "hello-from-mytool")
	}
}

// TestUserEnvProbe_None disables probing via devcontainer.json. The
// .bashrc-exported var should NOT be visible to subsequent execs.
func TestUserEnvProbe_None(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{
		"image": "`+bashImage+`",
		"userEnvProbe": "none",
		"postCreateCommand": "echo 'export EXTRA_PATH=/from/bashrc' > /etc/profile.d/dc-go-test.sh"
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "printenv EXTRA_PATH; true"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "" {
		t.Errorf("EXTRA_PATH = %q, want empty (userEnvProbe=none should skip injection)", got)
	}
}

// TestUserEnvProbe_SkipOption verifies the per-Exec opt-out: even with
// the default probe, a caller passing SkipUserEnvProbe gets a clean env.
func TestUserEnvProbe_SkipOption(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{
		"image": "`+bashImage+`",
		"postCreateCommand": "echo 'export EXTRA_PATH=/from/bashrc' > /etc/profile.d/dc-go-test.sh"
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	// Default Exec sees it.
	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"printenv", "EXTRA_PATH"},
	})
	if err != nil {
		t.Fatalf("default Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "/from/bashrc" {
		t.Fatalf("default exec EXTRA_PATH = %q, want %q (sanity precondition failed)", got, "/from/bashrc")
	}

	// SkipUserEnvProbe Exec doesn't.
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd:              []string{"sh", "-c", "printenv EXTRA_PATH; true"},
		SkipUserEnvProbe: true,
	})
	if err != nil {
		t.Fatalf("skip Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "" {
		t.Errorf("SkipUserEnvProbe Exec saw EXTRA_PATH = %q, want empty", got)
	}
}

// TestUserEnvProbe_NoBashFallback runs against a bash-less image
// (alpine ships only ash). The probe's first attempt (bash -lic) fails;
// we still want Up to succeed and Exec to work normally.
func TestUserEnvProbe_NoBashFallback(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{"image":"`+testImage+`"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	// Exec a basic command — failure mode would be an Up-level error
	// (already caught above) or Exec returning nonzero / wrong output.
	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"sh", "-c", "echo ok"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "ok") {
		t.Errorf("Exec stdout = %q, want contains 'ok'", res.Stdout)
	}
}

// TestUserEnvProbe_RemoteEnvMerged confirms cfg.RemoteEnv reaches Exec
// independently of whether the probe ran. RemoteEnv is the
// devcontainer-author-declared env; it must layer on top of probedEnv
// and be visible to every Exec call (matching @devcontainers/cli and
// devpod).
func TestUserEnvProbe_RemoteEnvMerged(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{
		"image": "`+bashImage+`",
		"remoteEnv": {
			"FROM_REMOTE": "remote-value"
		}
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"printenv", "FROM_REMOTE"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "remote-value" {
		t.Errorf("FROM_REMOTE = %q, want %q", got, "remote-value")
	}
}
