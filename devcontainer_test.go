package devcontainer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, body string) (workspace, configPath string) {
	t.Helper()
	dir := t.TempDir()
	dcDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dcDir, "devcontainer.json")
	if err := os.WriteFile(cfg, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return dir, cfg
}

func TestResolve_Discovery(t *testing.T) {
	ws, cfg := writeFixture(t, `{"image":"alpine"}`)
	got, err := Resolve(context.Background(), ResolveOptions{
		LocalWorkspaceFolder: ws,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.LocalWorkspaceFolder != ws {
		t.Errorf("LocalWorkspaceFolder = %q", got.LocalWorkspaceFolder)
	}
	if got.DevcontainerID == "" {
		t.Error("DevcontainerID should be populated")
	}
	_ = cfg // path used implicitly via discovery
}

func TestResolve_DotDevcontainerJson(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".devcontainer.json")
	if err := os.WriteFile(cfg, []byte(`{"image":"alpine"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), ResolveOptions{LocalWorkspaceFolder: dir}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestResolve_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := Resolve(context.Background(), ResolveOptions{LocalWorkspaceFolder: dir})
	if err == nil {
		t.Fatal("expected error when no devcontainer.json exists")
	}
	if !strings.Contains(err.Error(), "no devcontainer.json") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolve_RequiresAbsolute(t *testing.T) {
	_, err := Resolve(context.Background(), ResolveOptions{LocalWorkspaceFolder: "relative/path"})
	if err == nil {
		t.Fatal("expected error for relative path")
	}
}

func TestResolve_LocalEnvOverride(t *testing.T) {
	ws, _ := writeFixture(t, `{"image":"${localEnv:CUSTOM}"}`)
	got, err := Resolve(context.Background(), ResolveOptions{
		LocalWorkspaceFolder: ws,
		LocalEnv:             map[string]string{"CUSTOM": "frobnicate:1"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	src := got.Source
	if src == nil {
		t.Fatal("Source nil")
	}
}

func TestResolve_DevcontainerIDOverride(t *testing.T) {
	ws, _ := writeFixture(t, `{"image":"alpine"}`)
	called := false
	got, err := Resolve(context.Background(), ResolveOptions{
		LocalWorkspaceFolder: ws,
		DevcontainerIDFunc: func(local, cfg string) string {
			called = true
			return "deadbeef"
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !called {
		t.Error("DevcontainerIDFunc not invoked")
	}
	if got.DevcontainerID != "deadbeef" {
		t.Errorf("DevcontainerID = %q", got.DevcontainerID)
	}
}

func TestResolve_CtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Resolve(ctx, ResolveOptions{LocalWorkspaceFolder: "/tmp"})
	if err == nil {
		t.Fatal("expected ctx cancelled error")
	}
}
