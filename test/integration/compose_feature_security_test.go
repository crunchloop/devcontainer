//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// writeComposeSecurityWorkspace creates a compose-source workspace whose
// primary service declares NO security options itself, plus a local
// feature that declares privileged / init / capAdd / securityOpt in its
// metadata. It exercises the parity fix: feature-declared security
// metadata must be applied to the compose service (mirroring what the
// image/Dockerfile path does and what upstream devcontainers/cli does via
// its generated override compose file). Without the fix the container
// comes up unprivileged and a feature like docker-in-docker silently
// fails.
func writeComposeSecurityWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Note: the user's compose service does NOT set privileged/cap_add —
	// the values must come purely from the feature metadata.
	mustWrite(t, filepath.Join(dir, "docker-compose.yml"), `
services:
  app:
    image: `+testImage+`
    command: ["sh", "-c", "while sleep 1000; do :; done"]
`)

	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"dockerComposeFile": "../docker-compose.yml",
		"service": "app",
		"workspaceFolder": "/workspaces/proj",
		"features": { "./sec-feature": {} }
	}`)

	featureDir := filepath.Join(dir, ".devcontainer", "sec-feature")
	mustWrite(t, filepath.Join(featureDir, "devcontainer-feature.json"), `{
		"id": "sec-feature",
		"version": "1.0.0",
		"privileged": true,
		"init": true,
		"capAdd": ["SYS_ADMIN"],
		"securityOpt": ["seccomp=unconfined"],
		"entrypoint": "/usr/local/share/sec-entrypoint.sh"
	}`)
	// install.sh: drop the marker AND install an entrypoint script that
	// runs at container start (proving feature-entrypoint chaining). The
	// entrypoint must NOT exec/block — it records that it ran and returns
	// so the wrapper goes on to exec the service command.
	mustWrite(t, filepath.Join(featureDir, "install.sh"), `#!/bin/sh
set -e
echo sec-feature-ran > /etc/sec-feature-marker
cat > /usr/local/share/sec-entrypoint.sh <<'EOF'
#!/bin/sh
echo sec-entrypoint-ran > /tmp/sec-entrypoint-marker
EOF
chmod +x /usr/local/share/sec-entrypoint.sh
`)
	if err := os.Chmod(filepath.Join(featureDir, "install.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestComposeSource_FeatureSecurityOptionsApplied confirms that a
// feature's privileged/init/capAdd/securityOpt land on the real primary
// container for BOTH compose backends (native = in-memory project mutate;
// shellout = generated YAML override). Asserts against the backend's
// inspect (HostConfig), which is the source of truth for what was
// actually applied.
func TestComposeSource_FeatureSecurityOptionsApplied(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	backends := []struct {
		name    string
		backend devcontainer.ComposeBackend
	}{
		{"native", devcontainer.ComposeBackendNative},
		{"shellout", devcontainer.ComposeBackendShellout},
	}

	for _, bk := range backends {
		t.Run(bk.name, func(t *testing.T) {
			eng, rt := newEngineWith(t, devcontainer.EngineOptions{
				ComposeBackend: bk.backend,
			})
			defer rt.Close()

			ws := writeComposeSecurityWorkspace(t)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
				LocalWorkspaceFolder: ws,
				Recreate:             true,
				SkipLifecycle:        true,
			})
			if err != nil {
				t.Fatalf("Up: %v", err)
			}
			defer func() {
				_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{
					Remove:        true,
					RemoveVolumes: true,
				})
			}()

			if wsObj.Container == nil {
				t.Fatal("Workspace.Container is nil")
			}

			details, err := rt.InspectContainer(ctx, wsObj.Container.ID)
			if err != nil {
				t.Fatalf("InspectContainer: %v", err)
			}

			if !details.Privileged {
				t.Error("container is not privileged; feature privileged:true was not applied to the compose service")
			}
			// Docker/podman normalize capability names to the CAP_-prefixed
			// form in inspect output (SYS_ADMIN -> CAP_SYS_ADMIN), so compare
			// with the prefix stripped.
			if !hasCapability(details.CapAdd, "SYS_ADMIN") {
				t.Errorf("CapAdd = %v, want it to contain SYS_ADMIN", details.CapAdd)
			}
			if !containsStr(details.SecurityOpt, "seccomp=unconfined") {
				t.Errorf("SecurityOpt = %v, want it to contain seccomp=unconfined", details.SecurityOpt)
			}

			// Sanity: the feature actually installed (proves we inspected
			// the right container and the feature layer ran).
			res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
				Cmd: []string{"cat", "/etc/sec-feature-marker"},
			})
			if err != nil {
				t.Fatalf("Exec marker: %v", err)
			}
			if res.ExitCode != 0 {
				t.Errorf("sec-feature-marker missing: stderr=%q", res.Stderr)
			}

			// Feature-entrypoint chaining: the feature's entrypoint script
			// must have run at container start (before the service command,
			// which is still running — proving the wrapper exec'd it).
			res, err = eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
				Cmd: []string{"cat", "/tmp/sec-entrypoint-marker"},
			})
			if err != nil {
				t.Fatalf("Exec entrypoint marker: %v", err)
			}
			if res.ExitCode != 0 || !strings.Contains(res.Stdout, "sec-entrypoint-ran") {
				t.Errorf("feature entrypoint did not run: exit=%d stdout=%q stderr=%q",
					res.ExitCode, res.Stdout, res.Stderr)
			}
		})
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// hasCapability reports whether caps contains the given capability,
// ignoring the optional CAP_ prefix the daemon adds in inspect output.
func hasCapability(caps []string, want string) bool {
	want = strings.TrimPrefix(want, "CAP_")
	for _, c := range caps {
		if strings.TrimPrefix(c, "CAP_") == want {
			return true
		}
	}
	return false
}
