package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/tidwall/jsonc"
)

// parseRaw decodes a devcontainer.json document. Comments (line and block)
// and trailing commas are tolerated via the JSONC pre-processor.
//
// Unknown fields produce WarnUnknownField warnings rather than parse
// failures: spec consumers add new keys over time, and we want to be
// forward-compatible. Strict-decode is layered on top of permissive
// json.Unmarshal so the caller still gets a populated rawConfig even
// when the file declares fields the library doesn't yet understand.
//
// Strictness is currently applied at the top level and to the
// build / hostRequirements / hostRequirements.gpu objects — the places
// where typo'd nested keys (e.g. `build.dockerFile`) would silently
// no-op without it. Other nested shapes are user-keyed maps or
// position-arrays that don't admit a closed field set.
//
// path is used only for error messages and is otherwise opaque to the
// parser.
func parseRaw(src []byte, path string) (*rawConfig, []Warning, error) {
	cleaned := jsonc.ToJSON(src)
	var raw rawConfig
	if err := json.Unmarshal(cleaned, &raw); err != nil {
		return nil, nil, &ConfigParseError{Path: path, Err: err}
	}

	var warns []Warning
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(cleaned, &topLevel); err == nil {
		warns = append(warns, unknownKeyWarnings(topLevel, knownTopLevelFields, "")...)

		if b, ok := topLevel["build"]; ok && len(b) > 0 {
			var nested map[string]json.RawMessage
			if json.Unmarshal(b, &nested) == nil {
				warns = append(warns, unknownKeyWarnings(nested, knownBuildFields, "/build")...)
			}
		}
		if h, ok := topLevel["hostRequirements"]; ok && len(h) > 0 {
			var nested map[string]json.RawMessage
			if json.Unmarshal(h, &nested) == nil {
				warns = append(warns, unknownKeyWarnings(nested, knownHostRequirementsFields, "/hostRequirements")...)
				if g, ok := nested["gpu"]; ok && len(g) > 0 {
					var gpuMap map[string]json.RawMessage
					if json.Unmarshal(g, &gpuMap) == nil {
						warns = append(warns, unknownKeyWarnings(gpuMap, knownGPUFields, "/hostRequirements/gpu")...)
					}
				}
			}
		}
	}
	return &raw, warns, nil
}

// unknownKeyWarnings emits a WarnUnknownField for each key in obj that
// is not in the known set. Comparison is case-insensitive to mirror
// encoding/json's permissive field matching: a key like "dockerFile"
// hits the "dockerfile" struct field at decode time, so we should not
// claim it was ignored. Output is sorted by key name for stable
// warning ordering across runs.
func unknownKeyWarnings(obj map[string]json.RawMessage, known map[string]struct{}, pathPrefix string) []Warning {
	if len(obj) == 0 {
		return nil
	}
	unknown := make([]string, 0)
	for k := range obj {
		if _, ok := known[strings.ToLower(k)]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	out := make([]Warning, 0, len(unknown))
	for _, k := range unknown {
		out = append(out, Warning{
			Code:    WarnUnknownField,
			Message: fmt.Sprintf("unknown field %q; ignored", k),
			Path:    pathPrefix + "/" + k,
		})
	}
	return out
}

// jsonFieldNames returns the set of json tag names declared on T, used
// to drive unknown-field detection. Computes once per type via
// reflection; suitable for package-level var initialization.
func jsonFieldNames(t reflect.Type) map[string]struct{} {
	out := map[string]struct{}{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			continue
		}
		out[name] = struct{}{}
	}
	return out
}

var (
	knownTopLevelFields         = lowercaseSet(jsonFieldNames(reflect.TypeOf(rawConfig{})))
	knownBuildFields            = lowercaseSet(jsonFieldNames(reflect.TypeOf(rawBuild{})))
	knownHostRequirementsFields = lowercaseSet(jsonFieldNames(reflect.TypeOf(rawHostRequirements{})))
	knownGPUFields              = map[string]struct{}{"optional": {}}
)

func lowercaseSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[strings.ToLower(k)] = struct{}{}
	}
	return out
}
