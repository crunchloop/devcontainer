//go:build darwin && arm64

package applecontainer

import (
	"context"
	"errors"
	"testing"

	"github.com/crunchloop/devcontainer/runtime"
)

// TestBuildImage_PartialContract documents PR-G's stub shape: if the
// builder isn't running we return a typed BuilderUnavailableError; if
// it IS running we return a non-typed error explaining the build path
// isn't implemented yet. Either outcome is acceptable in CI — the
// test asserts we got SOMETHING (not a nil error, not a silent
// success) so the next PR notices when the real implementation
// changes the contract.
func TestBuildImage_PartialContract(t *testing.T) {
	rt := runtimeOrSkip(t)
	_, err := rt.BuildImage(context.Background(), runtime.BuildSpec{
		ContextPath: "/tmp/no-such-context",
		Dockerfile:  "Dockerfile",
		Tag:         "ac-build-test:latest",
	}, nil)
	if err == nil {
		t.Fatal("BuildImage: want error (PR-G is partial), got nil")
	}

	var unavail *runtime.BuilderUnavailableError
	if errors.As(err, &unavail) {
		t.Logf("builder not running (typed error path): %v", err)
		if unavail.Hint == "" {
			t.Errorf("BuilderUnavailableError.Hint is empty; should suggest `container builder start`")
		}
		return
	}

	// Otherwise: builder is up, and we got the "not implemented yet"
	// error. Sanity-check the message references both the not-yet-
	// implemented status and the workaround.
	msg := err.Error()
	if !contains(msg, "not yet implemented") {
		t.Errorf("BuildImage error should mention 'not yet implemented'; got %q", msg)
	}
	if !contains(msg, "image:") {
		t.Errorf("BuildImage error should mention the `image:` workaround; got %q", msg)
	}
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }
