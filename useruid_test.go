package devcontainer

import (
	"context"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
)

func TestReconcileRemoteUserUID_SkipsWhenDisabled(t *testing.T) {
	rt := newFakeRuntime()
	eng := &Engine{runtime: rt}
	f := false
	cfg := &config.ResolvedConfig{
		LocalWorkspaceFolder: t.TempDir(),
		RemoteUser:           "vscode",
		UpdateRemoteUserUID:  &f,
	}
	got, err := eng.reconcileRemoteUserUID(context.Background(), cfg, "img:tag", UpOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "img:tag" {
		t.Fatalf("expected unchanged image, got %q", got)
	}
}

func TestReconcileRemoteUserUID_SkipsWhenUserIsRoot(t *testing.T) {
	rt := newFakeRuntime()
	eng := &Engine{runtime: rt}
	cfg := &config.ResolvedConfig{
		LocalWorkspaceFolder: t.TempDir(),
		RemoteUser:           "root",
	}
	got, err := eng.reconcileRemoteUserUID(context.Background(), cfg, "img:tag", UpOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "img:tag" {
		t.Fatalf("expected unchanged image, got %q", got)
	}
}

func TestGenerateUIDDockerfile_ContainsKeyDirectives(t *testing.T) {
	df := generateUIDDockerfile("base:latest", "vscode", 1001, 65534)
	for _, want := range []string{
		"FROM $_DEV_CONTAINERS_BASE_IMAGE",
		"ARG _DEV_CONTAINERS_BASE_IMAGE=base:latest",
		"ARG _REMOTE_USER=vscode",
		"ARG _REMOTE_USER_UID=1001",
		"ARG _REMOTE_USER_GID=65534",
		"COPY uid-fix.sh",
		"/bin/sh /tmp/uid-fix.sh",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("dockerfile missing %q\n--\n%s", want, df)
		}
	}
}

func TestUIDReconcileScript_PortableShape(t *testing.T) {
	// Sanity-check that the embedded script doesn't depend on shadow-utils
	// commands. Catches regressions if someone reintroduces usermod /
	// groupmod / getent — those break Alpine.
	for _, banned := range []string{"usermod", "groupmod", "getent"} {
		if strings.Contains(uidReconcileScript, banned) {
			t.Errorf("uidReconcileScript references %q; would break BusyBox/Alpine images", banned)
		}
	}
	// Required portable primitives.
	for _, want := range []string{"awk -F:", "sed -i", "/etc/passwd", "/etc/group"} {
		if !strings.Contains(uidReconcileScript, want) {
			t.Errorf("uidReconcileScript missing %q", want)
		}
	}
}
