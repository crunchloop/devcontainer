package docker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/crunchloop/devcontainer/runtime"
)

func (r *Runtime) InspectContainer(ctx context.Context, id string) (*runtime.ContainerDetails, error) {
	res, err := r.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		if isContainerNotFound(err) {
			return nil, &runtime.ContainerNotFoundError{ID: id, Err: err}
		}
		return nil, fmt.Errorf("ContainerInspect: %w", err)
	}
	c := res.Container

	out := &runtime.ContainerDetails{
		Container: runtime.Container{
			ID:    c.ID,
			Name:  trimLeadingSlash(c.Name),
			Image: c.Image,
			State: stateFrom(c.State),
		},
		Created:   parseTime(c.Created),
		StartedAt: stateStartedAt(c.State),
		Mounts:    convertMounts(c.Mounts),
	}
	if c.Config != nil {
		out.User = c.Config.User
		out.Env = append([]string(nil), c.Config.Env...)
		out.Labels = copyLabels(c.Config.Labels)
	}
	return out, nil
}

func (r *Runtime) ContainerLogs(ctx context.Context, id string, w io.Writer, follow bool) error {
	res, err := r.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
	})
	if err != nil {
		if isContainerNotFound(err) {
			return &runtime.ContainerNotFoundError{ID: id, Err: err}
		}
		return fmt.Errorf("ContainerLogs: %w", err)
	}
	// Logs are framed when no TTY; demux into one writer.
	rc, ok := res.(io.ReadCloser)
	if !ok {
		return fmt.Errorf("ContainerLogs: result is not io.ReadCloser")
	}
	if follow {
		// Wrap in cancellable copy so ctx cancellation breaks the read.
		pr, pw := io.Pipe()
		go func() {
			_ = runtime.CancellableCopy(ctx, pw, rc)
			_ = pw.Close()
		}()
		return stdcopy(w, w, pr)
	}
	defer rc.Close()
	return stdcopy(w, w, rc)
}

func trimLeadingSlash(s string) string {
	return strings.TrimPrefix(s, "/")
}

func stateFrom(s *container.State) runtime.State {
	if s == nil {
		return ""
	}
	return runtime.State(s.Status)
}

func stateStartedAt(s *container.State) time.Time {
	if s == nil {
		return time.Time{}
	}
	return parseTime(s.StartedAt)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func convertMounts(in []container.MountPoint) []runtime.MountInspect {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.MountInspect, 0, len(in))
	for _, m := range in {
		out = append(out, runtime.MountInspect{
			Type:     string(m.Type),
			Source:   m.Source,
			Target:   m.Destination,
			ReadOnly: !m.RW,
		})
	}
	return out
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
