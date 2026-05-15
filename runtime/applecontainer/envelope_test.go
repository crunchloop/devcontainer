//go:build darwin && arm64

package applecontainer

import (
	"errors"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/runtime"
)

// Pure-Go tests for the envelope decoder. No daemon, no cgo at
// runtime — exercises the JSON contract from the style guide.

func TestDecodeEnvelope_Success(t *testing.T) {
	raw := `{"ok":true,"data":{"reference":"alpine","digest":"sha256:abc","architecture":"arm64"}}`
	env, err := decodeEnvelope[imageInspectPayload](raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.OK {
		t.Error("OK: want true")
	}
	if env.decoded.Reference != "alpine" {
		t.Errorf("Reference: %q", env.decoded.Reference)
	}
	if env.decoded.Digest != "sha256:abc" {
		t.Errorf("Digest: %q", env.decoded.Digest)
	}
}

func TestDecodeEnvelope_Failure(t *testing.T) {
	raw := `{"ok":false,"err":"not found"}`
	_, err := decodeEnvelope[imageInspectPayload](raw)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err msg: %v", err)
	}
}

func TestDecodeEnvelope_NullData(t *testing.T) {
	// find-by-label miss path: ok=true but data is null.
	raw := `{"ok":true,"data":null}`
	env, err := decodeEnvelope[containerSnapshot](raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.OK {
		t.Error("OK: want true for a null-data miss")
	}
	if env.decoded.Configuration.ID != "" {
		t.Errorf("decoded container ID should be empty for null data, got %q",
			env.decoded.Configuration.ID)
	}
}

func TestDecodeEnvelope_Malformed(t *testing.T) {
	_, err := decodeEnvelope[imageInspectPayload](`{not-json}`)
	if err == nil {
		t.Fatal("want error for malformed JSON")
	}
}

// TestRejectUnsupportedRunSpec pins the contract that RunArgs,
// Privileged, and SecurityOpt fail fast with a typed error rather
// than being silently dropped at the bridge boundary.
func TestRejectUnsupportedRunSpec(t *testing.T) {
	cases := []struct {
		name string
		spec runtime.RunSpec
		opt  string
	}{
		{name: "RunArgs", spec: runtime.RunSpec{RunArgs: []string{"--add-host=foo:1.2.3.4"}}, opt: "RunArgs"},
		{name: "Privileged", spec: runtime.RunSpec{Privileged: true}, opt: "Privileged"},
		{name: "SecurityOpt", spec: runtime.RunSpec{SecurityOpt: []string{"no-new-privileges"}}, opt: "SecurityOpt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectUnsupportedRunSpec(tc.spec)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			var unsup *runtime.UnsupportedOptionError
			if !errors.As(err, &unsup) {
				t.Fatalf("want *UnsupportedOptionError, got %T: %v", err, err)
			}
			if unsup.Option != tc.opt {
				t.Errorf("Option = %q, want %q", unsup.Option, tc.opt)
			}
			if unsup.Backend != "applecontainer" {
				t.Errorf("Backend = %q, want %q", unsup.Backend, "applecontainer")
			}
		})
	}
}

// TestRejectUnsupportedRunSpec_AllowsZeroValue confirms the validator
// is a no-op for the common case (all unsupported fields empty).
func TestRejectUnsupportedRunSpec_AllowsZeroValue(t *testing.T) {
	if err := rejectUnsupportedRunSpec(runtime.RunSpec{Image: "x", Name: "y"}); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestMapState(t *testing.T) {
	cases := map[string]string{
		"running":  "running",
		"stopped":  "exited",
		"stopping": "removing",
		"unknown":  "",
		"garbage":  "",
	}
	for in, want := range cases {
		if got := mapState(in); string(got) != want {
			t.Errorf("mapState(%q): want %q got %q", in, want, got)
		}
	}
}
