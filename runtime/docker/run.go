package docker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/crunchloop/devcontainer/runtime"
)

// keepAliveCmd is what we set on a container created with
// OverrideCommand=true so it stays running for exec-based interaction.
//
// Uses `tail -f /dev/null` (no shell, no loop) — tail blocks reading
// /dev/null forever and survives stop+start cycles cleanly. The
// previous `sh -c "while sleep 1000; do :; done"` form occasionally
// failed to come back up after a stop on slower runners, with the
// container reporting "exited" state shortly after the daemon
// reported it as running.
var keepAliveCmd = []string{"tail", "-f", "/dev/null"}

func (r *Runtime) RunContainer(ctx context.Context, spec runtime.RunSpec) (*runtime.Container, error) {
	cfg := &container.Config{
		Image:      spec.Image,
		User:       spec.User,
		WorkingDir: spec.WorkingDir,
		Env:        envMapToList(spec.Env),
		Labels:     spec.Labels,
		Entrypoint: spec.Entrypoint,
		Cmd:        spec.Cmd,
	}
	if spec.OverrideCommand {
		cfg.Entrypoint = nil
		cfg.Cmd = keepAliveCmd
	}

	hostCfg := &container.HostConfig{
		Mounts:      toMobyMounts(spec.Mounts),
		Privileged:  spec.Privileged,
		CapAdd:      spec.CapAdd,
		SecurityOpt: spec.SecurityOpt,
	}
	if spec.Init {
		t := true
		hostCfg.Init = &t
	}

	res, err := r.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       spec.Name,
		Config:     cfg,
		HostConfig: hostCfg,
	})
	if err != nil {
		if isImageNotFound(err) {
			return nil, &runtime.ImageNotFoundError{Ref: spec.Image, Err: err}
		}
		return nil, fmt.Errorf("ContainerCreate: %w", err)
	}
	return &runtime.Container{
		ID:    res.ID,
		Name:  spec.Name,
		Image: spec.Image,
		State: runtime.StateCreated,
	}, nil
}

func (r *Runtime) StartContainer(ctx context.Context, id string) error {
	if _, err := r.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		if isContainerNotFound(err) {
			return &runtime.ContainerNotFoundError{ID: id, Err: err}
		}
		return fmt.Errorf("ContainerStart: %w", err)
	}
	// ContainerStart returns once the daemon has accepted the start
	// request, not when the container's process is up. On slow runners
	// the next Inspect / Exec can race and see stale "exited" state or
	// a non-running container. Block until we observe StateRunning.
	return r.waitForRunning(ctx, id, startTimeout)
}

const startTimeout = 30 * time.Second

// waitForRunning polls Inspect until the container's state is running
// AND remains running for a brief settling period. Returns an error if
// the state transitions to exited / dead within the window.
//
// The settling check guards against a race observed on slower CI
// runners: the daemon reports state="running" briefly while the
// process is starting, then transitions to "exited" within a few
// hundred ms when the cmd dies for a non-obvious reason. A single
// "is it running?" read isn't sufficient — we also need to confirm
// it stays running.
func (r *Runtime) waitForRunning(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 50 * time.Millisecond
	const settleDelay = 200 * time.Millisecond
	for {
		details, err := r.InspectContainer(ctx, id)
		if err != nil {
			return err
		}
		switch details.State {
		case runtime.StateRunning:
			// Verify stability: re-check after a short settling delay.
			// If the container exited in that window, surface it now
			// rather than letting downstream Exec calls fail mysteriously.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(settleDelay):
			}
			details2, err := r.InspectContainer(ctx, id)
			if err != nil {
				return err
			}
			if details2.State == runtime.StateRunning {
				return nil
			}
			return fmt.Errorf(
				"container %s reached running but exited within %v (state=%q, started_at=%s)",
				id, settleDelay, details2.State, details2.StartedAt.Format(time.RFC3339Nano),
			)
		case runtime.StateExited, runtime.StateDead:
			return fmt.Errorf("container %s in state %q after start", id, details.State)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s did not reach running state within %v (last state=%q)",
				id, timeout, details.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < time.Second {
			backoff *= 2
		}
	}
}

