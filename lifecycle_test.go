package devcontainer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

// scriptedRuntime extends fakeRuntime with controllable Exec behavior:
// matching cmd substrings to canned ExecResults. Falls back to a default
// "exit=0, empty output" response.
type scriptedRuntime struct {
	*fakeRuntime
	scripts        map[string]runtime.ExecResult
	execLog        []runtime.ExecOptions
	containerStart time.Time
}

func newScriptedRuntime() *scriptedRuntime {
	now := time.Now().UTC()
	r := &scriptedRuntime{
		fakeRuntime:    newFakeRuntime(),
		scripts:        map[string]runtime.ExecResult{},
		containerStart: now,
	}
	return r
}

// override RunContainer to set realistic timestamps so lifecycle marker
// keying works.
func (s *scriptedRuntime) RunContainer(ctx context.Context, spec runtime.RunSpec) (*runtime.Container, error) {
	c, err := s.fakeRuntime.RunContainer(ctx, spec)
	if err != nil {
		return nil, err
	}
	d := s.fakeRuntime.containersByID[c.ID]
	d.Created = s.containerStart
	d.StartedAt = s.containerStart
	return c, nil
}

func (s *scriptedRuntime) ExecContainer(ctx context.Context, id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
	s.execLog = append(s.execLog, opts)
	for fragment, res := range s.scripts {
		for _, c := range opts.Cmd {
			if strings.Contains(c, fragment) {
				return res, nil
			}
		}
	}
	return runtime.ExecResult{ExitCode: 0}, nil
}

func TestUp_SkipsLifecycleWhenRequested(t *testing.T) {
	rt := newScriptedRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"postCreateCommand":"echo should-not-run"
	}`)

	_, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		SkipLifecycle:        true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, call := range rt.execLog {
		for _, c := range call.Cmd {
			if strings.Contains(c, "should-not-run") {
				t.Errorf("lifecycle ran despite SkipLifecycle=true: %v", call.Cmd)
			}
		}
	}
}

func TestUp_RunsLifecycleAndWritesMarker(t *testing.T) {
	rt := newScriptedRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"postCreateCommand":"echo lifecycle-ran"
	}`)

	_, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	var ranUserCmd, wroteMarker bool
	for _, call := range rt.execLog {
		joined := strings.Join(call.Cmd, " ")
		if strings.Contains(joined, "lifecycle-ran") {
			ranUserCmd = true
		}
		if strings.Contains(joined, "mkdir -p "+markerDir) {
			wroteMarker = true
		}
	}
	if !ranUserCmd {
		t.Error("expected postCreateCommand to run")
	}
	if !wroteMarker {
		t.Error("expected marker write after successful lifecycle")
	}
}

func TestUp_LifecycleErrorsSurfaceAsLifecycleError(t *testing.T) {
	rt := newScriptedRuntime()
	rt.scripts["broken-script"] = runtime.ExecResult{
		ExitCode: 17,
		Stderr:   "permission denied",
	}
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"postCreateCommand":"./broken-script"
	}`)

	_, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err == nil {
		t.Fatal("expected error from failing lifecycle")
	}
	if !IsLifecycleError(err) {
		t.Fatalf("expected *LifecycleError, got %T: %v", err, err)
	}
	var le *LifecycleError
	if !errors.As(err, &le) {
		t.Fatalf("expected *LifecycleError chain, got %v", err)
	}
	if le.ExitCode != 17 {
		t.Errorf("ExitCode = %d, want 17", le.ExitCode)
	}
	if le.Phase != config.LifecyclePostCreate {
		t.Errorf("Phase = %q, want postCreate", le.Phase)
	}
}

// TestUp_LifecycleFailureReturnsWorkspace verifies that when a lifecycle
// phase fails, Up still returns the *Workspace so callers can choose to
// recover (warn-and-continue, reattach for debugging) instead of treating
// every postCreateCommand bug as a fatal Up failure. Mirrors
// @devcontainers/cli, where lifecycle failure exits 1 but the container
// stays running and reattachable.
func TestUp_LifecycleFailureReturnsWorkspace(t *testing.T) {
	rt := newScriptedRuntime()
	rt.scripts["broken-script"] = runtime.ExecResult{ExitCode: 17, Stderr: "boom"}
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"postCreateCommand":"./broken-script"
	}`)

	wsObj, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err == nil {
		t.Fatal("expected error from failing lifecycle")
	}
	var le *LifecycleError
	if !errors.As(err, &le) {
		t.Fatalf("expected *LifecycleError, got %T: %v", err, err)
	}
	if wsObj == nil || wsObj.Container == nil || wsObj.Container.ID == "" {
		t.Fatalf("Up returned no usable workspace on lifecycle failure (want one so callers can recover): %+v", wsObj)
	}
	// The container should still be reachable via the runtime — Up
	// must not have torn it down. Run a no-op exec to confirm.
	if _, err := eng.Exec(context.Background(), wsObj, ExecOptions{Cmd: []string{"true"}}); err != nil {
		t.Errorf("Exec on returned workspace after lifecycle failure: %v", err)
	}
}

