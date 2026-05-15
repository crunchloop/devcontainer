//go:build darwin && arm64

package applecontainer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/runtime"
)

// runningContainer sets up a long-lived alpine container the test
// function can exec into. Cleans up automatically.
func runningContainer(t *testing.T, id string) *Runtime {
	t.Helper()
	rt := runtimeOrSkip(t)
	ctx := context.Background()

	_ = rt.RemoveContainer(ctx, id, runtime.RemoveOptions{Force: true})
	t.Cleanup(func() {
		_ = rt.RemoveContainer(ctx, id, runtime.RemoveOptions{Force: true})
	})

	cliRunStrict(t,
		"run", "--rm", "--name", "ac-alpine-warmup",
		"docker.io/library/alpine:latest", "/bin/true",
	)

	if _, err := rt.RunContainer(ctx, runtime.RunSpec{
		Image: "docker.io/library/alpine:latest",
		Name:  id,
		Cmd:   []string{"sleep", "180"},
	}); err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if err := waitForState(t, rt, id, runtime.StateRunning, 5*time.Second); err != nil {
		t.Fatalf("waitForState running: %v", err)
	}
	return rt
}

// TestExec_CaptureStdoutAndExit covers the bread-and-butter path:
// no stdin, captured stdout, exit code propagation.
func TestExec_CaptureStdoutAndExit(t *testing.T) {
	rt := runningContainer(t, "ac-exec-capture")
	ctx := context.Background()

	res, err := rt.ExecContainer(ctx, "ac-exec-capture", runtime.ExecOptions{
		Cmd: []string{"/bin/sh", "-c", "echo hello-stdout; echo hello-stderr 1>&2; exit 7"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode: want 7 got %d", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello-stdout") {
		t.Errorf("Stdout: want contains %q, got %q", "hello-stdout", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "hello-stderr") {
		t.Errorf("Stderr: want contains %q, got %q", "hello-stderr", res.Stderr)
	}
}

// TestExec_StdinRoundTrip pipes a payload through cat and verifies
// the bidirectional pipe wiring.
func TestExec_StdinRoundTrip(t *testing.T) {
	rt := runningContainer(t, "ac-exec-stdin")
	ctx := context.Background()

	const payload = "ping-pong-marker-42"
	res, err := rt.ExecContainer(ctx, "ac-exec-stdin", runtime.ExecOptions{
		Cmd:   []string{"/bin/cat"},
		Stdin: strings.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode: want 0 got %d", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, payload) {
		t.Errorf("Stdout: want contains %q, got %q", payload, res.Stdout)
	}
}

// TestExec_StreamingWriters bypasses the captured-string fallback to
// exercise the io.Writer streaming path (which is what DAP's readiness
// probe + workd attach will use in production).
func TestExec_StreamingWriters(t *testing.T) {
	rt := runningContainer(t, "ac-exec-stream")
	ctx := context.Background()

	var outBuf, errBuf bytes.Buffer
	res, err := rt.ExecContainer(ctx, "ac-exec-stream", runtime.ExecOptions{
		Cmd:    []string{"/bin/sh", "-c", "echo stream-out; echo stream-err 1>&2"},
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode: want 0 got %d", res.ExitCode)
	}
	// Captured fields stay empty when caller provides writers — this
	// is the documented contract on runtime.ExecOptions.
	if res.Stdout != "" || res.Stderr != "" {
		t.Errorf("captured fields should be empty when writers provided; got Stdout=%q Stderr=%q",
			res.Stdout, res.Stderr)
	}
	if !strings.Contains(outBuf.String(), "stream-out") {
		t.Errorf("stdout buffer: want contains stream-out, got %q", outBuf.String())
	}
	if !strings.Contains(errBuf.String(), "stream-err") {
		t.Errorf("stderr buffer: want contains stream-err, got %q", errBuf.String())
	}
}

// TestExec_ContextCancelKillsProcess is the design §11.3 question.
// A long-running process is launched, ctx is cancelled after a beat,
// and we assert ExecContainer returns ctx.Err() promptly (not after
// the process's natural timeout).
func TestExec_ContextCancelKillsProcess(t *testing.T) {
	rt := runningContainer(t, "ac-exec-cancel")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Spawn `sleep 60` then cancel after 500ms. ExecContainer should
	// return well before 60s (we give it 10s tolerance for the
	// SIGTERM round-trip + apiserver scheduling).
	done := make(chan struct{})
	var execErr error
	start := time.Now()
	go func() {
		defer close(done)
		_, execErr = rt.ExecContainer(ctx, "ac-exec-cancel", runtime.ExecOptions{
			Cmd: []string{"/bin/sleep", "60"},
		})
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("ExecContainer did not return within 15s of ctx cancel — SIGTERM may not be propagating")
	}

	elapsed := time.Since(start)
	if elapsed > 12*time.Second {
		t.Errorf("ExecContainer took %v after cancel; expected ≪ sleep timeout", elapsed)
	}
	if !errors.Is(execErr, context.Canceled) {
		t.Errorf("ExecContainer returned err %v; want context.Canceled", execErr)
	}
}

// TestExec_EnvAndCwd verifies the per-call env and workingDir
// overrides land in the in-VM process. Engine relies on this for
// devcontainer.json's remoteEnv + workspaceFolder.
func TestExec_EnvAndCwd(t *testing.T) {
	rt := runningContainer(t, "ac-exec-env")
	ctx := context.Background()

	res, err := rt.ExecContainer(ctx, "ac-exec-env", runtime.ExecOptions{
		Cmd:        []string{"/bin/sh", "-c", "echo MY=$MY_VAR PWD=$(pwd)"},
		Env:        map[string]string{"MY_VAR": "set-from-exec", "PATH": "/usr/bin:/bin"},
		WorkingDir: "/tmp",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "MY=set-from-exec") {
		t.Errorf("env not honored: stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "PWD=/tmp") {
		t.Errorf("workingDir not honored: stdout=%q", res.Stdout)
	}
}
