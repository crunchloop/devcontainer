//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// TestFeatureMountSubstitution is the integration repro for #52:
// a feature-contributed mount whose source contains ${devcontainerId}
// must have the placeholder resolved before reaching Docker. Without the
// fix, ContainerCreate fails with "includes invalid characters for a
// local volume name" because Docker sees the literal `${devcontainerId}`.
func TestFeatureMountSubstitution(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"image": "`+testImage+`",
		"features": { "./local-feature": {} }
	}`)

	// Mirror the docker-in-docker shape from the bug report: a volume
	// mount whose source embeds ${devcontainerId}.
	featureDir := filepath.Join(dir, ".devcontainer", "local-feature")
	mustWrite(t, filepath.Join(featureDir, "devcontainer-feature.json"), `{
		"id": "test-feat-mount-subst",
		"version": "1.0.0",
		"mounts": [{
			"source": "dc-feat-vol-${devcontainerId}",
			"target": "/feat-data",
			"type": "volume"
		}]
	}`)
	mustWrite(t, filepath.Join(featureDir, "install.sh"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(featureDir, "install.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: dir,
		Recreate:             true,
		SkipLifecycle:        true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	wantSource := "dc-feat-vol-" + string(wsObj.ID)
	t.Cleanup(func() {
		_ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true})
		// Named volumes are not removed by `docker rm -v`; clean up
		// explicitly so test runs don't accumulate volumes.
		_ = exec.Command("docker", "volume", "rm", "-f", wantSource).Run()
	})

	var got string
	for _, m := range wsObj.Container.Mounts {
		if m.Target == "/feat-data" {
			got = m.Source
			break
		}
	}
	if got == "" {
		t.Fatalf("/feat-data mount not present on container; mounts=%+v", wsObj.Container.Mounts)
	}
	// Docker reports volume mount sources as a host path
	// (/var/lib/docker/volumes/<name>/_data). The substituted volume
	// name must appear somewhere in that path; a literal `${` would
	// indicate the substitution never ran (and Up would have already
	// failed at ContainerCreate, since Docker rejects `${` in volume
	// names).
	if strings.Contains(got, "${") {
		t.Errorf("feature mount source still contains placeholder: %q", got)
	}
	if !strings.Contains(got, wantSource) {
		t.Errorf("feature mount source = %q, want substring %q", got, wantSource)
	}
}
