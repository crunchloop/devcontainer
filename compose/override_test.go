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
			"Z_LAST": "z",
			"A_FIRST": "a",
			"M_MID":  "m",
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
