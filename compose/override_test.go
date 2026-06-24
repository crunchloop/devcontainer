package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteBuildOverride_BasicShape(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dc-build.yaml")
	if err := WriteBuildOverride(dst, Override{
		Service: "app",
		Image:   "dc-go-final-abc:latest",
	}); err != nil {
		t.Fatalf("WriteBuildOverride: %v", err)
	}
	body, _ := os.ReadFile(dst)
	str := string(body)
	for _, want := range []string{
		"services:",
		"  app:",
		"image: dc-go-final-abc:latest",
		"build: !reset null",
	} {
		if !strings.Contains(str, want) {
			t.Errorf("dc-build.yaml missing %q\n%s", want, str)
		}
	}
}

func TestWriteBuildOverride_RequiresImage(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "x.yaml")
	if err := WriteBuildOverride(dst, Override{Service: "app"}); err == nil {
		t.Fatal("expected error when Image is empty")
	}
}

func TestWriteBuildOverride_QuotesIfNeeded(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dc-build.yaml")
	if err := WriteBuildOverride(dst, Override{
		Service: "app",
		Image:   "registry.example.com:5000/org/image:tag with spaces",
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(dst)
	if !strings.Contains(string(body), `"registry.example.com:5000/org/image:tag with spaces"`) {
		t.Errorf("expected quoted image; got\n%s", body)
	}
}

func TestWriteRunOverride_NoExistingProject(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dc-run.yaml")
	err := WriteRunOverride(dst, nil, Override{
		Service: "app",
		ExtraBindMounts: []BindMount{
			{Source: "/host/proj", Target: "/workspaces/proj"},
		},
		ExtraEnvironment: map[string]string{
			"WORKSPACE": "/workspaces/proj",
			"FOO":       "bar",
		},
		Labels: map[string]string{
			"dev.containers.id":     "abc123",
			"dev.containers.engine": "devcontainer-go/0.1",
		},
	})
	if err != nil {
		t.Fatalf("WriteRunOverride: %v", err)
	}
	body, _ := os.ReadFile(dst)
	str := string(body)

	for _, want := range []string{
		"services:",
		"app:",
		"volumes:",
		"type: bind",
		"source: /host/proj",
		"target: /workspaces/proj",
		"environment:",
		"FOO: bar",
		"WORKSPACE: /workspaces/proj",
		"labels:",
		"dev.containers.engine: devcontainer-go/0.1",
		"dev.containers.id: abc123",
	} {
		if !strings.Contains(str, want) {
			t.Errorf("dc-run.yaml missing %q\n%s", want, str)
		}
	}
}

func TestWriteRunOverride_PreservesExistingVolumes(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(composeFile, []byte(`
services:
  app:
    image: alpine
    volumes:
      - type: bind
        source: /user/host
        target: /user/container
      - type: volume
        source: shared-data
        target: /data

volumes:
  shared-data: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := Load(context.Background(), LoadOptions{
		Files: []string{composeFile}, WorkingDir: dir, ProjectName: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dc-run.yaml")
	err = WriteRunOverride(dst, project, Override{
		Service: "app",
		ExtraBindMounts: []BindMount{
			{Source: "/host/proj", Target: "/workspaces/proj"},
		},
	})
	if err != nil {
		t.Fatalf("WriteRunOverride: %v", err)
	}
	body, _ := os.ReadFile(dst)
	str := string(body)

	// User's volumes preserved alongside ours.
	for _, want := range []string{
		"source: /user/host",
		"target: /user/container",
		"source: shared-data",
		"target: /data",
		"source: /host/proj",
		"target: /workspaces/proj",
	} {
		if !strings.Contains(str, want) {
			t.Errorf("expected volume %q in merged list:\n%s", want, str)
		}
	}
}

func TestWriteRunOverride_EmitsSecurityMetadata(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(composeFile, []byte(`
services:
  app:
    image: alpine
    cap_add:
      - NET_ADMIN
`), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := Load(context.Background(), LoadOptions{
		Files: []string{composeFile}, WorkingDir: dir, ProjectName: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	priv := true
	initT := true
	dst := filepath.Join(dir, "dc-run.yaml")
	if err := WriteRunOverride(dst, project, Override{
		Service:     "app",
		Privileged:  &priv,
		Init:        &initT,
		CapAdd:      []string{"SYS_ADMIN"},
		SecurityOpt: []string{"seccomp=unconfined"},
	}); err != nil {
		t.Fatalf("WriteRunOverride: %v", err)
	}
	body, _ := os.ReadFile(dst)
	str := string(body)

	for _, want := range []string{
		"privileged: true",
		"init: true",
		"cap_add:",
		"- NET_ADMIN", // user's existing cap preserved (union)
		"- SYS_ADMIN", // feature cap added
		"security_opt:",
		"- seccomp=unconfined",
	} {
		if !strings.Contains(str, want) {
			t.Errorf("dc-run.yaml missing %q\n%s", want, str)
		}
	}
}

func TestWriteRunOverride_OmitsUnsetSecurityMetadata(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dc-run.yaml")
	if err := WriteRunOverride(dst, nil, Override{
		Service:          "app",
		ExtraEnvironment: map[string]string{"X": "1"},
	}); err != nil {
		t.Fatalf("WriteRunOverride: %v", err)
	}
	body, _ := os.ReadFile(dst)
	str := string(body)
	for _, unwanted := range []string{"privileged", "init:", "cap_add", "security_opt"} {
		if strings.Contains(str, unwanted) {
			t.Errorf("dc-run.yaml unexpectedly contains %q\n%s", unwanted, str)
		}
	}
}

func TestRenderEntrypointWrapper_NativeVsEscaped(t *testing.T) {
	eps := []string{"/usr/local/share/docker-init.sh"}

	// Native (in-memory, no compose interpolation): single-dollar.
	native := RenderEntrypointWrapper(eps, nil, false)
	if len(native) != 4 {
		t.Fatalf("native wrapper = %v, want 4 elements", native)
	}
	if native[0] != "/bin/sh" || native[1] != "-c" || native[3] != "-" {
		t.Errorf("wrapper shape wrong: %v", native)
	}
	if !strings.Contains(native[2], `exec "$@"`) {
		t.Errorf("native script missing single-dollar exec: %q", native[2])
	}
	if !strings.Contains(native[2], "/usr/local/share/docker-init.sh") {
		t.Errorf("native script missing feature entrypoint: %q", native[2])
	}
	if strings.Contains(native[2], "$$") {
		t.Errorf("native script must NOT double-escape dollars: %q", native[2])
	}

	// Escaped (shellout YAML, re-interpolated by docker compose): $$.
	escaped := RenderEntrypointWrapper(eps, nil, true)
	if !strings.Contains(escaped[2], `exec "$$@"`) {
		t.Errorf("escaped script missing $$@: %q", escaped[2])
	}
	if !strings.Contains(escaped[2], "$$!") {
		t.Errorf("escaped script missing $$!: %q", escaped[2])
	}
}

func TestRenderEntrypointWrapper_PreservesOriginalEntrypoint(t *testing.T) {
	w := RenderEntrypointWrapper(
		[]string{"/init.sh"},
		[]string{"/my/orig-entrypoint", "--flag"},
		false,
	)
	// Original tokens appended after the "-" sentinel so `exec "$@"` runs them.
	if w[3] != "-" || w[4] != "/my/orig-entrypoint" || w[5] != "--flag" {
		t.Errorf("original entrypoint not appended after sentinel: %v", w)
	}
}

func TestWriteRunOverride_EmitsEntrypointWrapper(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dc-run.yaml")
	if err := WriteRunOverride(dst, nil, Override{
		Service:     "app",
		Entrypoints: []string{"/usr/local/share/docker-init.sh"},
	}); err != nil {
		t.Fatalf("WriteRunOverride: %v", err)
	}
	body, _ := os.ReadFile(dst)
	str := string(body)
	for _, want := range []string{
		"entrypoint:",
		"/bin/sh",
		"docker-init.sh",
		"$$@", // doubled so it survives docker compose interpolation as $@
	} {
		if !strings.Contains(str, want) {
			t.Errorf("dc-run.yaml missing %q\n%s", want, str)
		}
	}
	// Must NOT contain a bare single-dollar $@ that compose would mangle.
	if strings.Contains(str, `"$@"`) {
		t.Errorf("dc-run.yaml has un-escaped $@ that compose would interpolate:\n%s", str)
	}
}

func TestWriteRunOverride_RequiresService(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "x.yaml")
	if err := WriteRunOverride(dst, nil, Override{}); err == nil {
		t.Fatal("expected error when Service is empty")
	}
}

func TestWriteRunOverride_DeterministicEnvOrder(t *testing.T) {
	// yaml.v3 sorts map keys, but we explicitly use sortedKeys to be
	// safe against future yaml-lib changes. Two runs with the same
	// inputs should produce byte-identical output.
	dst1 := filepath.Join(t.TempDir(), "a.yaml")
	dst2 := filepath.Join(t.TempDir(), "b.yaml")
	ov := Override{
		Service: "app",
		ExtraEnvironment: map[string]string{
			"Z_LAST":  "z",
			"A_FIRST": "a",
			"M_MID":   "m",
		},
	}
	_ = WriteRunOverride(dst1, nil, ov)
	_ = WriteRunOverride(dst2, nil, ov)
	a, _ := os.ReadFile(dst1)
	b, _ := os.ReadFile(dst2)
	if string(a) != string(b) {
		t.Errorf("output not deterministic:\n%s\n---\n%s", a, b)
	}
}
