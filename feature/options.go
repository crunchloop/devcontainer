package feature

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/crunchloop/devcontainer/config"
)

// MergeOptions combines user-supplied options with feature defaults
// and validates against the feature's option declarations.
//
// Order of precedence (later wins): defaults → user. Keys not declared
// in the feature's options are kept (some features accept arbitrary
// extras) but emit WarnUnknownFeatureOption. Enum-typed options are
// hard-validated; values outside the enum produce an error. Proposals
// (soft constraints) do not validate.
//
// Returns the merged map and any non-fatal warnings. Errors are
// validation failures (enum mismatch, type mismatch on declared options).
func MergeOptions(meta config.FeatureMetadata, user map[string]any) (map[string]any, []config.Warning, error) {
	out := make(map[string]any, len(meta.Options)+len(user))
	var warnings []config.Warning

	// Start with defaults from the feature's declared options.
	for name, opt := range meta.Options {
		if opt.Default != nil {
			out[name] = opt.Default
		}
	}

	// Overlay user values, validating against declarations.
	for name, val := range user {
		decl, declared := meta.Options[name]
		if !declared {
			out[name] = val
			warnings = append(warnings, config.Warning{
				Code:    config.WarnUnknownFeatureOption,
				Message: fmt.Sprintf("feature %q: option %q is not declared in devcontainer-feature.json", meta.ID, name),
			})
			continue
		}
		if err := validateOptionValue(name, decl, val); err != nil {
			return nil, warnings, fmt.Errorf("feature %q: %w", meta.ID, err)
		}
		out[name] = val
	}

	return out, warnings, nil
}

func validateOptionValue(name string, opt config.FeatureOption, val any) error {
	if len(opt.Enum) > 0 {
		for _, allowed := range opt.Enum {
			if anyEqual(allowed, val) {
				return nil
			}
		}
		return fmt.Errorf("option %q value %v is not in enum %v", name, val, opt.Enum)
	}
	switch opt.Type {
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("option %q expects boolean, got %T", name, val)
		}
	case "string", "":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("option %q expects string, got %T", name, val)
		}
	default:
		// Unknown type — accept and let install.sh deal with it.
	}
	return nil
}

func anyEqual(a, b any) bool {
	// Direct equality covers strings, bools, and numeric types after
	// json.Unmarshal (both float64). Compare via fmt.Sprint as a safety
	// net for cross-numeric mismatches (int vs float64).
	if a == b {
		return true
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// SerializeEnvFile emits options as a `KEY="value"` env file the feature's
// install.sh sources. Keys are uppercased and shell-safe; values are
// shell-quoted with single quotes. Sorted by key for determinism.
//
// Layout per spec / devpod convention:
//
//	OPTION_KEY1='value1'
//	OPTION_KEY2='true'
//
// Boolean values render as "true" / "false" without quoting changes.
// Numeric values render as their fmt.Sprint form.
func SerializeEnvFile(opts map[string]any) []byte {
	if len(opts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		envKey := safeEnvKey(k)
		b.WriteString(envKey)
		b.WriteByte('=')
		b.WriteString(shellSingleQuote(fmt.Sprint(opts[k])))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// safeEnvKey transforms a feature option name into a shell-safe
// uppercase environment variable name. Per devcontainer convention:
// non-word chars → "_", leading digit → prefixed with "_", uppercase.
var (
	nonWordRegexp     = regexp.MustCompile(`[^\w]`)
	leadingDigitRegexp = regexp.MustCompile(`^[\d]`)
)

func safeEnvKey(s string) string {
	if s == "" {
		return ""
	}
	s = nonWordRegexp.ReplaceAllString(s, "_")
	if leadingDigitRegexp.MatchString(s) {
		s = "_" + s
	}
	return strings.ToUpper(s)
}

// shellSingleQuote wraps s in single quotes, escaping any embedded
// single quotes. Result is always parseable by `sh` as a single string
// regardless of the input.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
