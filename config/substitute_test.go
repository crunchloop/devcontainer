package config

import (
	"reflect"
	"testing"
)

func TestResolveString(t *testing.T) {
	ctx := SubstitutionContext{
		LocalWorkspaceFolder:     "/home/u/proj",
		ContainerWorkspaceFolder: "/workspaces/proj",
		DevcontainerID:           "abcdef0123456789",
		LocalEnv: map[string]string{
			"USER": "alice",
			"HOME": "/home/alice",
		},
	}

	cases := []struct {
		name      string
		in        string
		want      string
		wantCodes []WarningCode
	}{
		{
			name: "no variables",
			in:   "plain string",
			want: "plain string",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "localWorkspaceFolder",
			in:   "${localWorkspaceFolder}",
			want: "/home/u/proj",
		},
		{
			name: "localWorkspaceFolderBasename",
			in:   "${localWorkspaceFolderBasename}",
			want: "proj",
		},
		{
			name: "containerWorkspaceFolder",
			in:   "${containerWorkspaceFolder}",
			want: "/workspaces/proj",
		},
		{
			name: "containerWorkspaceFolderBasename",
			in:   "${containerWorkspaceFolderBasename}",
			want: "proj",
		},
		{
			name: "devcontainerId",
			in:   "${devcontainerId}",
			want: "abcdef0123456789",
		},
		{
			name: "localEnv present",
			in:   "${localEnv:USER}",
			want: "alice",
		},
		{
			name: "env alias for localEnv",
			in:   "${env:USER}",
			want: "alice",
		},
		{
			name:      "localEnv missing, no default => empty + warn",
			in:        "${localEnv:NOPE}",
			want:      "",
			wantCodes: []WarningCode{WarnUnresolvedLocalEnv},
		},
		{
			name: "localEnv missing, with default => default",
			in:   "${localEnv:NOPE:fallback}",
			want: "fallback",
		},
		{
			name: "localEnv present, default ignored",
			in:   "${localEnv:USER:ignored}",
			want: "alice",
		},
		{
			name:      "localEnv with no name => warn, empty",
			in:        "${localEnv:}",
			want:      "",
			wantCodes: []WarningCode{WarnUnresolvedLocalEnv},
		},
		{
			name: "containerEnv passes through",
			in:   "${containerEnv:PATH}",
			want: "${containerEnv:PATH}",
		},
		{
			name: "containerEnv with default passes through",
			in:   "${containerEnv:PATH:/usr/bin}",
			want: "${containerEnv:PATH:/usr/bin}",
		},
		{
			name:      "unknown variable left literal + warn",
			in:        "${notARealVar}",
			want:      "${notARealVar}",
			wantCodes: []WarningCode{WarnUnknownVariable},
		},
		{
			name: "multiple variables in one string",
			in:   "user=${localEnv:USER} home=${localEnv:HOME}",
			want: "user=alice home=/home/alice",
		},
		{
			name: "variable adjacent to text",
			in:   "prefix-${localEnv:USER}-suffix",
			want: "prefix-alice-suffix",
		},
		{
			name: "mixed resolved and pass-through",
			in:   "${localWorkspaceFolder}:${containerEnv:PATH}",
			want: "/home/u/proj:${containerEnv:PATH}",
		},
		{
			name: "no match for unbalanced placeholder",
			in:   "${unterminated",
			want: "${unterminated",
		},
		{
			name: "empty placeholder is not matched",
			in:   "${}",
			want: "${}",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := ResolveString(tc.in, ctx)
			if got != tc.want {
				t.Errorf("ResolveString(%q) = %q, want %q", tc.in, got, tc.want)
			}
			var gotCodes []WarningCode
			for _, w := range warns {
				gotCodes = append(gotCodes, w.Code)
			}
			if !reflect.DeepEqual(gotCodes, tc.wantCodes) {
				t.Errorf("ResolveString(%q) warning codes = %v, want %v", tc.in, gotCodes, tc.wantCodes)
			}
		})
	}
}

