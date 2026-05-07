package compose

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeCompose(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_BasicProject(t *testing.T) {
	dir := t.TempDir()
	writeCompose(t, dir, "compose.yml", `
services:
  app:
    image: alpine:3.20
    environment:
      FOO: bar
`)

	project, err := Load(context.Background(), LoadOptions{
		Files:       []string{filepath.Join(dir, "compose.yml")},
		WorkingDir:  dir,
		ProjectName: "test",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if project.Name != "test" {
		t.Errorf("project name = %q", project.Name)
	}
	if len(project.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(project.Services))
	}
	app, err := PrimaryService(project, "app")
	if err != nil {
		t.Fatalf("PrimaryService: %v", err)
	}
	if app.Image != "alpine:3.20" {
		t.Errorf("Image = %q", app.Image)
	}
	if app.Environment["FOO"] == nil || *app.Environment["FOO"] != "bar" {
		t.Errorf("Environment[FOO] = %v", app.Environment["FOO"])
	}
}

func TestLoad_MultipleFilesOverride(t *testing.T) {
	dir := t.TempDir()
	writeCompose(t, dir, "compose.yml", `
services:
  app:
    image: alpine:3.20
    environment:
      ENV: prod
`)
	writeCompose(t, dir, "compose.override.yml", `
services:
  app:
    environment:
      ENV: dev
      EXTRA: yes
`)

	project, err := Load(context.Background(), LoadOptions{
		Files: []string{
			filepath.Join(dir, "compose.yml"),
			filepath.Join(dir, "compose.override.yml"),
		},
		WorkingDir:  dir,
		ProjectName: "test",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	app, _ := PrimaryService(project, "app")
	if app.Environment["ENV"] == nil || *app.Environment["ENV"] != "dev" {
		t.Errorf("ENV not overridden: %v", app.Environment["ENV"])
	}
	if _, ok := app.Environment["EXTRA"]; !ok {
		t.Error("EXTRA not merged")
	}
}

func TestLoad_PrimaryServiceMissing(t *testing.T) {
	dir := t.TempDir()
	writeCompose(t, dir, "compose.yml", `
services:
  db:
    image: postgres
`)
	project, err := Load(context.Background(), LoadOptions{
		Files:       []string{filepath.Join(dir, "compose.yml")},
		WorkingDir:  dir,
		ProjectName: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrimaryService(project, "nonexistent"); err == nil {
		t.Fatal("expected error for missing service")
	}
}

func TestLoad_ProfilesActivate(t *testing.T) {
	dir := t.TempDir()
	writeCompose(t, dir, "compose.yml", `
services:
  always:
    image: alpine
  optional:
    image: alpine
    profiles: ["dev"]
`)
	// Default (no profiles): "optional" is disabled.
	project, err := Load(context.Background(), LoadOptions{
		Files:       []string{filepath.Join(dir, "compose.yml")},
		WorkingDir:  dir,
		ProjectName: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrimaryService(project, "optional"); err == nil {
		t.Error("optional service should be disabled without profile")
	}
	// With profile activated: both services present.
	project2, err := Load(context.Background(), LoadOptions{
		Files:       []string{filepath.Join(dir, "compose.yml")},
		WorkingDir:  dir,
		ProjectName: "test",
		Profiles:    []string{"dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrimaryService(project2, "optional"); err != nil {
		t.Errorf("optional should be active with profile: %v", err)
	}
}

func TestLoad_RequiresFiles(t *testing.T) {
	if _, err := Load(context.Background(), LoadOptions{}); err == nil {
		t.Fatal("expected error for empty Files")
	}
}
