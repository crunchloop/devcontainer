package devcontainer

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

// userEnvProbeTimeout caps the time we wait for a login/interactive shell
// to print its environment. A misconfigured rc file (infinite loop,
// blocking prompt) shouldn't hang Up indefinitely; we'd rather fall back
// to no probed env than block.
const userEnvProbeTimeout = 10 * time.Second

// probeUserEnv runs the shell strategy named by probe inside the
// container and returns the parsed environment. Mirrors @devcontainers/cli
// and devpod's behavior:
//
//   - "none" → returns nil (no probing).
//   - others → spawn bash with the appropriate flags, dump
//     /proc/self/environ; fall back to printenv if that fails.
//
// Errors from the probe are non-fatal at the caller (Up); we return
// (nil, err) so the caller can log and proceed without injection.
func (e *Engine) probeUserEnv(ctx context.Context, ws *Workspace, probe config.UserEnvProbe) (map[string]string, error) {
	if probe == config.UserEnvProbeNone {
		return nil, nil
	}

	flags := probeShellFlags(probe)

	probeCtx, cancel := context.WithTimeout(ctx, userEnvProbeTimeout)
	defer cancel()

	// First attempt: read /proc/self/environ (NUL-delimited, captures
	// values with embedded newlines correctly).
	out, err := e.runtime.ExecContainer(probeCtx, ws.Container.ID, runtime.ExecOptions{
		Cmd:  []string{"bash", flags, "cat /proc/self/environ"},
		User: effectiveUser(ws.Config),
	})
	if err == nil && out.ExitCode == 0 && out.Stdout != "" {
		return parseEnvironBytes(out.Stdout, '\x00'), nil
	}

	// Fallback: printenv (newline-delimited). Used when /proc isn't
	// available (some non-Linux containers) or bash isn't installed
	// and we ended up running another shell that lacks /proc/self.
	out, err2 := e.runtime.ExecContainer(probeCtx, ws.Container.ID, runtime.ExecOptions{
		Cmd:  []string{"sh", flags, "printenv"},
		User: effectiveUser(ws.Config),
	})
	if err2 != nil {
		// Surface the original /proc/self/environ error if the
		// fallback also failed — that's the more diagnostic one.
		if err != nil {
			return nil, err
		}
		return nil, err2
	}
	if out.ExitCode != 0 {
		return nil, nil
	}
	return parseEnvironBytes(out.Stdout, '\n'), nil
}

func probeShellFlags(probe config.UserEnvProbe) string {
	switch probe {
	case config.UserEnvProbeLoginShell:
		return "-lc"
	case config.UserEnvProbeInteractiveShell:
		return "-ic"
	case config.UserEnvProbeLoginInteractive:
		return "-lic"
	default:
		return "-lic"
	}
}

func parseEnvironBytes(s string, sep byte) map[string]string {
	out := map[string]string{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == sep {
			line := s[start:i]
			start = i + 1
			if line == "" {
				continue
			}
			eq := strings.IndexByte(line, '=')
			if eq <= 0 {
				continue
			}
			// Don't trim: env values can legitimately contain leading
			// or trailing whitespace, and /proc/self/environ entries
			// have no separator framing to strip.
			out[line[:eq]] = line[eq+1:]
		}
	}
	// PWD reflects the probing shell's cwd, not the caller's intent —
	// matches devpod's behavior of dropping it.
	delete(out, "PWD")
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeProbedEnv composes the effective environment for an exec call.
// Layering (lowest precedence first):
//
//  1. probed (from userEnvProbe)
//  2. cfg.RemoteEnv (devcontainer author intent — overrides probed)
//  3. callerEnv (the explicit ExecOptions.Env passed by the user — wins)
//
// PATH gets a special merge that mirrors devpod: probed PATH is the
// base, then RemoteEnv PATH entries are inserted in their declared
// order so author-added prefix entries land in front. Caller's PATH,
// if any, still wins outright (consistent with the rest of the
// precedence rule — caller is most explicit).
//
// /sbin paths from RemoteEnv are stripped for non-root users, again
// matching devpod and the @devcontainers/cli reference, since /sbin
// binaries usually require root.
func mergeProbedEnv(probed, remoteEnv, callerEnv map[string]string, remoteUser string) map[string]string {
	if len(probed) == 0 && len(remoteEnv) == 0 {
		return callerEnv
	}
	out := make(map[string]string, len(probed)+len(remoteEnv)+len(callerEnv))
	for k, v := range probed {
		out[k] = v
	}
	for k, v := range remoteEnv {
		out[k] = v
	}

	// PATH-aware merge between probed and remoteEnv (caller PATH still
	// wins below).
	probedPath, hasProbedPath := probed["PATH"]
	remotePath, hasRemotePath := remoteEnv["PATH"]
	if hasProbedPath && hasRemotePath {
		out["PATH"] = mergePath(probedPath, remotePath, remoteUser)
	}

	for k, v := range callerEnv {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var sbinSegment = regexp.MustCompile(`/sbin(/|$)`)

func mergePath(probed, remote, remoteUser string) string {
	probedTokens := strings.Split(probed, ":")
	insertAt := 0
	for _, e := range strings.Split(remote, ":") {
		i := slices.Index(probedTokens, e)
		if i == -1 {
			if remoteUser == "root" || !sbinSegment.MatchString(e) {
				probedTokens = slices.Insert(probedTokens, insertAt, e)
				insertAt++
			}
		} else {
			insertAt = i + 1
		}
	}
	return strings.Join(probedTokens, ":")
}
