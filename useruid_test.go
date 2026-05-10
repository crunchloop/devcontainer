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
		"usermod --uid",
		"groupmod --gid",
		"chown -R",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("dockerfile missing %q\n--\n%s", want, df)
		}
	}
}
