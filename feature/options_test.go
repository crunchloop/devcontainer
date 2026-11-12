package feature

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
)

func TestMergeOptions_DefaultsAndOverrides(t *testing.T) {
	meta := config.FeatureMetadata{
		ID: "node",
		Options: map[string]config.FeatureOption{
			"version":     {Type: "string", Default: "lts"},
			"installNvm":  {Type: "boolean", Default: true},
			"unspecified": {Type: "string", Default: "x"},
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

// The reference CLI writes options into devcontainer-features.env
// through a JavaScript template literal, so a non-string value of a
// string option is converted as JavaScript's String() converts it.

func stringOptionMeta(opt config.FeatureOption) config.FeatureMetadata {
	return config.FeatureMetadata{
		ID:      "apt-packages",
		Options: map[string]config.FeatureOption{"packages": opt},
	}
}

func coerced(t *testing.T, opt config.FeatureOption, val any) (string, []config.Warning) {
	t.Helper()
	merged, warns, err := MergeOptions(stringOptionMeta(opt), map[string]any{"packages": val})
	if err != nil {
		t.Fatalf("MergeOptions(%#v): %v", val, err)
	}
	s, ok := merged["packages"].(string)
	if !ok {
		t.Fatalf("packages = %#v, want a string", merged["packages"])
	}
	return s, warns
}

func TestMergeOptions_ArrayJoinedWithWarning(t *testing.T) {
	got, warns := coerced(t, config.FeatureOption{Type: "string"}, []any{"jq", "tree"})
	if got != "jq,tree" {
		t.Errorf("packages = %q, want %q", got, "jq,tree")
	}
	if len(warns) != 1 || warns[0].Code != config.WarnFeatureOptionCoerced {
		t.Fatalf("warnings = %v, want one %s", warns, config.WarnFeatureOptionCoerced)
	}
	if !strings.Contains(warns[0].Message, `"packages"`) || !strings.Contains(warns[0].Message, `"jq,tree"`) {
		t.Errorf("warning message %q should name the option and the converted value", warns[0].Message)
	}
}

func TestMergeOptions_StringValueNoCoercionWarning(t *testing.T) {
	_, warns := coerced(t, config.FeatureOption{Type: "string"}, "jq tree")
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
}

func TestMergeOptions_EmptyArrayIsEmptyString(t *testing.T) {
	if got, _ := coerced(t, config.FeatureOption{Type: "string"}, []any{}); got != "" {
		t.Errorf("packages = %q, want empty", got)
	}
}

func TestMergeOptions_NestedArrayAndNilElement(t *testing.T) {
	got, _ := coerced(t, config.FeatureOption{Type: "string"}, []any{[]any{"a", "b"}, nil, "c"})
	if got != "a,b,,c" {
		t.Errorf("packages = %q, want %q", got, "a,b,,c")
	}
}

func TestMergeOptions_NumbersFormattedAsJavaScript(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{float64(5), "5"},
		{1.5, "1.5"},
		{1e6, "1000000"},
		{1e21, "1e+21"},
		{-1.5e22, "-1.5e+22"},
		{1e-7, "1e-7"},
		{0.000001, "0.000001"},
		{json.Number("42"), "42"},
		{7, "7"},
	}
	for _, tc := range cases {
		if got, _ := coerced(t, config.FeatureOption{Type: "string"}, tc.in); got != tc.want {
			t.Errorf("%#v → %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMergeOptions_BoolAndNullConverted(t *testing.T) {
	if got, _ := coerced(t, config.FeatureOption{Type: "string"}, true); got != "true" {
		t.Errorf("true → %q", got)
	}
	if got, _ := coerced(t, config.FeatureOption{Type: "string"}, nil); got != "null" {
		t.Errorf("nil → %q", got)
	}
}

func TestMergeOptions_ObjectStillRejected(t *testing.T) {
	for _, val := range []any{
		map[string]any{"a": float64(1)},
		[]any{"a", map[string]any{"b": "c"}},
	} {
		_, _, err := MergeOptions(stringOptionMeta(config.FeatureOption{Type: "string"}), map[string]any{"packages": val})
		if err == nil {
			t.Fatalf("%#v should be rejected", val)
		}
		if !strings.Contains(err.Error(), `"packages"`) || !strings.Contains(err.Error(), "expects string") {
			t.Errorf("error %q should name the option and its type", err)
		}
	}
}

func TestMergeOptions_UntypedOptionConverted(t *testing.T) {
	got, warns := coerced(t, config.FeatureOption{}, []any{"jq", "tree"})
	if got != "jq,tree" {
		t.Errorf("packages = %q, want %q", got, "jq,tree")
	}
	if len(warns) != 1 {
		t.Errorf("warnings = %v, want one", warns)
	}
}

func TestMergeOptions_EnumCheckedAfterConversion(t *testing.T) {
	opt := config.FeatureOption{Type: "string", Enum: []any{"20", "22"}}
	if got, _ := coerced(t, opt, float64(20)); got != "20" {
		t.Errorf("20 → %q, want %q", got, "20")
	}
	_, _, err := MergeOptions(stringOptionMeta(opt), map[string]any{"packages": float64(18)})
	if err == nil || !strings.Contains(err.Error(), "enum") {
		t.Errorf("18 should fail the enum check, got %v", err)
	}
}

func TestMergeOptions_NonStringDefaultConverted(t *testing.T) {
	merged, _, err := MergeOptions(stringOptionMeta(config.FeatureOption{Type: "string", Default: []any{"git"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if merged["packages"] != "git" {
		t.Errorf("packages = %#v, want %q", merged["packages"], "git")
	}
}

func TestMergeOptions_UndeclaredArrayConverted(t *testing.T) {
	merged, _, err := MergeOptions(config.FeatureMetadata{ID: "x"}, map[string]any{"extra": []any{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if merged["extra"] != "a,b" {
		t.Errorf("extra = %#v, want %q", merged["extra"], "a,b")
	}
}

func TestSerializeEnvFile_AfterCoercion(t *testing.T) {
	merged, _, err := MergeOptions(stringOptionMeta(config.FeatureOption{Type: "string"}), map[string]any{
		"packages": []any{"jq", "tree"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(SerializeEnvFile(merged)); got != "PACKAGES='jq,tree'\n" {
		t.Errorf("env file = %q, want %q", got, "PACKAGES='jq,tree'\n")
	}
}
