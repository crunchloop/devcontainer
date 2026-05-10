//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	dcruntime "github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

// buildVscodeUserImage builds a base image carrying a non-root vscode
// user at a known image-default UID/GID. Used to verify the
// updateRemoteUserUID feature: the test runner's UID (the workspace
// folder owner) differs from this baked UID, so the engine should
// rewrite /etc/passwd to match.
//
// The Dockerfile lines differ per distro because user-creation tools
// differ — the test only cares that we end up with `vscode` at the
// declared UID.
func buildVscodeUserImage(t *testing.T, rt *docker.Runtime, distro string, uid int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir := t.TempDir()
	var df string
	switch distro {
	case "alpine":
		df = `FROM alpine:3.20
RUN addgroup -g ` + strconv.Itoa(uid) + ` vscode \
 && adduser -D -u ` + strconv.Itoa(uid) + ` -G vscode -s /bin/sh vscode
USER vscode
`
	case "debian":
		df = `FROM debian:bookworm-slim
RUN groupadd -g ` + strconv.Itoa(uid) + ` vscode \
 && useradd -m -u ` + strconv.Itoa(uid) + ` -g vscode -s /bin/sh vscode
USER vscode
`
	default:
		t.Fatalf("unknown distro %q", distro)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0644); err != nil {
		t.Fatal(err)
	}

	tag := strings.ToLower("dc-it-uid-" + distro + "-" + strings.ReplaceAll(t.Name(), "/", "-") + ":latest")
	if _, err := rt.BuildImage(ctx, dcruntime.BuildSpec{
		ContextPath: dir,
		Dockerfile:  "Dockerfile",
		Tag:         tag,
	}, nil); err != nil {
		t.Fatalf("BuildImage(%s base): %v", distro, err)
	}
	return tag
}

func hostWorkspaceUID(t *testing.T, dir string) int {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat workspace: %v", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("stat info doesn't carry Uid (non-Unix?)")
	}
	return int(st.Uid)
}

// TestUpdateRemoteUserUID_ReconcilesUIDOnDebianAndAlpine builds a base
// image with vscode at a fixed UID, runs Up against a workspace owned
// by the test runner (different UID), and asserts that `id -u vscode`
// inside the resulting container matches the host workspace owner —
// proving the reconciliation Dockerfile actually ran and rewrote
// /etc/passwd.
//
// Linux-only: macOS Docker Desktop remaps file ownership in the VM,
// so the host UID our test sees is not what the container's bind
// mount sees. CI runs on Linux per PRD §9.2.
func TestUpdateRemoteUserUID_ReconcilesUIDOnDebianAndAlpine(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}
	if runtime.GOOS != "linux" {
		t.Skip("UID reconciliation assertions require Linux host (macOS Docker Desktop remaps ownership)")
	}

	const imageUID = 12345

	for _, distro := range []string{"debian", "alpine"} {
		distro := distro
		t.Run(distro, func(t *testing.T) {
			eng, rt := newEngine(t)
			defer rt.Close()

			image := buildVscodeUserImage(t, rt, distro, imageUID)
			ws := writeWorkspace(t, `{"image":"`+image+`","remoteUser":"vscode"}`)

			runnerUID := hostWorkspaceUID(t, ws)
			if runnerUID == imageUID {
				t.Skipf("test runner UID matches image-baked UID %d; reconciliation would be a no-op", imageUID)
			}
			if runnerUID == 0 {
				t.Skip("test runner is root; reconciliation is skipped by design")
			}

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
				Cmd:  []string{"sh", "-c", "id -u vscode"},
				User: "root",
			})
			if err != nil {
				t.Fatalf("Exec id: %v", err)
			}
			if res.ExitCode != 0 {
				t.Fatalf("id exit=%d stderr=%q", res.ExitCode, res.Stderr)
			}
			got := strings.TrimSpace(res.Stdout)
			want := fmt.Sprintf("%d", runnerUID)
			if got != want {
				t.Errorf("id -u vscode = %q, want %q (host workspace owner UID)", got, want)
			}
		})
	}
}

// TestUpdateRemoteUserUID_FalseLeavesUIDUnchanged verifies the negative
// path: with `updateRemoteUserUID: false`, vscode keeps its image-default
// UID even when the host workspace UID differs.
func TestUpdateRemoteUserUID_FalseLeavesUIDUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux host")
	}

	const imageUID = 12345
	eng, rt := newEngine(t)
	defer rt.Close()

	image := buildVscodeUserImage(t, rt, "debian", imageUID)
	ws := writeWorkspace(t, `{
		"image":"`+image+`",
		"remoteUser":"vscode",
		"updateRemoteUserUID": false
	}`)

	if hostWorkspaceUID(t, ws) == 0 {
		t.Skip("root host runner")
	}

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
		Cmd:  []string{"sh", "-c", "id -u vscode"},
		User: "root",
	})
	if err != nil {
		t.Fatalf("Exec id: %v", err)
	}
	got := strings.TrimSpace(res.Stdout)
	want := strconv.Itoa(imageUID)
	if got != want {
		t.Errorf("id -u vscode = %q, want %q (image default; reconciliation should be off)", got, want)
	}
}
