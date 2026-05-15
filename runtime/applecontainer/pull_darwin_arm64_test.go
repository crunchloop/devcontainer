//go:build darwin && arm64

package applecontainer

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/runtime"
)

// TestPullImage_SmallPublic pulls a tiny public image. Skips if the
// daemon is unreachable. Verifies the ImageRef + the BuildEvent
// surface (one log + one completed event, both non-blocking).
//
// Pre-removes the image first so we test the real pull path rather
// than the local-cache hit (which Apple resolves without an upstream
// fetch).
func TestPullImage_SmallPublic(t *testing.T) {
	rt := runtimeOrSkip(t)
	ctx := context.Background()
	const ref = "docker.io/library/alpine:latest"

	// Pre-cleanup: try to delete via CLI. Best-effort.
	cliRun(t, "images", "rm", ref)
	cliRun(t, "image", "rm", ref)

	events := make(chan runtime.BuildEvent, 16)
	imageRef, err := rt.PullImage(ctx, ref, events)
	close(events)

	if err != nil {
		// If `container images rm` isn't available, the image may
		// still be cached from earlier tests and pull will still
		// succeed. If it actually fails, surface it.
		t.Fatalf("PullImage: %v", err)
	}

	if imageRef.ID == "" {
		t.Errorf("ImageRef.ID is empty")
	}
	if len(imageRef.Tags) == 0 || imageRef.Tags[0] == "" {
		t.Errorf("ImageRef.Tags: want non-empty, got %v", imageRef.Tags)
	}

	// We accept anywhere from 1 (just completed, with the slow-
	// consumer guard dropping the log) to 2 events. Just verify at
	// least the completed event made it through.
	var sawCompleted bool
	for ev := range events {
		if ev.Kind == runtime.BuildEventCompleted {
			sawCompleted = true
			if ev.Digest == "" {
				t.Errorf("BuildEventCompleted.Digest is empty")
			}
		}
	}
	if !sawCompleted {
		t.Errorf("BuildEventCompleted not emitted")
	}
}

// TestPullImage_NoSuchImage asserts the typed-error contract — must
// translate Apple's "notFound" into runtime.ImageNotFoundError so
// engine code can do typed dispatch.
func TestPullImage_NoSuchImage(t *testing.T) {
	rt := runtimeOrSkip(t)
	_, err := rt.PullImage(context.Background(),
		"docker.io/library/does-not-exist-zzz-99:latest", nil)
	if err == nil {
		t.Fatal("PullImage: want error, got nil")
	}
	var nf *runtime.ImageNotFoundError
	if !errors.As(err, &nf) {
		// Some registry-level errors don't surface as "notFound" —
		// network refusal, auth — and we don't want to falsely claim
		// not-found in those cases. Accept any error here, but log
		// the type so a real regression surfaces in CI output.
		t.Logf("non-typed pull error (acceptable for genuine network failures): %T %v", err, err)
		// Best-effort assertion: real "missing image" errors should
		// reach this branch. If they don't, the test still passes
		// because some valid network errors land here too — but the
		// CI log will show which.
		if isLikelyMissingImage(err) {
			t.Errorf("error looks like a missing-image error but wasn't typed: %v", err)
		}
	}
}

func isLikelyMissingImage(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "notfound") ||
		strings.Contains(msg, "no such")
}

// cliRun reused from inspect_darwin_arm64_test.go — best-effort
// `container ...` wrapper. Re-declared here would be a duplicate;
// the import of os/exec keeps this file self-contained for the cases
// where the helper isn't useful (e.g. when running this test file
// in isolation in IDEs that don't load sibling files).
var _ = exec.LookPath