func TestRunPhase_SkipsWhenMarkerMatches(t *testing.T) {
	rt := newScriptedRuntime()
	// Pretend a marker exists matching the container's Created timestamp.
	// Format that the readMarker code expects.
	rt.scripts["cat /var/devcontainer-go/markers/postCreate"] = runtime.ExecResult{
		ExitCode: 0,
		Stdout: `{
			"v": 1,
			"phase": "postCreate",
			"keyTimestamp": "` + rt.containerStart.UTC().Format(time.RFC3339Nano) + `",
			"ranAt": "` + rt.containerStart.UTC().Format(time.RFC3339Nano) + `",
			"durationMs": 0,
			"exitCode": 0
		}`,
	}

	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"postCreateCommand":"echo should-be-skipped"
	}`)

	_, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	for _, call := range rt.execLog {
		for _, c := range call.Cmd {
			if strings.Contains(c, "should-be-skipped") {
				t.Errorf("postCreate ran despite matching marker: %v", call.Cmd)
			}
		}
	}
}

func TestRunPhase_RunsWhenMarkerStale(t *testing.T) {
	rt := newScriptedRuntime()
	// Marker keyed to a different (older) timestamp.
	rt.scripts["cat /var/devcontainer-go/markers/postCreate"] = runtime.ExecResult{
		ExitCode: 0,
		Stdout: `{
			"v": 1,
			"phase": "postCreate",
			"keyTimestamp": "2020-01-01T00:00:00Z",
			"ranAt": "2020-01-01T00:00:00Z",
			"durationMs": 0,
			"exitCode": 0
		}`,
	}

	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"postCreateCommand":"echo fresh-run"
	}`)
	_, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	ran := false
	for _, call := range rt.execLog {
		joined := strings.Join(call.Cmd, " ")
		if strings.Contains(joined, "fresh-run") {
			ran = true
		}
	}
	if !ran {
		t.Error("expected postCreate to re-run when marker is stale")
	}
}

func TestPostAttach_AlwaysRuns(t *testing.T) {
	rt := newScriptedRuntime()
	// Even with a "matching" marker present, postAttach must run because
	// it has no marker semantics.
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"postAttachCommand":"echo attach-ran"
	}`)
	_, err := eng.Up(context.Background(), UpOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	ran := false
	for _, call := range rt.execLog {
		joined := strings.Join(call.Cmd, " ")
		if strings.Contains(joined, "attach-ran") {
			ran = true
		}
	}
	if !ran {
		t.Error("postAttach should always run")
	}
}

func TestRunLifecycle_EmptyPhaseIsNoop(t *testing.T) {
	rt := newScriptedRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{"image":"alpine:3.20"}`)

	wsObj, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		SkipLifecycle:        true, // we'll invoke explicitly
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	rt.execLog = nil
	if err := eng.RunLifecycle(context.Background(), wsObj, config.LifecyclePostCreate); err != nil {
		t.Fatalf("RunLifecycle: %v", err)
	}
	if len(rt.execLog) != 0 {
		t.Errorf("empty phase should not exec, got %d calls", len(rt.execLog))
	}
}

