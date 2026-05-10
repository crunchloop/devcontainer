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

	user := effectiveContainerUser(ctx, e.runtime, cfg, finalImage)
	if user == "" || user == "root" || user == "0" {
		return finalImage, nil
	}

	tag := finalImage + "-uid"
	// If `finalImage` is itself a tagged ref like "name:tag", append the
	// suffix to the tag rather than producing "name:tag-uid" — both are
	// valid, but the latter still parses, and matches upstream's tagging.

	tmp, err := os.MkdirTemp("", "dc-go-uid-*")
	if err != nil {
		return "", fmt.Errorf("create uid build context: %w", err)
	}
	defer os.RemoveAll(tmp)

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
// image's default USER.
func effectiveContainerUser(ctx context.Context, rt dcruntime.Runtime, cfg *config.ResolvedConfig, image string) string {
	if cfg.RemoteUser != "" {
		return cfg.RemoteUser
	}
	if cfg.ContainerUser != "" {
		return cfg.ContainerUser
	}
	details, err := rt.InspectImage(ctx, image)
	if err != nil || details == nil {
		return ""
	}
	return details.User
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

// generateUIDDockerfile produces a single-stage Dockerfile that
// reconciles `user`'s UID/GID to (hostUID, hostGID) when they differ
// in the base image. The conditional keeps the resulting layer a near
// no-op when UIDs already match (idempotent rebuilds).
func generateUIDDockerfile(baseImage, user string, hostUID, hostGID int) string {
	return "# syntax=docker/dockerfile:1.4\n" +
		"ARG _DEV_CONTAINERS_BASE_IMAGE=" + baseImage + "\n" +
		"FROM $_DEV_CONTAINERS_BASE_IMAGE\n" +
		"USER root\n" +
		"ARG _REMOTE_USER=" + user + "\n" +
		"ARG _REMOTE_USER_UID=" + strconv.Itoa(hostUID) + "\n" +
		"ARG _REMOTE_USER_GID=" + strconv.Itoa(hostGID) + "\n" +
		"RUN set -e; \\\n" +
		"    if ! id -u \"$_REMOTE_USER\" >/dev/null 2>&1; then \\\n" +
		"        echo \"updateRemoteUserUID: user $_REMOTE_USER not found in image; skipping\" >&2; \\\n" +
		"        exit 0; \\\n" +
		"    fi; \\\n" +
		"    CUR_UID=$(id -u \"$_REMOTE_USER\"); \\\n" +
		"    CUR_GID=$(id -g \"$_REMOTE_USER\"); \\\n" +
		"    if [ \"$CUR_UID\" = \"$_REMOTE_USER_UID\" ] && [ \"$CUR_GID\" = \"$_REMOTE_USER_GID\" ]; then \\\n" +
		"        exit 0; \\\n" +
		"    fi; \\\n" +
		"    OLD_GROUP=$(id -gn \"$_REMOTE_USER\"); \\\n" +
		"    HOME_DIR=$(getent passwd \"$_REMOTE_USER\" | cut -d: -f6); \\\n" +
		"    if [ \"$CUR_GID\" != \"$_REMOTE_USER_GID\" ]; then \\\n" +
		"        if getent group \"$_REMOTE_USER_GID\" >/dev/null; then \\\n" +
		"            usermod --gid \"$_REMOTE_USER_GID\" \"$_REMOTE_USER\"; \\\n" +
		"        else \\\n" +
		"            groupmod --gid \"$_REMOTE_USER_GID\" \"$OLD_GROUP\"; \\\n" +
		"        fi; \\\n" +
		"    fi; \\\n" +
		"    usermod --uid \"$_REMOTE_USER_UID\" \"$_REMOTE_USER\"; \\\n" +
		"    if [ -n \"$HOME_DIR\" ] && [ -d \"$HOME_DIR\" ]; then \\\n" +
		"        chown -R \"$_REMOTE_USER_UID:$_REMOTE_USER_GID\" \"$HOME_DIR\"; \\\n" +
		"    fi\n"
}
