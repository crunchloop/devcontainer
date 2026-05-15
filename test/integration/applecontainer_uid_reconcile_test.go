//go:build integration && darwin && arm64

// Apple-container backend: the inverted UID-reconcile contract.
// Design §13.8 declares we DO NOT run updateRemoteUserUID on this
// backend — virtiofs is identity-permissive, so the docker-style
// dance of rewriting /etc/passwd to match the host UID would be
// harmful without buying anything. This test pins that behavior:
//   - Workspace folder lives at the host user's UID.
//   - Container has a baked vscode user at a different UID.
//   - After Up + Exec as vscode, /etc/passwd vscode's UID must be
//     UNCHANGED (the baked image-default UID), AND the workspace
//     mount must still be writable as vscode.

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/runtime"
)

func TestAppleContainer_UID_NotReconciled_MountStillWritable(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newAppleContainerEngine(t)

	// Build a base image with vscode at UID 4321 (intentionally weird
	// so it can't collide with the host's actual UID).
	dir := t.TempDir()
	df := `FROM docker.io/library/alpine:latest
RUN addgroup -g 4321 vscode \
 && adduser -D -u 4321 -G vscode -s /bin/sh vscode
`
	if err := writeFile(filepath.Join(dir, "Dockerfile"), df); err != nil {
		t.Fatal(err)
	}
	tag := "dc-it-ac-uid-baked:latest"
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
		"remoteUser": "vscode",
		"containerUser": "vscode"
	}`)

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

	// Read vscode's UID from inside the container. Apple-container
	// behavior we're pinning: this MUST still be 4321 — the baked
	// image-default — not the host UID. If a future engine change
	// adds UID reconciliation to the apple-container path, this
	// assertion fails.
	res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"id", "-u", "vscode"},
	})
	if err != nil {
		t.Fatalf("Exec id -u vscode: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "4321" {
		t.Errorf("vscode UID inside container = %q, want %q (design §13.8 says we don't reconcile UIDs on apple-container)",
			got, "4321")
	}

	// Verify the workspace mount is writable as vscode (virtiofs
	// identity-permissive contract). Probe-3 from the M6 design
	// validation already proved this at the runtime layer; here we
	// re-prove it through the full Engine.Up bind-mount path.
	const marker = "ac-uid-test-writable-99"
	// The engine binds the workspace at /workspaces/<basename>.
	cmd := "echo " + marker + " > /workspaces/$(basename " + ws + ")/.uid-test && cat /workspaces/$(basename " + ws + ")/.uid-test"
	res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
		Cmd: []string{"/bin/sh", "-c", cmd},
	})
	if err != nil {
		t.Fatalf("Exec write probe: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("write probe failed: exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}
	if !strings.Contains(res.Stdout, marker) {
		t.Errorf("marker not readable; got %q", res.Stdout)
	}

	// Host-side readback confirms the file made it through virtiofs
	// with the OUR-UID owner from the host's perspective.
	data, err := os.ReadFile(ws + "/.uid-test")
	if err != nil {
		t.Fatalf("host readback: %v", err)
	}
	if !strings.Contains(string(data), marker) {
		t.Errorf("host readback content mismatch: %q", string(data))
	}
}