func TestInitializeCommand_RequiresHostExecutor(t *testing.T) {
	rt := newScriptedRuntime()
	eng, _ := New(EngineOptions{Runtime: rt})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"initializeCommand":"echo on-host"
	}`)
	_, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		RunInitializeCommand: true,
	})
	if err == nil {
		t.Fatal("expected error when HostExecutor is unset")
	}
	if !IsLifecycleError(err) {
		t.Fatalf("want *LifecycleError, got %T", err)
	}
	if !errors.Is(err, ErrHostExecutorNotConfigured) {
		t.Errorf("want ErrHostExecutorNotConfigured in error chain, got %v", err)
	}
}

// fakeHostExecutor records every call and returns a canned exit code.
type fakeHostExecutor struct {
	calls    []HostCommand
	exitCode int
	stderr   string
	err      error
}

func (f *fakeHostExecutor) ExecHost(ctx context.Context, cmd HostCommand) (HostExecResult, error) {
	f.calls = append(f.calls, cmd)
	if f.err != nil {
		return HostExecResult{}, f.err
	}
	return HostExecResult{ExitCode: f.exitCode, Stderr: f.stderr}, nil
}

func TestInitializeCommand_RoutesToHostExecutorSingle(t *testing.T) {
	rt := newScriptedRuntime()
	hx := &fakeHostExecutor{}
	eng, _ := New(EngineOptions{Runtime: rt, HostExecutor: hx})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"initializeCommand":"echo on-host"
	}`)
	if _, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		RunInitializeCommand: true,
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(hx.calls) != 1 {
		t.Fatalf("expected one host call, got %d", len(hx.calls))
	}
	if hx.calls[0].Shell != "echo on-host" {
		t.Errorf("Shell = %q, want %q", hx.calls[0].Shell, "echo on-host")
	}
	if hx.calls[0].WorkingDir != ws {
		t.Errorf("WorkingDir = %q, want %q (LocalWorkspaceFolder)", hx.calls[0].WorkingDir, ws)
	}
}

// concurrentHostExecutor records calls under a mutex (the parallel
// form runs goroutines, so the bare slice in fakeHostExecutor would
// race under -race).
type concurrentHostExecutor struct {
	mu    sync.Mutex
	calls []HostCommand
}

func (c *concurrentHostExecutor) ExecHost(ctx context.Context, cmd HostCommand) (HostExecResult, error) {
	c.mu.Lock()
	c.calls = append(c.calls, cmd)
	c.mu.Unlock()
	return HostExecResult{}, nil
}

func TestInitializeCommand_RoutesToHostExecutorParallel(t *testing.T) {
	rt := newScriptedRuntime()
	hx := &concurrentHostExecutor{}
	eng, _ := New(EngineOptions{Runtime: rt, HostExecutor: hx})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"initializeCommand": {
			"setup":   "echo setup",
			"prepare": "echo prepare"
		}
	}`)
	if _, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		RunInitializeCommand: true,
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(hx.calls) != 2 {
		t.Fatalf("expected 2 host calls, got %d", len(hx.calls))
	}
	got := map[string]bool{}
	for _, c := range hx.calls {
		got[c.Shell] = true
		if c.WorkingDir != ws {
			t.Errorf("WorkingDir = %q, want %q", c.WorkingDir, ws)
		}
	}
	for _, want := range []string{"echo setup", "echo prepare"} {
		if !got[want] {
			t.Errorf("missing parallel call %q (got %v)", want, got)
		}
	}
}

func TestInitializeCommand_NonZeroExitProducesLifecycleError(t *testing.T) {
	rt := newScriptedRuntime()
	hx := &fakeHostExecutor{exitCode: 7, stderr: "boom"}
	eng, _ := New(EngineOptions{Runtime: rt, HostExecutor: hx})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"initializeCommand":"false"
	}`)
	_, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
		RunInitializeCommand: true,
	})
	if err == nil {
		t.Fatal("expected error on non-zero host exit")
	}
	var le *LifecycleError
	if !errors.As(err, &le) {
		t.Fatalf("want *LifecycleError, got %T", err)
	}
	if le.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", le.ExitCode)
	}
}

func TestInitializeCommand_SkippedByDefault(t *testing.T) {
	// RunInitializeCommand=false (default) → host executor never called,
	// even if devcontainer.json declares initializeCommand.
	rt := newScriptedRuntime()
	hx := &fakeHostExecutor{}
	eng, _ := New(EngineOptions{Runtime: rt, HostExecutor: hx})
	ws := writeImageDevcontainer(t, `{
		"image":"alpine:3.20",
		"initializeCommand":"echo never"
	}`)
	if _, err := eng.Up(context.Background(), UpOptions{
		LocalWorkspaceFolder: ws,
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(hx.calls) != 0 {
		t.Errorf("HostExecutor must not be invoked unless RunInitializeCommand=true, got %d calls", len(hx.calls))
	}
}
