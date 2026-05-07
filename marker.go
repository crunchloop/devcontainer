package devcontainer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

// markerDir is the in-container directory holding lifecycle phase markers.
// Located at the filesystem root so it survives container stops/starts and
// is independent of the user's home directory.
const markerDir = "/var/devcontainer-go/markers"

// markerVersion is the JSON schema version. Bump on incompatible
// changes; the runner treats any other version as "no marker present"
// and re-runs the phase.
const markerVersion = 1

// marker is the on-disk representation of a completed lifecycle phase.
// keyTimestamp matches container.Created for create-keyed phases
// (onCreate / updateContent / postCreate) or container.StartedAt for
// start-keyed phases (postStart). postAttach has no marker.
type marker struct {
	Version      int       `json:"v"`
	Phase        string    `json:"phase"`
	KeyTimestamp time.Time `json:"keyTimestamp"`
	RanAt        time.Time `json:"ranAt"`
	DurationMs   int64     `json:"durationMs"`
	ExitCode     int       `json:"exitCode"`
}

// readMarker reads /var/devcontainer-go/markers/<phase> from the container.
// Returns (nil, nil) if the marker is absent. Errors only on unexpected
// runtime failures, never on "file not found".
func (e *Engine) readMarker(ctx context.Context, ws *Workspace, phase config.LifecyclePhase) (*marker, error) {
	path := markerDir + "/" + string(phase)
	res, err := e.runtime.ExecContainer(ctx, ws.Container.ID, runtime.ExecOptions{
		Cmd: []string{"sh", "-c", "cat " + path + " 2>/dev/null"},
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return nil, nil
	}
	var m marker
	if err := json.Unmarshal([]byte(res.Stdout), &m); err != nil {
		return nil, nil // corrupt or older schema → treat as absent, re-run
	}
	if m.Version != markerVersion {
		return nil, nil // unsupported schema → re-run
	}
	return &m, nil
}

// writeMarker writes /var/devcontainer-go/markers/<phase> in the container.
// The directory is created if absent. Marker writes happen as the
// container's default user; if that user can't write the directory the
// error surfaces here as a non-zero exit code.
func (e *Engine) writeMarker(ctx context.Context, ws *Workspace, m marker) error {
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	// Use a heredoc to avoid quoting the JSON. EOF tag is unique enough
	// the JSON won't accidentally close it.
	script := fmt.Sprintf(
		"mkdir -p %s && cat > %s/%s <<'__DC_GO_EOF__'\n%s\n__DC_GO_EOF__\n",
		markerDir, markerDir, m.Phase, string(body),
	)
	res, err := e.runtime.ExecContainer(ctx, ws.Container.ID, runtime.ExecOptions{
		Cmd: []string{"sh", "-c", script},
	})
	if err != nil {
		return fmt.Errorf("exec write marker: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write marker %s: exit=%d stderr=%q", m.Phase, res.ExitCode, res.Stderr)
	}
	return nil
}

// shouldRun returns true if phase needs to execute given the existing
// marker (or absence thereof) and the current container's timestamps.
func shouldRun(phase config.LifecyclePhase, m *marker, container *runtime.ContainerDetails) bool {
	if phase == config.LifecyclePostAttach {
		return true // never markered; always runs
	}
	if m == nil {
		return true
	}
	key := keyTimestampFor(phase, container)
	return !m.KeyTimestamp.Equal(key)
}

func keyTimestampFor(phase config.LifecyclePhase, container *runtime.ContainerDetails) time.Time {
	if phase == config.LifecyclePostStart {
		return container.StartedAt
	}
	return container.Created
}

// LifecycleError describes a lifecycle phase that ran but exited
// non-zero. Wrapped errors include exec-level failures (daemon
// connectivity etc.) and unwrap to those.
type LifecycleError struct {
	Phase    config.LifecyclePhase
	ExitCode int
	Stdout   string
	Stderr   string
	Cause    error
}

func (e *LifecycleError) Error() string {
	if e.Cause != nil && e.ExitCode == 0 {
		return fmt.Sprintf("lifecycle %s: %v", e.Phase, e.Cause)
	}
	if msg := strings.TrimSpace(e.Stderr); msg != "" {
		return fmt.Sprintf("lifecycle %s: exit=%d: %s", e.Phase, e.ExitCode, truncate(msg, 200))
	}
	return fmt.Sprintf("lifecycle %s: exit=%d", e.Phase, e.ExitCode)
}

func (e *LifecycleError) Unwrap() error { return e.Cause }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// IsLifecycleError reports whether err is a *LifecycleError.
func IsLifecycleError(err error) bool {
	var le *LifecycleError
	return errors.As(err, &le)
}

// renderShellHeredoc emits a single-quoted heredoc-safe representation of s.
// Currently unused (we use single-quoted EOF tags above); reserved for
// scripts that need to inline user-provided content with escapes.
var _ = func(s string) string {
	var b bytes.Buffer
	b.WriteString("'")
	b.WriteString(strings.ReplaceAll(s, "'", `'\''`))
	b.WriteString("'")
	return b.String()
}
