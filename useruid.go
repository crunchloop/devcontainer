package devcontainer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/crunchloop/devcontainer/config"
	dcruntime "github.com/crunchloop/devcontainer/runtime"
)

// reconcileRemoteUserUID mirrors @devcontainers/cli's updateRemoteUserUID
// behavior: if the host workspace folder is owned by a UID/GID that
// differs from the container user's UID/GID, derive a new image (tagged
// with a "-uid" suffix) whose user has been groupmod/usermod-ed to
// match. Returns the image to use for container creation — either the
// derived image, or finalImage unchanged if no reconciliation applies.
//
// Skips (returns finalImage unchanged, no error) when:
//   - cfg.UpdateRemoteUserUID is explicitly false
//   - host is not Linux/Darwin (Stat_t.Uid not portably available)
//   - LocalWorkspaceFolder cannot be stat-ed
//   - host UID is 0 (root can write anywhere)
//   - effective container user resolves to root / "0" / empty
//
// We always emit the conditional Dockerfile rather than probing the
// existing user's UID first: the build-time check (`id -u $user`) makes
// the layer a no-op when UIDs already match, and avoids spinning a
// throwaway container just to read /etc/passwd. Costs one cached layer
// in the steady state.
func (e *Engine) reconcileRemoteUserUID(ctx context.Context, cfg *config.ResolvedConfig, finalImage string, opts UpOptions) (string, error) {
	if cfg.UpdateRemoteUserUID != nil && !*cfg.UpdateRemoteUserUID {
		return finalImage, nil
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return finalImage, nil
	}

	hostUID, hostGID, ok := statOwner(cfg.LocalWorkspaceFolder)
	if !ok {
		return finalImage, nil
	}
	if hostUID == 0 {
		return finalImage, nil
	}

	user, err := effectiveContainerUser(ctx, e.runtime, cfg, finalImage)
	if err != nil {
		// Only reachable when neither remoteUser nor containerUser is
		// set in cfg, so we have to fall back on the image's USER. If
		// that inspect fails, we can't determine who to reconcile —
		// skip with a warning rather than failing Up. Any genuine
		// "image not found" surfaces at RunContainer with the same ref
		// and a clearer message.
		cfg.Warnings = append(cfg.Warnings, config.Warning{
			Code:    config.WarnUIDReconcileSkipped,
			Message: "updateRemoteUserUID: could not determine effective container user (" + err.Error() + "); skipping reconciliation",
			Source:  finalImage,
		})
		return finalImage, nil
	}
	if user == "" || user == "root" || user == "0" {
		return finalImage, nil
	}

	// Use a deterministic engine-owned tag rather than appending to
	// `finalImage`: the latter breaks on digest-pinned refs
	// (`name@sha256:...-uid` is not a valid docker tag) and on refs that
	// already carry a `:tag` suffix where appending produces a confusing
	// `name:tag-uid` rather than a clean `name:tag` form. Matches the
	// `dc-go-final-` / `dc-go-base-` naming used elsewhere.
	tag := "dc-go-uid-" + cfg.DevcontainerID + ":latest"

	tmp, err := os.MkdirTemp("", "dc-go-uid-*")
	if err != nil {
		return "", fmt.Errorf("create uid build context: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := os.WriteFile(filepath.Join(tmp, "uid-fix.sh"), []byte(uidReconcileScript), 0o755); err != nil {
		return "", err
	}
	df := generateUIDDockerfile(finalImage, user, hostUID, hostGID)
	if err := os.WriteFile(filepath.Join(tmp, "Dockerfile"), []byte(df), 0o644); err != nil {
		return "", err
	}
	if _, err := e.runtime.BuildImage(ctx, dcruntime.BuildSpec{
		ContextPath: tmp,
		Dockerfile:  "Dockerfile",
		Tag:         tag,
	}, opts.Events); err != nil {
		return "", fmt.Errorf("build uid-reconciled image: %w", err)
	}
	return tag, nil
}

// effectiveContainerUser resolves the container user that the workspace
// will run as, using the spec's precedence: remoteUser > containerUser >
// image's default USER. Inspect failures are propagated so callers can
// distinguish "no user configured" (return value "") from "we failed to
// figure out what the user is" (non-nil error).
func effectiveContainerUser(ctx context.Context, rt dcruntime.Runtime, cfg *config.ResolvedConfig, image string) (string, error) {
	if cfg.RemoteUser != "" {
		return cfg.RemoteUser, nil
	}
	if cfg.ContainerUser != "" {
		return cfg.ContainerUser, nil
	}
	details, err := rt.InspectImage(ctx, image)
	if err != nil {
		return "", fmt.Errorf("inspect image %s: %w", image, err)
	}
	if details == nil {
		return "", nil
	}
	return details.User, nil
}

// statOwner returns the UID/GID of path on Unix-like systems. ok=false
// if path doesn't exist or its stat info isn't a *syscall.Stat_t.
func statOwner(path string) (uid, gid int, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	st, stOk := fi.Sys().(*syscall.Stat_t)
	if !stOk {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// generateUIDDockerfile produces a single-stage Dockerfile that runs
// uidReconcileScript against the base image. The script writes
// /etc/passwd and /etc/group directly via awk + sed so it works on
// Debian (shadow tools) and Alpine/BusyBox (no shadow tools) alike —
// no `usermod`/`groupmod`/`getent` runtime dependency.
func generateUIDDockerfile(baseImage, user string, hostUID, hostGID int) string {
	return "# syntax=docker/dockerfile:1.4\n" +
		"ARG _DEV_CONTAINERS_BASE_IMAGE=" + baseImage + "\n" +
		"FROM $_DEV_CONTAINERS_BASE_IMAGE\n" +
		"USER root\n" +
		"ARG _REMOTE_USER=" + user + "\n" +
		"ARG _REMOTE_USER_UID=" + strconv.Itoa(hostUID) + "\n" +
		"ARG _REMOTE_USER_GID=" + strconv.Itoa(hostGID) + "\n" +
		"COPY uid-fix.sh /tmp/uid-fix.sh\n" +
		"RUN chmod +x /tmp/uid-fix.sh && /bin/sh /tmp/uid-fix.sh && rm /tmp/uid-fix.sh\n"
}

// uidReconcileScript rewrites the configured user's UID/GID in
// /etc/passwd and /etc/group to (hostUID, hostGID), then chowns the
// home directory. Uses POSIX awk/sed/chmod so it works on both Debian
// shadow-utils and Alpine BusyBox without conditional branching.
//
// Skips quietly (exit 0) when:
//   - the configured user doesn't exist in /etc/passwd (caller intent
//     unclear; chasing it would risk creating wrong-named users);
//   - UIDs already match (idempotent rebuild stays a near no-op).
//
// Limitations: doesn't touch /etc/shadow or /etc/gshadow. Those files
// key by username, not UID, so password-based login keeps working —
// but `su <user>` semantics that rely on shadow records may be
// affected on hardened images. No real-world devcontainer flow
// depends on this; document and move on. Tracked under #29 if a
// consumer ever hits it.
const uidReconcileScript = `#!/bin/sh
set -eu

USER_LINE=$(awk -F: -v u="$_REMOTE_USER" '$1==u {print; exit}' /etc/passwd)
if [ -z "$USER_LINE" ]; then
    echo "updateRemoteUserUID: user $_REMOTE_USER not found in /etc/passwd; skipping" >&2
    exit 0
fi
CUR_UID=$(echo "$USER_LINE" | awk -F: '{print $3}')
CUR_GID=$(echo "$USER_LINE" | awk -F: '{print $4}')
HOME_DIR=$(echo "$USER_LINE" | awk -F: '{print $6}')

if [ "$CUR_UID" = "$_REMOTE_USER_UID" ] && [ "$CUR_GID" = "$_REMOTE_USER_GID" ]; then
    exit 0
fi

# Re-map the user's primary group to the target GID, but only if no
# other group already owns that GID (avoid duplicate-GID conflicts;
# fall through to just re-pointing the user instead).
if [ "$CUR_GID" != "$_REMOTE_USER_GID" ]; then
    if ! awk -F: -v g="$_REMOTE_USER_GID" '$3==g {found=1} END {exit !found+0}' /etc/group; then
        OLD_GROUP_NAME=$(awk -F: -v g="$CUR_GID" '$3==g {print $1; exit}' /etc/group)
        if [ -n "$OLD_GROUP_NAME" ]; then
            sed -i "s/^\(${OLD_GROUP_NAME}:[^:]*:\)$CUR_GID:/\1$_REMOTE_USER_GID:/" /etc/group
        fi
    fi
fi

# Rewrite the user's row in /etc/passwd, swapping just UID and GID.
sed -i "s/^\(${_REMOTE_USER}:[^:]*:\)$CUR_UID:$CUR_GID:/\1$_REMOTE_USER_UID:$_REMOTE_USER_GID:/" /etc/passwd

if [ -n "$HOME_DIR" ] && [ -d "$HOME_DIR" ]; then
    chown -R "$_REMOTE_USER_UID:$_REMOTE_USER_GID" "$HOME_DIR"
fi
`
