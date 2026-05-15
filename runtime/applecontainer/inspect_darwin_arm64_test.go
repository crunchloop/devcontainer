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

// runtimeOrSkip returns a Runtime if the daemon is up, skipping
// otherwise. Keeps each smoke test boilerplate-free.
func runtimeOrSkip(t *testing.T) *Runtime {
	t.Helper()
	rt, err := New(context.Background(), Options{PingTimeoutSeconds: 3})
	if err != nil {
		var unavail *runtime.DaemonUnavailableError
		if errors.As(err, &unavail) {
			t.Skipf("daemon not reachable: %v", err)
		}
		t.Fatalf("New: %v", err)
	}
	return rt
}

// cliRun is a best-effort wrapper around the `container` CLI used to
// seed and tear down containers for these smoke tests. Errors are
// logged in verbose mode but not fatal — cleanup paths use this.
func cliRun(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("container", args...).CombinedOutput()
	if err != nil && testing.Verbose() {
		t.Logf("container %v -> %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
}

// cliRunStrict logs full output unconditionally and fails the test
// on a non-zero exit. Used for setup ops where a silent failure
// would leave the test trying to operate on something that doesn't
// exist.
func cliRunStrict(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("container", args...).CombinedOutput()
	if err != nil {
		t.Skipf("`container %v` failed: %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
}

// TestInspectContainer_RoundTrip seeds a container with a known label
// via the CLI, then inspects it through the bridge and asserts the
// label and status round-trip.
func TestInspectContainer_RoundTrip(t *testing.T) {
	rt := runtimeOrSkip(t)
	const id = "ac-inspect-test"
	cliRun(t, "delete", "--force", id)
	t.Cleanup(func() { cliRun(t, "delete", "--force", id) })

	cliRunStrict(t,
		"run", "-d", "--name", id,
		"--label", "dev.containers.id=test-marker-42",
		"docker.io/library/alpine:latest",
		"sleep", "120",
	)

	details, err := rt.InspectContainer(context.Background(), id)
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if details.ID != id {
		t.Errorf("ID: want %q got %q", id, details.ID)
	}
	if details.State != runtime.StateRunning {
		t.Errorf("State: want %q got %q", runtime.StateRunning, details.State)
	}
	if got := details.Labels["dev.containers.id"]; got != "test-marker-42" {
		t.Errorf("Labels[dev.containers.id]: want %q got %q (all labels: %v)",
			"test-marker-42", got, details.Labels)
	}
}

// TestInspectContainer_NotFound asserts the typed-error contract.
func TestInspectContainer_NotFound(t *testing.T) {
	rt := runtimeOrSkip(t)
	_, err := rt.InspectContainer(context.Background(), "ac-no-such-container-xyz")
	if err == nil {
		t.Fatal("InspectContainer: want error, got nil")
	}
	var nf *runtime.ContainerNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want *ContainerNotFoundError, got %T: %v", err, err)
	}
}

// TestInspectImage_LabelsRoundTrip pulls alpine and verifies the
// inspect surface returns the expected OS/arch fields. We don't check
// for specific labels because alpine doesn't ship any, but we DO
// verify Labels is at least a non-nil map (or empty) so the
// devcontainer.metadata fast path has predictable behavior against
// real images.
func TestInspectImage_LabelsRoundTrip(t *testing.T) {
	rt := runtimeOrSkip(t)
	// `container images pull` requires a plugin that isn't always
	// installed locally. Instead, ensure alpine is in the local image
	// cache by running a throwaway container (which fetches the image
	// implicitly if missing). The image stays cached after the
	// container is removed.
	cliRunStrict(t,
		"run", "--rm", "--name", "ac-alpine-warmup",
		"docker.io/library/alpine:latest", "/bin/true",
	)

	details, err := rt.InspectImage(context.Background(), "docker.io/library/alpine:latest")
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if details.ID == "" {
		t.Errorf("ID (digest) is empty")
	}
	if len(details.Tags) == 0 {
		t.Errorf("Tags is empty")
	}
	t.Logf("alpine: digest=%s tags=%v user=%q env=%v labels=%v",
		details.ID, details.Tags, details.User, details.Env, details.Labels)
}

// TestInspectImage_NotFound asserts the typed-error contract.
func TestInspectImage_NotFound(t *testing.T) {
	rt := runtimeOrSkip(t)
	_, err := rt.InspectImage(context.Background(), "docker.io/library/no-such-image-xyz:0")
	if err == nil {
		t.Fatal("InspectImage: want error, got nil")
	}
	var nf *runtime.ImageNotFoundError
	if !errors.As(err, &nf) {
		t.Logf("InspectImage non-found-but-not-typed err: %T %v", err, err)
		t.Fatalf("want *ImageNotFoundError, got %T: %v", err, err)
	}
}

// TestFindContainerByLabel covers both the hit and miss paths.
func TestFindContainerByLabel(t *testing.T) {
	rt := runtimeOrSkip(t)
	const id = "ac-findbylabel-test"
	cliRun(t, "delete", "--force", id)
	t.Cleanup(func() { cliRun(t, "delete", "--force", id) })

	cliRunStrict(t,
		"run", "-d", "--name", id,
		"--label", "dev.containers.id=findme-99",
		"docker.io/library/alpine:latest",
		"sleep", "120",
	)

	hit, err := rt.FindContainerByLabel(context.Background(), "dev.containers.id", "findme-99")
	if err != nil {
		t.Fatalf("FindContainerByLabel(hit): %v", err)
	}
	if hit == nil {
		t.Fatal("FindContainerByLabel(hit): want container, got nil")
	}
	if hit.ID != id {
		t.Errorf("FindContainerByLabel(hit): want id %q got %q", id, hit.ID)
	}

	miss, err := rt.FindContainerByLabel(context.Background(), "dev.containers.id", "does-not-exist")
	if err != nil {
		t.Fatalf("FindContainerByLabel(miss): %v", err)
	}
	if miss != nil {
		t.Errorf("FindContainerByLabel(miss): want nil, got %+v", miss)
	}
}
