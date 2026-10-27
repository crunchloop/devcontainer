//go:build integration

package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// dindFeatureRef is the upstream docker-in-docker feature. Its metadata
// declares the mount this test is about —
// {"source":"dind-var-lib-docker-${devcontainerId}","target":"/var/lib/docker","type":"volume"}
// — alongside privileged and the docker-init.sh entrypoint.
const dindFeatureRef = "ghcr.io/devcontainers/features/docker-in-docker:2"

// dindBaseImage is a Debian base the feature's install script supports.
const dindBaseImage = "debian:bookworm-slim"

// TestComposeSource_DockerInDockerRunsContainers is the end-to-end
// check for feature volume mounts on compose-source devcontainers.
// Before the fix only bind mounts from cfg.Mounts reached the compose
// service, so the feature's /var/lib/docker volume was dropped, the
// inner dockerd's data root sat on the container's overlay root, and
// `docker run` failed (overlay-on-overlay, EINVAL). Runs on both
// compose backends.
func TestComposeSource_DockerInDockerRunsContainers(t *testing.T) {
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

			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, "docker-compose.yml"), `
services:
  app:
    image: `+dindBaseImage+`
    command: ["sh", "-c", "while sleep 1000; do :; done"]
`)
			mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
				"dockerComposeFile": "../docker-compose.yml",
				"service": "app",
				"workspaceFolder": "/workspaces/proj",
				"features": { "`+dindFeatureRef+`": {} }
			}`)

			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
			defer cancel()

			wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
				LocalWorkspaceFolder: dir,
				Recreate:             true,
				SkipLifecycle:        true,
			})
			if err != nil {
				t.Fatalf("Up: %v", err)
			}

			// Both backends create the feature's named volume as a
			// project volume, labelled the way compose labels one.
			volKey := "dind-var-lib-docker-" + string(wsObj.ID)
			out, lsErr := exec.Command("docker", "volume", "ls", "-q",
				"--filter", "label=com.docker.compose.volume="+volKey).Output()
			vols := strings.Fields(string(out))
			t.Cleanup(func() {
				_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{
					Remove:        true,
					RemoveVolumes: true,
				})
				// Down does not remove every named volume on every
				// backend; remove the feature's volume explicitly.
				for _, v := range vols {
					_ = exec.Command("docker", "volume", "rm", "-f", v).Run()
				}
			})
			if lsErr != nil || len(vols) != 1 || !strings.HasSuffix(vols[0], "_"+volKey) {
				t.Fatalf("project volume for %s = %v (err %v), want exactly one <project>_%s", volKey, vols, lsErr, volKey)
			}

			var dockerMount bool
			for _, m := range wsObj.Container.Mounts {
				if m.Target == "/var/lib/docker" {
					dockerMount = true
					if m.Type != "volume" || !strings.Contains(m.Source, vols[0]) {
						t.Errorf("/var/lib/docker mount = %+v, want volume %s", m, vols[0])
					}
				}
			}
			if !dockerMount {
				t.Fatalf("no mount at /var/lib/docker: %+v", wsObj.Container.Mounts)
			}

			// dockerd is started in the background by the feature's
			// entrypoint; wait for it, then run a container.
			deadline := time.Now().Add(2 * time.Minute)
			for {
				res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
					Cmd: []string{"docker", "info", "--format", "{{.Driver}}"},
				})
				if err == nil && res.ExitCode == 0 {
					t.Logf("inner dockerd storage driver: %s", strings.TrimSpace(res.Stdout))
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("inner dockerd never became ready: err=%v res=%+v", err, res)
				}
				time.Sleep(2 * time.Second)
			}

			res, err := eng.Exec(ctx, wsObj, devcontainer.ExecOptions{
				Cmd: []string{"docker", "run", "--rm", "hello-world"},
			})
			if err != nil {
				t.Fatalf("Exec docker run: %v", err)
			}
			if res.ExitCode != 0 || !strings.Contains(res.Stdout, "Hello from Docker!") {
				t.Fatalf("docker run hello-world failed: exit=%d stdout=%q stderr=%q",
					res.ExitCode, res.Stdout, res.Stderr)
			}
		})
	}
}
