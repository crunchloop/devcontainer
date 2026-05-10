package devcontainer

import (
	"context"
	"errors"
	"testing"
)

func TestParseSecretsKV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"empty", "", nil},
		{"single", "FOO=bar\n", map[string]string{"FOO": "bar"}},
		{"trailing newline missing", "FOO=bar", map[string]string{"FOO": "bar"}},
		{"crlf", "FOO=bar\r\nBAZ=qux\r\n", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"comments and blanks ignored", "# header\n\nFOO=bar\n  # indented comment\nBAZ=qux\n", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"value with =", "URL=https://x?a=b&c=d\n", map[string]string{"URL": "https://x?a=b&c=d"}},
		{"empty value", "EMPTY=\nFOO=bar\n", map[string]string{"EMPTY": "", "FOO": "bar"}},
		{"key whitespace trimmed", "  FOO  =bar\n", map[string]string{"FOO": "bar"}},
		{"value preserved verbatim", `QUOTED="x y"`, map[string]string{"QUOTED": `"x y"`}},
		{"no equals skipped", "GARBAGE\nFOO=bar\n", map[string]string{"FOO": "bar"}},
		{"empty key skipped", "=value\nFOO=bar\n", map[string]string{"FOO": "bar"}},
		{"only blanks", "\n\n# only\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSecretsKV(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("[%s] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestSecretsCommand_RequiresHostExecutor(t *testing.T) {
	rt := newScriptedRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"secretsCommand":"echo TOKEN=abc"
	}`)
	_, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		RunSecretsCommand:    true,
	})
	if err == nil {
		t.Fatal("expected error when HostExecutor is unset")
	}
	if !errors.Is(err, ErrHostExecutorNotConfigured) {
		t.Errorf("want ErrHostExecutorNotConfigured in error chain, got %v", err)
	}
	var le *LifecycleError
	if !errors.As(err, &le) {
		t.Fatalf("want *LifecycleError, got %T", err)
	}
	if le.Phase != phaseSecretsCommand {
		t.Errorf("Phase = %q, want %q", le.Phase, phaseSecretsCommand)
	}
}

// secretsHostExecutor returns a canned stdout for the first call and
// records the host command shell so tests can assert on it.
type secretsHostExecutor struct {
	stdout   string
	stderr   string
	exitCode int
	calls    []HostCommand
}

func (s *secretsHostExecutor) ExecHost(ctx context.Context, cmd HostCommand) (HostExecResult, error) {
	s.calls = append(s.calls, cmd)
	return HostExecResult{ExitCode: s.exitCode, Stdout: s.stdout, Stderr: s.stderr}, nil
}

func TestSecretsCommand_MergesIntoContainerEnv(t *testing.T) {
	rt := newFakeRuntime()
	hx := &secretsHostExecutor{stdout: "TOKEN=abc\nDB_URL=postgres://x\n"}
	eng, _ := New(EngineOptions{Runtime: rt, HostExecutor: hx})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"containerEnv": {"FROM_CFG":"keep"},
		"secretsCommand":"echo TOKEN=abc"
	}`)
	if _, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		RunSecretsCommand:    true,
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if rt.createdSpec == nil {
		t.Fatal("createdSpec is nil")
	}
	if len(hx.calls) != 1 {
		t.Fatalf("expected one host call, got %d", len(hx.calls))
	}
	if hx.calls[0].Shell != "echo TOKEN=abc" {
		t.Errorf("Shell = %q, want %q", hx.calls[0].Shell, "echo TOKEN=abc")
	}
	if hx.calls[0].WorkingDir != ws {
		t.Errorf("WorkingDir = %q, want %q", hx.calls[0].WorkingDir, ws)
	}
	for _, want := range []struct{ k, v string }{
		{"TOKEN", "abc"},
		{"DB_URL", "postgres://x"},
		{"FROM_CFG", "keep"},
	} {
		if got := rt.createdSpec.Env[want.k]; got != want.v {
			t.Errorf("Env[%s] = %q, want %q", want.k, got, want.v)
		}
	}
}

func TestSecretsCommand_ExtraContainerEnvOverridesSecrets(t *testing.T) {
	rt := newFakeRuntime()
	hx := &secretsHostExecutor{stdout: "TOKEN=from-secrets\nLEAVE=alone\n"}
	eng, _ := New(EngineOptions{Runtime: rt, HostExecutor: hx})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"secretsCommand":"emit"
	}`)
	if _, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		RunSecretsCommand:    true,
		ExtraContainerEnv:    map[string]string{"TOKEN": "from-caller"},
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := rt.createdSpec.Env["TOKEN"]; got != "from-caller" {
		t.Errorf("ExtraContainerEnv must win on collision, got TOKEN=%q", got)
	}
	if got := rt.createdSpec.Env["LEAVE"]; got != "alone" {
		t.Errorf("non-colliding secret dropped: LEAVE=%q", got)
	}
}

func TestSecretsCommand_SkippedByDefault(t *testing.T) {
	rt := newFakeRuntime()
	hx := &secretsHostExecutor{stdout: "TOKEN=should-not-appear\n"}
	eng, _ := New(EngineOptions{Runtime: rt, HostExecutor: hx})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"secretsCommand":"emit"
	}`)
	if _, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(hx.calls) != 0 {
		t.Errorf("HostExecutor must not be invoked unless RunSecretsCommand=true, got %d calls", len(hx.calls))
	}
	if _, ok := rt.createdSpec.Env["TOKEN"]; ok {
		t.Errorf("secretsCommand output leaked into env despite RunSecretsCommand=false")
	}
}

func TestSecretsCommand_NonZeroExitProducesLifecycleError(t *testing.T) {
	rt := newFakeRuntime()
	hx := &secretsHostExecutor{exitCode: 13, stderr: "no creds"}
	eng, _ := New(EngineOptions{Runtime: rt, HostExecutor: hx})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"secretsCommand":"fail"
	}`)
	_, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		RunSecretsCommand:    true,
	})
	if err == nil {
		t.Fatal("expected error on non-zero secretsCommand exit")
	}
	var le *LifecycleError
	if !errors.As(err, &le) {
		t.Fatalf("want *LifecycleError, got %T", err)
	}
	if le.ExitCode != 13 {
		t.Errorf("ExitCode = %d, want 13", le.ExitCode)
	}
	if le.Phase != phaseSecretsCommand {
		t.Errorf("Phase = %q, want %q", le.Phase, phaseSecretsCommand)
	}
}
