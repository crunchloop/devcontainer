package config

import (
	"errors"
	"testing"
)

func TestParseRaw_Plain(t *testing.T) {
	src := []byte(`{"image": "alpine:3.20"}`)
	raw, _, err := parseRaw(src, "")
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
	raw, _, err := parseRaw(src, "")
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

func TestParseRaw_UnknownTopLevelFieldEmitsWarning(t *testing.T) {
	src := []byte(`{"image":"alpine:3.20","wibble":42}`)
	raw, warns, err := parseRaw(src, "")
	if err != nil {
		t.Fatalf("parseRaw: %v", err)
	}
	if raw.Image != "alpine:3.20" {
		t.Errorf("Image = %q", raw.Image)
	}
	found := false
	for _, w := range warns {
		if w.Code == WarnUnknownField && w.Path == "/wibble" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected WarnUnknownField for /wibble, got %v", warns)
	}
}

func TestParseRaw_UnknownNestedBuildFieldEmitsWarning(t *testing.T) {
	// Real typo (not just casing — encoding/json is case-insensitive,
	// so "dockerFile" actually decodes correctly into Dockerfile).
	src := []byte(`{"build":{"dockerfile":"Dockerfile","contxt":"."}}`)
	raw, warns, err := parseRaw(src, "")
	if err != nil {
		t.Fatalf("parseRaw: %v", err)
	}
	if raw.Build == nil || raw.Build.Dockerfile != "Dockerfile" {
		t.Errorf("expected raw.Build.Dockerfile populated, got %+v", raw.Build)
	}
	if raw.Build.Context != "" {
		t.Errorf("expected raw.Build.Context empty (typo), got %q", raw.Build.Context)
	}
	found := false
	for _, w := range warns {
		if w.Code == WarnUnknownField && w.Path == "/build/contxt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected WarnUnknownField for /build/contxt, got %v", warns)
	}
}

func TestParseRaw_CaseVariantOfKnownFieldDoesNotWarn(t *testing.T) {
	// encoding/json matches case-insensitively, so "dockerFile" populates
	// "dockerfile". Don't claim it was ignored.
	src := []byte(`{"build":{"dockerFile":"Dockerfile"}}`)
	raw, warns, err := parseRaw(src, "")
	if err != nil {
		t.Fatalf("parseRaw: %v", err)
	}
	if raw.Build == nil || raw.Build.Dockerfile != "Dockerfile" {
		t.Errorf("expected Dockerfile populated via case-insensitive match, got %+v", raw.Build)
	}
	for _, w := range warns {
		if w.Code == WarnUnknownField {
			t.Errorf("unexpected WarnUnknownField: %+v", w)
		}
	}
}

func TestParseRaw_UnknownHostRequirementsFieldEmitsWarning(t *testing.T) {
	src := []byte(`{"hostRequirements":{"cpus":2,"ram":"4gb","gpu":{"optional":true,"strange":1}}}`)
	_, warns, err := parseRaw(src, "")
	if err != nil {
		t.Fatalf("parseRaw: %v", err)
	}
	wantPaths := map[string]bool{
		"/hostRequirements/ram":         false,
		"/hostRequirements/gpu/strange": false,
	}
	for _, w := range warns {
		if w.Code == WarnUnknownField {
			if _, ok := wantPaths[w.Path]; ok {
				wantPaths[w.Path] = true
			}
		}
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Errorf("expected WarnUnknownField for %s, got %v", p, warns)
		}
	}
}

func TestParseRaw_KnownFieldsProduceNoWarnings(t *testing.T) {
	src := []byte(`{
		"image": "alpine:3.20",
		"build": {"dockerfile": "Dockerfile", "context": "."},
		"hostRequirements": {"cpus": 2, "memory": "4gb", "gpu": {"optional": true}},
		"runArgs": ["--init"]
	}`)
	_, warns, err := parseRaw(src, "")
	if err != nil {
		t.Fatalf("parseRaw: %v", err)
	}
	for _, w := range warns {
		if w.Code == WarnUnknownField {
			t.Errorf("unexpected WarnUnknownField: %+v", w)
		}
	}
}

func TestParseRaw_InvalidJSON(t *testing.T) {
	src := []byte(`{"image": }`)
	_, _, err := parseRaw(src, "/path/to/devcontainer.json")
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