func (r *Runtime) StopContainer(ctx context.Context, id string, opts runtime.StopOptions) error {
	stopOpts := client.ContainerStopOptions{}
	if opts.Timeout > 0 {
		secs := int(opts.Timeout / time.Second)
		stopOpts.Timeout = &secs
	}
	if _, err := r.api.ContainerStop(ctx, id, stopOpts); err != nil {
		if isContainerNotFound(err) {
			return &runtime.ContainerNotFoundError{ID: id, Err: err}
		}
		return fmt.Errorf("ContainerStop: %w", err)
	}
	// ContainerStop returns once the daemon's stop request is accepted,
	// not after the container has fully transitioned to a stopped state.
	// On slower runners a subsequent Start can race with the still-in-
	// flight stop and silently fail to actually run the cmd: the daemon
	// returns success but the container stays in `exited` with the old
	// finishedAt. Wait for the daemon to settle into a stable not-running
	// state before returning.
	return r.waitForStopped(ctx, id, stopTimeout)
}

const stopTimeout = 30 * time.Second

func (r *Runtime) waitForStopped(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 50 * time.Millisecond
	for {
		details, err := r.InspectContainer(ctx, id)
		if err != nil {
			// Container already gone — fine.
			var nf *runtime.ContainerNotFoundError
			if errors.As(err, &nf) {
				return nil
			}
			return err
		}
		switch details.State {
		case runtime.StateExited, runtime.StateDead, "":
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s did not reach stopped state within %v (last state=%q)",
				id, timeout, details.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < time.Second {
			backoff *= 2
		}
	}
}

func (r *Runtime) RemoveContainer(ctx context.Context, id string, opts runtime.RemoveOptions) error {
	if _, err := r.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{
		Force:         opts.Force,
		RemoveVolumes: opts.RemoveVolumes,
	}); err != nil {
		if isContainerNotFound(err) {
			return &runtime.ContainerNotFoundError{ID: id, Err: err}
		}
		return fmt.Errorf("ContainerRemove: %w", err)
	}
	return nil
}

func (r *Runtime) FindContainerByLabel(ctx context.Context, key, value string) (*runtime.Container, error) {
	filters := client.Filters{}.Add("label", key+"="+value)
	res, err := r.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: filters,
	})
	if err != nil {
		return nil, fmt.Errorf("ContainerList: %w", err)
	}
	if len(res.Items) == 0 {
		return nil, nil
	}
	// Most recently created first.
	sort.Slice(res.Items, func(i, j int) bool {
		return res.Items[i].Created > res.Items[j].Created
	})
	c := res.Items[0]
	name := ""
	if len(c.Names) > 0 {
		name = c.Names[0]
		if len(name) > 0 && name[0] == '/' {
			name = name[1:]
		}
	}
	return &runtime.Container{
		ID:    c.ID,
		Name:  name,
		Image: c.Image,
		State: runtime.State(c.State),
	}, nil
}

func envMapToList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func toMobyMounts(in []runtime.MountSpec) []mount.Mount {
	if len(in) == 0 {
		return nil
	}
	out := make([]mount.Mount, 0, len(in))
	for _, m := range in {
		mm := mount.Mount{
			Type:     mount.Type(m.Type),
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		}
		if m.Propagation != "" {
			mm.Consistency = mount.Consistency(m.Propagation)
		}
		out = append(out, mm)
	}
	return out
}

// Daemon errors don't expose typed error values for "not found"; we
// pattern-match on the message. Brittle but matches how the docker CLI
// itself does it.
func isContainerNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errContainerNotFoundSentinel) ||
		containsAny(err.Error(), "No such container", "no such container")
}

func isImageNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errImageNotFoundSentinel) ||
		containsAny(err.Error(), "No such image", "no such image", "manifest unknown")
}

var (
	errContainerNotFoundSentinel = errors.New("container not found sentinel")
	errImageNotFoundSentinel     = errors.New("image not found sentinel")
)

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) <= len(s) && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
