package config

import (
	"errors"
	"testing"
)

func TestParseRaw_Plain(t *testing.T) {
	src := []byte(`{"image": "alpine:3.20"}`)
	raw, err := parseRaw(src, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if raw.Image != "alpine:3.20" {
		t.Errorf("Image = %q, want alpine:3.20", raw.Image)
	}
}

func TestParseRaw_JSONCFeatures(t *testing.T) {
	src := []byte(`{
		// line comment
		"image": "alpine:3.20", /* block comment */
		"runArgs": [
			"--init",
			"--privileged", // trailing-comma + comment combo
		],
	}`)
	raw, err := parseRaw(src, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if raw.Image != "alpine:3.20" {
		t.Errorf("Image = %q", raw.Image)
	}
	if len(raw.RunArgs) != 2 {
		t.Errorf("RunArgs len = %d, want 2", len(raw.RunArgs))
	}
}

func TestParseRaw_InvalidJSON(t *testing.T) {
	src := []byte(`{"image": }`)
	_, err := parseRaw(src, "/path/to/devcontainer.json")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	var pe *ConfigParseError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ConfigParseError, got %T", err)
	}
	if pe.Path != "/path/to/devcontainer.json" {
		t.Errorf("path = %q, want /path/to/devcontainer.json", pe.Path)
	}
	if pe.Unwrap() == nil {
		t.Error("expected wrapped error to be non-nil")
	}
}
