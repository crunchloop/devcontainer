//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
)

// TestEngineBuild_ImageSource_NoFeatures exercises the short-circuit:
// pure image source with no features should pull the image and return
// its ref unchanged, with ImageName ignored (there's nothing to retag
// — a TagImage primitive is a tracked follow-up).
func TestEngineBuild_ImageSource_NoFeatures(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeWorkspace(t, `{"image": "`+testImage+`"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := eng.Build(ctx, devcontainer.BuildOptions{
		LocalWorkspaceFolder: ws,
		ImageName:            "ignored-because-no-build-step:latest",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.ImageID != testImage {
		t.Errorf("ImageID = %q, want %q (ImageName should be ignored when no build step exists)", res.ImageID, testImage)
	}
}

// TestEngineBuild_BuildSource_WithFeature is the canonical happy path:
// a Dockerfile source plus a local feature. Build should run both
// stages (base Dockerfile, then feature-layered final image) and
// return the final tag. With ImageName set, the final image carries
// that tag.
func TestEngineBuild_BuildSource_WithFeature(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeBuildSourceWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const customTag = "dc-go-build-test:v1"
	res, err := eng.Build(ctx, devcontainer.BuildOptions{
		LocalWorkspaceFolder: ws,
		ImageName:            customTag,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.ImageID != customTag {
		t.Errorf("ImageID = %q, want %q", res.ImageID, customTag)
	}

	// Confirm the image actually exists locally — Build should not
	// have created or run any container, only produced the image.
	details, err := rt.InspectImage(ctx, res.ImageID)
	if err != nil {
		t.Fatalf("InspectImage(%s): %v", res.ImageID, err)
	}
	if details == nil {
		t.Fatalf("InspectImage(%s) returned nil", res.ImageID)
	}

	// Clean up the tagged image so subsequent runs don't accumulate.
	t.Cleanup(func() {
		_ = rt.RemoveImage(context.Background(), res.ImageID)
	})
}

// TestEngineBuild_BuildSource_NoFeatures: when there's a Dockerfile
// but no features, the base build IS the final build, so ImageName
// applies to it directly.
func TestEngineBuild_BuildSource_NoFeatures(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "Dockerfile"), "FROM "+testImage+"\nRUN echo build-no-features > /etc/marker\n")
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"),
		`{"build": {"dockerfile": "Dockerfile", "context": ".."}}`)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const tag = "dc-go-build-test-base-only:v1"
	res, err := eng.Build(ctx, devcontainer.BuildOptions{
		LocalWorkspaceFolder: dir,
		ImageName:            tag,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.ImageID != tag {
		t.Errorf("ImageID = %q, want %q", res.ImageID, tag)
	}
	t.Cleanup(func() { _ = rt.RemoveImage(context.Background(), tag) })
}

// TestEngineBuild_ComposeRefused: compose sources are refused with a
// clear error rather than silently doing nothing.
func TestEngineBuild_ComposeRefused(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "compose.yaml"),
		"services:\n  app:\n    image: "+testImage+"\n    command: [\"sleep\", \"infinity\"]\n")
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"),
		`{"dockerComposeFile": "../compose.yaml", "service": "app", "workspaceFolder": "/workspace"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := eng.Build(ctx, devcontainer.BuildOptions{LocalWorkspaceFolder: dir})
	if err == nil {
		t.Fatal("Build accepted a compose source; expected refusal")
	}
	if !strings.Contains(err.Error(), "compose") {
		t.Errorf("error %q should mention compose", err.Error())
	}
}

// TestEngineBuild_NoContainerCreated: Build must not leave a container
// behind for the workspace it builds — that's Up's job.
func TestEngineBuild_NoContainerCreated(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	ws := writeBuildSourceWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := eng.Build(ctx, devcontainer.BuildOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = rt.RemoveImage(context.Background(), res.ImageID) })

	// Resolve the workspace ID through a second pass; any container
	// labelled with it would indicate Build ran a container.
	cfg, err := devcontainer.Resolve(ctx, devcontainer.ResolveOptions{LocalWorkspaceFolder: ws})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	c, err := rt.FindContainerByLabel(ctx, devcontainer.LabelDevcontainerID, cfg.DevcontainerID)
	if err != nil {
		t.Fatalf("FindContainerByLabel: %v", err)
	}
	if c != nil {
		t.Errorf("Build left a container behind (id=%s) — should be image-only", c.ID)
	}
}
