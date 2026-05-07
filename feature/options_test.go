package feature

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
)

func TestMergeOptions_DefaultsAndOverrides(t *testing.T) {
	meta := config.FeatureMetadata{
		ID: "node",
		Options: map[string]config.FeatureOption{
			"version":      {Type: "string", Default: "lts"},
			"installNvm":   {Type: "boolean", Default: true},
			"unspecified":  {Type: "string", Default: "x"},
		},
	}
	user := map[string]any{
		"version":    "20",
		"installNvm": false,
	}
	merged, warns, err := MergeOptions(meta, user)
	if err != nil {
		t.Fatalf("MergeOptions: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	want := map[string]any{
		"version":     "20",
		"installNvm":  false,
		"unspecified": "x",
	}
	if !reflect.DeepEqual(merged, want) {
		t.Errorf("merged = %+v, want %+v", merged, want)
	}
}

func TestMergeOptions_EnumValidation(t *testing.T) {
	meta := config.FeatureMetadata{
		ID: "shell",
		Options: map[string]config.FeatureOption{
			"name": {Type: "string", Enum: []any{"bash", "zsh", "fish"}},
		},
	}
	if _, _, err := MergeOptions(meta, map[string]any{"name": "zsh"}); err != nil {
		t.Errorf("zsh should be allowed: %v", err)
	}
	if _, _, err := MergeOptions(meta, map[string]any{"name": "tcsh"}); err == nil {
		t.Error("tcsh should be rejected by enum")
	}
}

func TestMergeOptions_TypeValidation(t *testing.T) {
	meta := config.FeatureMetadata{
		ID: "x",
		Options: map[string]config.FeatureOption{
			"flag": {Type: "boolean"},
		},
	}
	if _, _, err := MergeOptions(meta, map[string]any{"flag": "yes"}); err == nil {
		t.Error("string for boolean option should error")
	}
	if _, _, err := MergeOptions(meta, map[string]any{"flag": true}); err != nil {
		t.Errorf("bool for boolean option should pass: %v", err)
	}
}

func TestMergeOptions_UndeclaredKeptWithWarning(t *testing.T) {
	meta := config.FeatureMetadata{
		ID:      "x",
		Options: map[string]config.FeatureOption{"version": {Type: "string"}},
	}
	merged, warns, err := MergeOptions(meta, map[string]any{
		"version": "1.0",
		"extras":  "yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if merged["extras"] != "yes" {
		t.Errorf("undeclared option should be kept, got %+v", merged)
	}
	found := false
	for _, w := range warns {
		if w.Code == config.WarnUnknownFeatureOption {
			found = true
		}
	}
	if !found {
		t.Errorf("expected WarnUnknownFeatureOption, got %v", warns)
	}
}

func TestSerializeEnvFile(t *testing.T) {
	got := string(SerializeEnvFile(map[string]any{
		"version":    "1.2.3",
		"installNvm": true,
		"weird-key":  "with spaces",
	}))
	// Sorted: INSTALLNVM, VERSION, WEIRD_KEY
	want := "INSTALLNVM='true'\nVERSION='1.2.3'\nWEIRD_KEY='with spaces'\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSerializeEnvFile_EmbeddedQuotes(t *testing.T) {
	got := string(SerializeEnvFile(map[string]any{
		"msg": `it's "quoted"`,
	}))
	if !strings.Contains(got, `'\''`) {
		t.Errorf("embedded single quote not escaped: %q", got)
	}
}

func TestSafeEnvKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"version", "VERSION"},
		{"install-nvm", "INSTALL_NVM"},
		{"path.to.thing", "PATH_TO_THING"},
		{"3node", "_3NODE"},
		{"_underscore", "_UNDERSCORE"},
	}
	for _, tc := range cases {
		if got := safeEnvKey(tc.in); got != tc.want {
			t.Errorf("safeEnvKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
