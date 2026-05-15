//go:build integration && darwin && arm64

// Apple-container backend: userEnvProbe behavior through the engine.
// Mirrors a representative subset of userenvprobe_test.go for the
// docker backend (PathFromBashrc + LifecycleSeesBashrcPath + None).
// The full 6-variant matrix on docker is overkill here; these three
// hit the load-bearing engine paths.

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
)

const appleBashImage = "docker.io/library/bash:5.2-alpine3.20"

func TestAppleContainer_UserEnvProbe_PathFromBashrc(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, _ := newAppleContainerEngine(t)

	ws := writeWorkspace(t, `{
		"image": "`+appleBashImage+`",
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
	defer func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
	}()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"printenv", "EXTRA_PATH"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("printenv exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != "/from/bashrc" {
		t.Errorf("EXTRA_PATH = %q, want %q (probedEnv didn't inject from rc files)", got, "/from/bashrc")
	}
}

func TestAppleContainer_UserEnvProbe_None(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, _ := newAppleContainerEngine(t)

	ws := writeWorkspace(t, `{
		"image": "`+appleBashImage+`",
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
	defer func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
	}()

	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"printenv", "EXTRA_PATH"},
	})
	// printenv exits non-zero when the var is unset; that's the
	// success signal here.
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("EXTRA_PATH leaked with userEnvProbe=none (stdout=%q)", res.Stdout)
	}
}

// TestAppleContainer_UserEnvProbe_LifecycleSeesBashrcPath is the
// stricter variant: postCreate itself must see the probed env so a
// tool installed only via rc files is resolvable during the hook. Uses
// PR-G2 BuildImage to bake the fake tool + rc snippet into a base
// image. Skips if the builder isn't running.
func TestAppleContainer_UserEnvProbe_LifecycleSeesBashrcPath(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	eng, rt := newAppleContainerEngine(t)

	dir := t.TempDir()
	df := `FROM ` + appleBashImage + `
RUN mkdir -p /opt/mytool/bin /etc/profile.d \
 && printf '#!/bin/sh\necho hello-from-mytool\n' > /opt/mytool/bin/mytool \
 && chmod +x /opt/mytool/bin/mytool \
 && printf 'export PATH=/opt/mytool/bin:$PATH\n' > /etc/profile.d/mytool.sh
`
	if err := writeFile(dir+"/Dockerfile", df); err != nil {
		t.Fatal(err)
	}
	tag := "dc-it-ac-userenv:latest"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := rt.BuildImage(ctx, runtimeBuildSpec(dir, tag), nil); err != nil {
		var unavail *runtime.BuilderUnavailableError
		if errors.As(err, &unavail) {
			t.Skipf("BuildImage (builder not running): %v", err)
		}
		t.Fatalf("BuildImage: %v", err)
	}

	ws := writeWorkspace(t, `{
		"image": "`+tag+`",
		"postCreateCommand": "command -v mytool && mytool > /tmp/lifecycle-out"
	}`)

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: ws,
		Recreate:             true,
	})
	if err != nil {
		t.Fatalf("Up: %v (postCreate likely failed to resolve mytool — probe not merged into lifecycle env)", err)
	}
	defer func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
	}()

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
