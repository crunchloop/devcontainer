package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// decodeStringOrStringArray accepts a JSON value that's either a string or a
// []string and returns a flat []string. Empty input returns nil.
func decodeStringOrStringArray(data json.RawMessage) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		return arr, nil
	}
	return nil, fmt.Errorf("expected string or []string")
}

// decodeLifecycleCommand decodes a spec lifecycle-command field, which may
// be a single string (shell), an array (exec), or an object mapping names to
// either string or array (parallel-named).
func decodeLifecycleCommand(data json.RawMessage) (LifecycleCommand, error) {
	if len(data) == 0 {
		return LifecycleCommand{}, nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return LifecycleCommand{Single: &Command{Shell: s}}, nil
	}

	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		return LifecycleCommand{Single: &Command{Exec: arr}}, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err == nil {
		parallel := make(map[string]Command, len(m))
		for name, raw := range m {
			cmd, err := decodeCommandValue(raw)
			if err != nil {
				return LifecycleCommand{}, fmt.Errorf("lifecycle command %q: %w", name, err)
			}
			parallel[name] = cmd
		}
		return LifecycleCommand{Parallel: parallel}, nil
	}

	return LifecycleCommand{}, fmt.Errorf("lifecycle command must be string, []string, or object")
}

func decodeCommandValue(data json.RawMessage) (Command, error) {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return Command{Shell: s}, nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		return Command{Exec: arr}, nil
	}
	return Command{}, fmt.Errorf("must be string or []string")
}

// decodeMounts decodes the mounts field which can be a heterogeneous array
// of CSV strings ("type=bind,source=...,target=...") and/or objects.
func decodeMounts(data json.RawMessage) ([]Mount, []Warning, error) {
	if len(data) == 0 {
		return nil, nil, nil
	}
	var rawArr []json.RawMessage
	if err := json.Unmarshal(data, &rawArr); err != nil {
		return nil, nil, fmt.Errorf("mounts must be an array")
	}
	var (
		mounts   []Mount
		warnings []Warning
	)
	for i, item := range rawArr {
		var s string
		if err := json.Unmarshal(item, &s); err == nil {
			mounts = append(mounts, parseMountString(s))
			continue
		}
		var obj rawMountObject
		if err := json.Unmarshal(item, &obj); err == nil && (obj.Type != "" || obj.Source != "" || obj.Target != "") {
			mounts = append(mounts, Mount{
				Type:     MountType(obj.Type),
				Source:   obj.Source,
				Target:   obj.Target,
				ReadOnly: obj.ReadOnly,
			})
			continue
		}
		warnings = append(warnings, Warning{
			Code:    WarnUnknownField,
			Message: fmt.Sprintf("mounts[%d]: not a recognized string or object; skipped", i),
			Path:    fmt.Sprintf("/mounts/%d", i),
		})
	}
	return mounts, warnings, nil
}

type rawMountObject struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readonly"`
}

// parseMountString parses a docker-style CSV mount specification. Supports
// the spec aliases (src/source, dst/destination/target). Unknown components
// are silently dropped (caller can layer a Warning if it cares).
func parseMountString(s string) Mount {
	var m Mount
	for _, p := range strings.Split(s, ",") {
		key, val, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		switch key {
		case "type":
			m.Type = MountType(val)
		case "source", "src":
			m.Source = val
		case "target", "dst", "destination":
			m.Target = val
		case "readonly", "ro":
			m.ReadOnly = val == "true" || val == "1"
		}
	}
	return m
}

// decodeWorkspaceMount accepts the spec form (CSV string or mount object)
// and returns a *Mount, or nil if absent.
func decodeWorkspaceMount(data json.RawMessage) (*Mount, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		m := parseMountString(s)
		return &m, nil
	}
	var obj rawMountObject
	if err := json.Unmarshal(data, &obj); err == nil {
		return &Mount{
			Type:     MountType(obj.Type),
			Source:   obj.Source,
			Target:   obj.Target,
			ReadOnly: obj.ReadOnly,
		}, nil
	}
	return nil, fmt.Errorf("workspaceMount must be a string or object")
}

// decodeForwardPorts decodes the forwardPorts field, a heterogeneous array
// of int (container port) or string ("host:container" or just "container").
func decodeForwardPorts(data json.RawMessage) ([]PortSpec, []Warning, error) {
	if len(data) == 0 {
		return nil, nil, nil
	}
	var rawArr []json.RawMessage
	if err := json.Unmarshal(data, &rawArr); err != nil {
		return nil, nil, fmt.Errorf("forwardPorts must be an array")
	}
	var (
		out      []PortSpec
		warnings []Warning
	)
	for i, item := range rawArr {
		var n int
		if err := json.Unmarshal(item, &n); err == nil {
			out = append(out, PortSpec{Container: n})
			continue
		}
		var s string
		if err := json.Unmarshal(item, &s); err == nil {
			ps, err := parsePortString(s)
			if err != nil {
				warnings = append(warnings, Warning{
					Code:    WarnUnknownField,
					Message: fmt.Sprintf("forwardPorts[%d]: %v; skipped", i, err),
					Path:    fmt.Sprintf("/forwardPorts/%d", i),
				})
				continue
			}
			out = append(out, ps)
			continue
		}
		warnings = append(warnings, Warning{
			Code:    WarnUnknownField,
			Message: fmt.Sprintf("forwardPorts[%d]: not a number or string; skipped", i),
			Path:    fmt.Sprintf("/forwardPorts/%d", i),
		})
	}
	return out, warnings, nil
}

func parsePortString(s string) (PortSpec, error) {
	host, container, hasColon := strings.Cut(s, ":")
	if hasColon {
		h, err1 := strconv.Atoi(host)
		c, err2 := strconv.Atoi(container)
		if err1 != nil || err2 != nil {
			return PortSpec{}, fmt.Errorf("invalid port spec %q", s)
		}
		return PortSpec{Host: h, Container: c}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return PortSpec{}, fmt.Errorf("invalid port %q", s)
	}
	return PortSpec{Container: n}, nil
}