func TestResolveString_EmptyContextLeavesLiterals(t *testing.T) {
	// With an empty context, host vars should be left as literal placeholders
	// (no warning — caller may be intentionally producing a partially
	// substituted template).
	ctx := SubstitutionContext{}
	cases := []string{
		"${localWorkspaceFolder}",
		"${localWorkspaceFolderBasename}",
		"${containerWorkspaceFolder}",
		"${containerWorkspaceFolderBasename}",
		"${devcontainerId}",
	}
	for _, in := range cases {
		got, warns := ResolveString(in, ctx)
		if got != in {
			t.Errorf("ResolveString(%q, empty ctx) = %q, want literal pass-through", in, got)
		}
		if len(warns) != 0 {
			t.Errorf("ResolveString(%q, empty ctx) emitted warnings: %v", in, warns)
		}
	}
}

func TestResolveString_ContainerEnvPass(t *testing.T) {
	// When ContainerEnv is populated (container-pass), ${containerEnv:*}
	// resolves with the same default-and-warning semantics as localEnv.
	ctx := SubstitutionContext{
		ContainerEnv: map[string]string{
			"HOME": "/home/vscode",
			"PATH": "/usr/bin",
		},
	}

	cases := []struct {
		name      string
		in        string
		want      string
		wantCodes []WarningCode
	}{
		{"present", "${containerEnv:HOME}", "/home/vscode", nil},
		{"missing with default", "${containerEnv:NOPE:fallback}", "fallback", nil},
		{
			name:      "missing no default => empty + warn",
			in:        "${containerEnv:NOPE}",
			want:      "",
			wantCodes: []WarningCode{WarnUnresolvedContainerEnv},
		},
		{"localEnv unaffected when ContainerEnv is set", "${localEnv:HOME:fallback}", "fallback", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := ResolveString(tc.in, ctx)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			var codes []WarningCode
			for _, w := range warns {
				codes = append(codes, w.Code)
			}
			if !reflect.DeepEqual(codes, tc.wantCodes) {
				t.Errorf("codes = %v, want %v", codes, tc.wantCodes)
			}
		})
	}
}

func TestResolveString_HostPassPreservesContainerEnv(t *testing.T) {
	// When ContainerEnv is nil (host pass), ${containerEnv:*} survives
	// unchanged — even with a default, even with no var name.
	ctx := SubstitutionContext{LocalWorkspaceFolder: "/x"}
	cases := []string{
		"${containerEnv:HOME}",
		"${containerEnv:HOME:fallback}",
	}
	for _, in := range cases {
		got, warns := ResolveString(in, ctx)
		if got != in {
			t.Errorf("ResolveString(%q) = %q, want literal pass-through", in, got)
		}
		if len(warns) != 0 {
			t.Errorf("unexpected warnings: %v", warns)
		}
	}
}

func TestResolveString_WarningMessages(t *testing.T) {
	// Confirm warning messages reference the offending variable so they're
	// useful in caller-facing diagnostics.
	ctx := SubstitutionContext{LocalEnv: map[string]string{}}

	_, warns := ResolveString("${localEnv:GONE}", ctx)
	if len(warns) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warns))
	}
	if warns[0].Code != WarnUnresolvedLocalEnv {
		t.Errorf("got code %q, want %q", warns[0].Code, WarnUnresolvedLocalEnv)
	}
	if !contains(warns[0].Message, "GONE") {
		t.Errorf("warning message %q should mention variable name", warns[0].Message)
	}

	_, warns = ResolveString("${weirdVar}", ctx)
	if len(warns) != 1 || warns[0].Code != WarnUnknownVariable {
		t.Fatalf("expected one WarnUnknownVariable, got %v", warns)
	}
	if !contains(warns[0].Message, "weirdVar") {
		t.Errorf("warning message %q should mention variable name", warns[0].Message)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
