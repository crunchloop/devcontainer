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

// SubstitutionContext holds the values used to resolve placeholders in
// devcontainer.json strings. Fields left empty cause the corresponding
// variable to be preserved as a literal placeholder.
//
// ContainerEnv is populated only after a container has been created and
// inspected; nil signals "host pass" and leaves ${containerEnv:*}
// references literal so the runtime layer can resolve them later.
type SubstitutionContext struct {
	LocalWorkspaceFolder     string
	ContainerWorkspaceFolder string
	DevcontainerID           string
	LocalEnv                 map[string]string
	ContainerEnv             map[string]string
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
		if ctx.ContainerEnv == nil {
			// Host pass: leave literal for the runtime layer to resolve
			// once the container is up.
			return match, nil
		}
		return lookupContainerEnv(ctx.ContainerEnv, args)

	default:
		return match, &Warning{
			Code:    WarnUnknownVariable,
			Message: fmt.Sprintf("unknown substitution variable %q; left as literal", name),
		}
	}
}

func lookupLocalEnv(env map[string]string, args []string) (string, *Warning) {
	return lookupEnvWithDefault(env, args, "localEnv", WarnUnresolvedLocalEnv)
}

func lookupContainerEnv(env map[string]string, args []string) (string, *Warning) {
	return lookupEnvWithDefault(env, args, "containerEnv", WarnUnresolvedContainerEnv)
}

func lookupEnvWithDefault(env map[string]string, args []string, kind string, missingCode WarningCode) (string, *Warning) {
	if len(args) == 0 || args[0] == "" {
		return "", &Warning{
			Code:    missingCode,
			Message: fmt.Sprintf("${%s} requires a variable name", kind),
		}
	}
	name := args[0]
	if v, ok := env[name]; ok {
		return v, nil
	}
	if len(args) > 1 {
		// Default value. Spec/devpod take args[1] only; multi-colon
		// defaults (e.g. URLs) lose information after the first colon —
		// known limitation matching upstream behavior.
		return args[1], nil
	}
	return "", &Warning{
		Code:    missingCode,
		Message: fmt.Sprintf("${%s:%s} not set; substituted empty string", kind, name),
	}
}
