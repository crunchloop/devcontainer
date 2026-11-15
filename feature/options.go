package feature

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/crunchloop/devcontainer/config"
)

// MergeOptions combines user-supplied options with feature defaults
// and validates against the feature's option declarations.
//
// Order of precedence (later wins): defaults → user. Keys not declared
// in the feature's options are kept (some features accept arbitrary
// extras), converted to strings when they can be, and emit
// WarnUnknownFeatureOption. Enum-typed options are
// hard-validated; values outside the enum produce an error. Proposals
// (soft constraints) do not validate.
//
// A string-typed (or untyped) option given a non-string value is
// converted the way the reference CLI converts it (see
// coerceOptionString) and emits WarnFeatureOptionCoerced; only an
// object is rejected. A boolean-typed option accepts the strings
// "true" and "false" as the booleans (also with
// WarnFeatureOptionCoerced) and rejects any other non-boolean.
//
// Returns the merged map and any non-fatal warnings. Errors are
// validation failures (enum mismatch, type mismatch on declared options).
func MergeOptions(meta config.FeatureMetadata, user map[string]any) (map[string]any, []config.Warning, error) {
	out := make(map[string]any, len(meta.Options)+len(user))
	var warnings []config.Warning

	// Start with defaults from the feature's declared options. A
	// non-string default of a string option is converted too, so the
	// env file never carries Go's formatting of it.
	for name, opt := range meta.Options {
		if opt.Default == nil {
			continue
		}
		out[name] = opt.Default
		if isStringOption(opt) {
			if s, ok := coerceOptionString(opt.Default); ok {
				out[name] = s
			}
		}
	}

	// Overlay user values, validating against declarations.
	for name, val := range user {
		decl, declared := meta.Options[name]
		if !declared {
			// The reference CLI writes undeclared options to the env
			// file too; convert them the same way when it can.
			out[name] = val
			if s, ok := coerceOptionString(val); ok {
				out[name] = s
			}
			warnings = append(warnings, config.Warning{
				Code:    config.WarnUnknownFeatureOption,
				Message: fmt.Sprintf("feature %q: option %q is not declared in devcontainer-feature.json", meta.ID, name),
			})
			continue
		}
		v, converted, err := validateOptionValue(name, decl, val)
		if err != nil {
			return nil, warnings, fmt.Errorf("feature %q: %w", meta.ID, err)
		}
		if converted {
			want := decl.Type
			if want == "" {
				want = "string"
			}
			warnings = append(warnings, config.Warning{
				Code:    config.WarnFeatureOptionCoerced,
				Message: fmt.Sprintf("feature %q: option %q expects a %s; %T value converted to %#v", meta.ID, name, want, val, v),
			})
		}
		out[name] = v
	}

	return out, warnings, nil
}

func isStringOption(opt config.FeatureOption) bool {
	return opt.Type == "string" || opt.Type == ""
}

// validateOptionValue checks val against opt and returns the value to
// install with: val itself, or for a string option its string form,
// or for a boolean option given the string "true" or "false" that
// boolean. converted reports whether the returned value differs in
// type from val.
func validateOptionValue(name string, opt config.FeatureOption, val any) (v any, converted bool, err error) {
	switch {
	case isStringOption(opt):
		s, ok := coerceOptionString(val)
		if !ok {
			return nil, false, fmt.Errorf("option %q expects string, got %T", name, val)
		}
		_, wasString := val.(string)
		val, converted = s, !wasString
	case opt.Type == "boolean":
		// The reference CLI writes "true" and true identically
		// (TRUE="true"), so the strings "true" and "false" mean the
		// boolean. Any other string stays an error.
		if s, ok := val.(string); ok && (s == "true" || s == "false") {
			val, converted = s == "true", true
		}
	}
	if len(opt.Enum) > 0 {
		for _, allowed := range opt.Enum {
			if anyEqual(allowed, val) {
				return val, converted, nil
			}
		}
		return nil, false, fmt.Errorf("option %q value %v is not in enum %v", name, val, opt.Enum)
	}
	if opt.Type == "boolean" {
		if _, ok := val.(bool); !ok {
			return nil, false, fmt.Errorf("option %q expects boolean, got %T", name, val)
		}
	}
	// Any other type: accept and let install.sh deal with it.
	return val, converted, nil
}

// coerceOptionString converts a JSON option value to a string the way
// the reference CLI (@devcontainers/cli) does: it writes each value
// into devcontainer-features.env through a JavaScript template
// literal, i.e. JavaScript's String(). An array joins its elements
// with "," (null → "", nested arrays flatten), null is "null", and
// numbers use JavaScript's number formatting. An object (which
// JavaScript would write as "[object Object]") is not converted: it
// reports false so the option is rejected.
func coerceOptionString(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "null", true
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		return jsNumberString(x), true
	case float32:
		return jsNumberString(float64(x)), true
	case json.Number:
		// JavaScript parses every JSON number as a double, so
		// "1e6" is 1000000 and a 19-digit integer loses precision.
		if f, err := x.Float64(); err == nil {
			return jsNumberString(f), true
		}
		return x.String(), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case uint:
		return strconv.FormatUint(uint64(x), 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case uint32:
		return strconv.FormatUint(uint64(x), 10), true
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				continue // Array.prototype.join writes null as "".
			}
			s, ok := coerceOptionString(e)
			if !ok {
				return "", false
			}
			parts[i] = s
		}
		return strings.Join(parts, ","), true
	default:
		return "", false
	}
}

// jsNumberString formats f as JavaScript's Number.prototype.toString
// does: shortest round-trip digits, positional notation for
// 1e-6 <= |f| < 1e21, and otherwise exponent form with no exponent
// padding ("1e+21", "1.5e-7").
func jsNumberString(f float64) string {
	if f == 0 {
		return "0" // also -0
	}
	if a := math.Abs(f); a >= 1e21 || a < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[:1]
		exp = strings.TrimLeft(exp[1:], "0")
		return mant + "e" + sign + exp
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
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
	nonWordRegexp      = regexp.MustCompile(`[^\w]`)
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
