//go:build darwin && arm64

package applecontainer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/runtime"
)

// TestPing_DaemonRunning round-trips a real ClientHealthCheck.ping
// through the Swift bridge to the system container-apiserver. Requires
// `container system start` to have been run on the host. Skips if the
// daemon is not reachable.
func TestPing_DaemonRunning(t *testing.T) {
	ctx := context.Background()
	rt, err := New(ctx, Options{PingTimeoutSeconds: 3})
	if err != nil {
		var unavail *runtime.DaemonUnavailableError
		if errors.As(err, &unavail) {
			t.Skipf("daemon not reachable (run `container system start`): %v", err)
		}
		t.Fatalf("New: %v", err)
	}

	if got := rt.BridgeVersion(); !strings.HasPrefix(got, "ACBridge/") {
		t.Errorf("bridge version: want prefix %q, got %q", "ACBridge/", got)
	}

	res, err := rt.Ping(ctx, 3)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.APIServerVersion == "" {
		t.Errorf("Ping: empty apiServerVersion (got %+v)", res)
	}
	if res.InstallRoot == "" {
		t.Errorf("Ping: empty installRoot (got %+v)", res)
	}
}

// TestNew_NoDaemon_ReturnsTypedError verifies the typed error path by
// invoking Ping with a 0-second timeout against the bridge — this
// either succeeds (daemon is up) or returns a typed
// DaemonUnavailableError. Both outcomes are acceptable; we just don't
// want an untyped error to leak through.
func TestNew_TypedErrorOnFailure(t *testing.T) {
	ctx := context.Background()
	_, err := New(ctx, Options{PingTimeoutSeconds: 0})
	if err == nil {
		return
	}
	var unavail *runtime.DaemonUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("expected *runtime.DaemonUnavailableError, got %T: %v", err, err)
	}
}
