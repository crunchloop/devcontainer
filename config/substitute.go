package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// variableRegexp matches a single ${...} placeholder. Non-greedy, no nesting
// support — matches devpod / VS Code / @devcontainers/cli behavior.
var variableRegexp = regexp.MustCompile(`\$\{([^}]+)\}`)

// SubstitutionContext holds host-side values used to resolve placeholders in
// devcontainer.json strings. Fields left empty cause the corresponding
// variable to be preserved as a literal placeholder.
type SubstitutionContext struct {
	LocalWorkspaceFolder     string
	ContainerWorkspaceFolder string
	DevcontainerID           string
	LocalEnv                 map[string]string
}

// ResolveString substitutes host-context variables in s.
//
// Supported variables:
//   - ${localWorkspaceFolder}, ${localWorkspaceFolderBasename}
//   - ${containerWorkspaceFolder}, ${containerWorkspaceFolderBasename}
//   - ${devcontainerId}
//   - ${localEnv:VAR} and ${localEnv:VAR:default}
//   - ${env:VAR} (alias for localEnv)
//
// Pass-through (left as literal): ${containerEnv:VAR} — resolved at runtime
// against the live container.
//
// Undefined ${localEnv:VAR} (no default) substitutes to empty string and
// emits WarnUnresolvedLocalEnv, matching shell semantics. Unknown variable
// names are left as literals and emit WarnUnknownVariable.
func ResolveString(s string, ctx SubstitutionContext) (string, []Warning) {
	var warnings []Warning
	out := variableRegexp.ReplaceAllStringFunc(s, func(match string) string {
		inner := match[2 : len(match)-1]
		parts := strings.Split(inner, ":")
		name := parts[0]
		args := parts[1:]
		val, w := lookupVariable(ctx, match, name, args)
		if w != nil {
			warnings = append(warnings, *w)
		}
		return val
	})
	return out, warnings
}

func lookupVariable(ctx SubstitutionContext, match, name string, args []string) (string, *Warning) {
	switch name {
	case "localWorkspaceFolder":
		if ctx.LocalWorkspaceFolder == "" {
			return match, nil
		}
		return ctx.LocalWorkspaceFolder, nil

	case "localWorkspaceFolderBasename":
		if ctx.LocalWorkspaceFolder == "" {
			return match, nil
		}
		return filepath.Base(ctx.LocalWorkspaceFolder), nil

	case "containerWorkspaceFolder":
		if ctx.ContainerWorkspaceFolder == "" {
			return match, nil
		}
		return ctx.ContainerWorkspaceFolder, nil

	case "containerWorkspaceFolderBasename":
		if ctx.ContainerWorkspaceFolder == "" {
			return match, nil
		}
		return filepath.Base(ctx.ContainerWorkspaceFolder), nil

	case "devcontainerId":
		if ctx.DevcontainerID == "" {
			return match, nil
		}
		return ctx.DevcontainerID, nil

	case "localEnv", "env":
		return lookupLocalEnv(ctx.LocalEnv, args)

	case "containerEnv":
		// Pass through; resolved at runtime against the live container.
		return match, nil

	default:
		return match, &Warning{
			Code:    WarnUnknownVariable,
			Message: fmt.Sprintf("unknown substitution variable %q; left as literal", name),
		}
	}
}

func lookupLocalEnv(env map[string]string, args []string) (string, *Warning) {
	if len(args) == 0 || args[0] == "" {
		return "", &Warning{
			Code:    WarnUnresolvedLocalEnv,
			Message: "${localEnv} requires a variable name",
		}
	}
	name := args[0]
	if v, ok := env[name]; ok {
		return v, nil
	}
	if len(args) > 1 {
		// Default value. Spec/devpod use args[1] only; we match for compat.
		// Multi-colon defaults (e.g. URLs) lose information after the
		// first colon — known limitation.
		return args[1], nil
	}
	return "", &Warning{
		Code:    WarnUnresolvedLocalEnv,
		Message: fmt.Sprintf("${localEnv:%s} not set; substituted empty string", name),
	}
}
