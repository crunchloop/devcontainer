//go:build darwin && arm64

package applecontainer

import (
	"strings"
	"testing"
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
