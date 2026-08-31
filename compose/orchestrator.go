// Package compose's runtime-agnostic orchestrator.
//
// The orchestrator drives any runtime.Runtime implementation through
// a Plan (Up) or DownPlan (Down). It owns compose semantics:
// topological ordering, idempotent reuse via ConfigHash, health
// gating, partial-failure handling, label-scoped teardown. The
// runtime implementation owns the backend specifics.
//
// See design/compose-native.md §5 for the algorithm.
//
// Scope of this initial commit (C6):
//   - Up: validate -> infrastructure (network + named volumes) ->
//     level-by-level service start -> reuse-or-recreate via
//     ConfigHash -> service_started gating.
//   - Down: list by project label -> stop + remove containers ->
//     remove network -> optionally remove volumes / images.
//   - service_healthy / service_completed_successfully gating: the
//     polling loop reads InspectContainer fields the runtime
//     already exposes.
//
// Out of scope here, picked up in later PRs:
//   - Port bindings (RunSpec doesn't carry them yet).
//   - --rmi local execution (the primitive exists; orchestrator
//     wiring is a one-line addition in C8 if we want it).
//   - Per-service health-timeout overrides (single global today).
package compose

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/runtime"
)

// Labels stamped on every container the orchestrator creates.
// Compose-CLI interop labels coexist with our own engine labels so
// the user's `docker compose ps` keeps working against our
// containers, while convergence policy stays internal to our hash.
//
// See design/compose-native.md §3.1 for full provenance.
const (
	LabelComposeProject = "com.docker.compose.project"
	LabelComposeService = "com.docker.compose.service"
	LabelComposeOneoff  = "com.docker.compose.oneoff"
	LabelEngine         = "dev.containers.engine"
	LabelConfigHash     = "dev.containers.config-hash"
	LabelImageDigest    = "dev.containers.image-digest"
)

// EngineDisplayName identifies our orchestrator in stamped labels.
// Aligned with the constant at attach.go scope, but kept local to
// avoid a package-import cycle on the engine.
const EngineDisplayName = "devcontainer-go/compose"

// DefaultHealthTimeout bounds how long Orchestrator.Up will poll
// for a depends_on health/completion condition before giving up.
// Configurable per call via Orchestrator.HealthTimeout.
const DefaultHealthTimeout = 60 * time.Second

// Orchestrator implements compose Up / Down against a runtime.
// Construct with NewOrchestrator. Methods are safe for sequential
// use; concurrent invocations against the same project are caller's
// responsibility.
type Orchestrator struct {
	rt runtime.Runtime

	// HealthTimeout overrides DefaultHealthTimeout. Applied per
	// depends_on edge, not for the whole Up.
	HealthTimeout time.Duration

	// PollInterval is the cadence for health polling. Tests
	// override; production default below.
	PollInterval time.Duration
}

// NewOrchestrator constructs an Orchestrator with sane defaults.
func NewOrchestrator(rt runtime.Runtime) *Orchestrator {
	return &Orchestrator{
		rt:            rt,
		HealthTimeout: DefaultHealthTimeout,
		PollInterval:  500 * time.Millisecond,
	}
}

// UpResult reports the per-service outcome of Up.
type UpResult struct {
	// ContainerIDs maps service name -> backend container ID for
	// every service that ended Up running. Includes reused
	// containers (config-hash hit). Failed services are absent.
	ContainerIDs map[string]string

	// Network is the project's default network's backend ID, or ""
	// if creation failed.
	Network string
}

// Up applies the plan: validate, create infrastructure, level-by-
// level start with reuse-or-recreate semantics. Returns a
// *PartialUpError if a service fails after one or more services
// already started; the already-running services are NOT torn down
// (debuggability matters more than tidiness — see design §5.3).
func (o *Orchestrator) Up(ctx context.Context, plan *Plan) (UpResult, error) {
	if err := plan.Validate(); err != nil {
		return UpResult{}, err
	}

	levels, err := TopoSort(plan.Project)
	if err != nil {
		return UpResult{}, err
	}

	projectLabels := map[string]string{
		LabelComposeProject: plan.ProjectName,
		LabelEngine:         EngineDisplayName,
	}

	res := UpResult{ContainerIDs: map[string]string{}}

	// Project network.
	netID, err := o.rt.CreateNetwork(ctx, runtime.NetworkSpec{
		Name:   plan.ProjectName + "_default",
		Labels: projectLabels,
	})
	if err != nil {
		return res, fmt.Errorf("compose.Up: project network: %w", err)
	}
	res.Network = netID

	// Named volumes referenced by at least one service in the plan.
	if err := o.ensureNamedVolumes(ctx, plan, projectLabels); err != nil {
		return res, fmt.Errorf("compose.Up: named volumes: %w", err)
	}

	// Filter the level list against plan.Services if the caller
	// limited which services to bring up.
	keep := makeKeepSet(plan)

	// resolveContainer maps a service name to its already-started
	// container ID, for `service:<x>` namespace-mode references. The
	// target is always in an earlier level (serviceEdges makes it a
	// TopoSort dependency), but same-level writes to the map are
	// concurrent, so reads take the same lock.
	var idsMu sync.Mutex
	resolveContainer := func(name string) string {
		idsMu.Lock()
		defer idsMu.Unlock()
		return res.ContainerIDs[name]
	}

	for _, level := range levels {
		var started []string
		var startMu sync.Mutex
		var firstErr error
		var firstSvc string

		var wg sync.WaitGroup
		for _, svcName := range level {
			if !keep[svcName] {
				continue
			}
			svcName := svcName
			wg.Add(1)
			go func() {
				defer wg.Done()
				svc, ok := plan.Project.Services[svcName]
				if !ok {
					return
				}
				id, err := o.ensureService(ctx, plan, svc, projectLabels, resolveContainer)
				startMu.Lock()
				defer startMu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						firstSvc = svcName
					}
					return
				}
				idsMu.Lock()
				res.ContainerIDs[svcName] = id
				idsMu.Unlock()
				started = append(started, svcName)
			}()
		}
		wg.Wait()

		if firstErr != nil {
			sort.Strings(started)
			startedAccum := mapKeys(res.ContainerIDs)
			sort.Strings(startedAccum)
			return res, &PartialUpError{
				Started: startedAccum,
				Failed:  firstSvc,
				Err:     firstErr,
			}
		}

		// Health-gate against the SAME level's services? No: only
		// edges from this level INTO the next level matter, and
		// Kahn's algorithm guarantees this level's deps are already
		// in earlier levels (and already gated when we left them).
		// Future levels gate against THIS level's services in their
		// own iteration via gateLevel below.
		if err := o.gateLevel(ctx, plan, level, res.ContainerIDs, keep); err != nil {
			return res, err
		}
	}

	return res, nil
}

// Down tears down a project. Idempotent: missing resources are
// no-ops; missing project leaves no observable state change.
func (o *Orchestrator) Down(ctx context.Context, plan *DownPlan) error {
	if plan.ProjectName == "" {
		return fmt.Errorf("compose.Down: ProjectName required")
	}

	containers, err := o.rt.ListContainers(ctx, runtime.LabelFilter{
		Match: map[string]string{LabelComposeProject: plan.ProjectName},
	})
	if err != nil {
		return fmt.Errorf("compose.Down: ListContainers: %w", err)
	}

	// Best-effort reverse topo if we have the project file. Without
	// it, order doesn't matter — every container is going away.
	containers = orderContainersForTeardown(containers, plan)

	for _, c := range containers {
		_ = o.rt.StopContainer(ctx, c.ID, runtime.StopOptions{Timeout: 10 * time.Second})
		if err := o.rt.RemoveContainer(ctx, c.ID, runtime.RemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("compose.Down: RemoveContainer(%s): %w", c.ID, err)
		}
	}

	// Remove the project network by name. Docker's NetworkRemove
	// resolves by id-or-name, and our CreateNetwork uses
	// <project>_default as the name + id, so RemoveNetwork with the
	// same string works. The call is idempotent — missing-network
	// errors are swallowed at the backend.
	_ = o.rt.RemoveNetwork(ctx, plan.ProjectName+"_default")

	if plan.RemoveVolumes {
		if plan.Project != nil {
			for volName := range plan.Project.Volumes {
				_ = o.rt.RemoveVolume(ctx, plan.ProjectName+"_"+volName)
			}
		}
	}

	if plan.RemoveImages {
		imgs, err := o.rt.ListImages(ctx, runtime.LabelFilter{
			Match: map[string]string{LabelComposeProject: plan.ProjectName},
		})
		if err == nil {
			for _, img := range imgs {
				_ = o.rt.RemoveImage(ctx, img.ID)
			}
		}
	}

	return nil
}

// ensureNamedVolumes creates volumes the plan references but doesn't
// touch ones it doesn't. compose-go puts the top-level volumes:
// list on the project; we cross-check service usage so unused
// declarations don't trigger creation.
func (o *Orchestrator) ensureNamedVolumes(ctx context.Context, plan *Plan, labels map[string]string) error {
	if plan.Project == nil {
		return nil
	}
	used := map[string]struct{}{}
	for _, svc := range plan.Project.Services {
		for _, v := range svc.Volumes {
			if v.Type == composetypes.VolumeTypeVolume && v.Source != "" {
				used[v.Source] = struct{}{}
			}
		}
	}
	for name := range used {
		if _, declared := plan.Project.Volumes[name]; !declared {
			continue
		}
		volLabels := copyLabels(labels)
		volLabels["com.docker.compose.volume"] = name
		_, err := o.rt.CreateVolume(ctx, runtime.VolumeSpec{
			Name:   plan.ProjectName + "_" + name,
			Labels: volLabels,
		})
		if err != nil {
			return fmt.Errorf("CreateVolume(%s): %w", name, err)
		}
	}
	return nil
}

// ensureService runs the per-service state machine: reuse on
// config-hash match, otherwise stop+remove existing then create
// fresh. Returns the container ID on success.
func (o *Orchestrator) ensureService(
	ctx context.Context,
	plan *Plan,
	svc composetypes.ServiceConfig,
	projectLabels map[string]string,
	resolveContainer func(string) string,
) (string, error) {
	// Resolve the service's image to its digest before hashing. The
	// compose file carries a tag (e.g. "postgres:17-alpine") which is
	// mutable on the registry side — `docker pull` against the same
	// tag can land a different digest. Hashing the tag means we'd
	// silently reuse an old container after a tag update; hashing the
	// digest forces a recreate, matching docker/compose's
	// ImageDigestLabel check (convergence.go).
	imageDigest := o.resolveImageDigest(ctx, svc.Image)
	hash := ConfigHash(imageDigest, svc)

	// Try to find an existing container for this (project, service).
	existing, err := o.rt.ListContainers(ctx, runtime.LabelFilter{
		Match: map[string]string{
			LabelComposeProject: plan.ProjectName,
			LabelComposeService: svc.Name,
		},
	})
	if err != nil {
		return "", fmt.Errorf("ListContainers(%s): %w", svc.Name, err)
	}

	if len(existing) > 0 {
		c := existing[0]
		// Resume: reattach on-disk state exactly. Reuse the existing
		// container unconditionally — start if stopped, attach if
		// running — without consulting the config-hash. A restored
		// workspace's primary image is feature-layered fresh every boot
		// (new digest), so a hash check would force a recreate that
		// abandons the container's upperdir and binds a new empty
		// anonymous volume; adoption keeps both. Recreate-mode Up has
		// already torn the project down before reaching here, so this
		// only fires on non-recreating (resume) Ups.
		if plan.AdoptExisting {
			if c.State != runtime.StateRunning {
				if err := o.rt.StartContainer(ctx, c.ID); err != nil {
					return "", fmt.Errorf("StartContainer(resumed %s): %w", svc.Name, err)
				}
			}
			return c.ID, nil
		}
		// Inspect to read the stored hash + image digest. Recreate if
		// either has drifted; the image-digest check is the second
		// line of defense for tag-update scenarios where ConfigHash
		// might match by accident (e.g. a digest probe that returned
		// empty).
		details, ierr := o.rt.InspectContainer(ctx, c.ID)
		if ierr == nil && details != nil {
			if _, managed := details.Labels[LabelConfigHash]; !managed {
				// A container for this (project, service) that this
				// orchestrator did not create — the shellout backend or a
				// plain `docker compose up`. Adopt it instead of
				// recreating: without our labels there is no stored hash
				// to detect drift against, and removing it would destroy
				// the writable layer (the user's $HOME inside the
				// container, etc.) that the shellout path's NoRecreate
				// contract preserved across restarts. A caller that wants
				// these replaced asks for it explicitly: a Recreate-mode
				// Up tears the whole project down before reaching here.
				if c.State != runtime.StateRunning {
					if err := o.rt.StartContainer(ctx, c.ID); err != nil {
						return "", fmt.Errorf("StartContainer(adopted %s): %w", svc.Name, err)
					}
				}
				return c.ID, nil
			}
			if details.Labels[LabelConfigHash] == hash &&
				details.Labels[LabelImageDigest] == imageDigest {
				// Config and image match — the container is reusable.
				// If it's not currently Running (e.g. dockerd was just
				// restarted and brought stopped containers back from
				// on-disk state), start it instead of destroying it.
				// Recreating would lose the writable layer — see issue #71.
				if c.State != runtime.StateRunning {
					if err := o.rt.StartContainer(ctx, c.ID); err != nil {
						return "", fmt.Errorf("StartContainer(%s): %w", svc.Name, err)
					}
				}
				return c.ID, nil
			}
		}
		// Different config — recreate.
		_ = o.rt.StopContainer(ctx, c.ID, runtime.StopOptions{Timeout: 10 * time.Second})
		if err := o.rt.RemoveContainer(ctx, c.ID, runtime.RemoveOptions{Force: true}); err != nil {
			return "", fmt.Errorf("RemoveContainer(%s): %w", c.ID, err)
		}
	}

	spec, err := serviceToRunSpec(plan, svc, projectLabels, hash, imageDigest, resolveContainer)
	if err != nil {
		return "", err
	}
	c, err := o.rt.RunContainer(ctx, spec)
	if err != nil {
		// Compose's `up -d` pulls missing images implicitly. Mirror
		// that here: on the first attempt's image-not-found, pull
		// then retry once. Anything else propagates.
		var nf *runtime.ImageNotFoundError
		if errors.As(err, &nf) && svc.Image != "" {
			if _, perr := o.rt.PullImage(ctx, svc.Image, nil); perr != nil {
				return "", fmt.Errorf("PullImage(%s) for service %q: %w", svc.Image, svc.Name, perr)
			}
			c, err = o.rt.RunContainer(ctx, spec)
		}
		if err != nil {
			return "", fmt.Errorf("RunContainer(%s): %w", svc.Name, err)
		}
	}
	if err := o.rt.StartContainer(ctx, c.ID); err != nil {
		return c.ID, fmt.Errorf("StartContainer(%s): %w", svc.Name, err)
	}
	return c.ID, nil
}

// resolveImageDigest returns a stable identifier for the image —
// the local store's digest if InspectImage can resolve it, the
// reference itself as a fallback. Empty input returns empty.
//
// The fallback path matters: at first Up, the image hasn't been
// pulled yet, so Inspect returns ImageNotFoundError. Using the
// reference is the right thing because the hash will be
// recalculated after pull-on-miss recreates and stamps it.
// Re-runs against a moved tag pull a different digest, and the
// next Inspect surfaces the new digest, recreating the container.
func (o *Orchestrator) resolveImageDigest(ctx context.Context, ref string) string {
	if ref == "" {
		return ""
	}
	if details, err := o.rt.InspectImage(ctx, ref); err == nil && details != nil {
		if details.ID != "" {
			return details.ID
		}
	}
	return ref
}

// gateLevel polls dependents at the just-completed level for any
// downstream health/completion conditions. Returns *HealthTimeoutError
// if a service fails to satisfy its condition in time.
func (o *Orchestrator) gateLevel(
	ctx context.Context,
	plan *Plan,
	level Level,
	containerIDs map[string]string,
	keep map[string]bool,
) error {
	// We need to inspect dependents on FUTURE levels; instead, we
	// gate THIS level by waiting for its services that any dependent
	// will require to be healthy/exited.
	// Build a set: services in this level that some kept dependent
	// requires via service_healthy / service_completed_successfully.
	type requirement struct {
		condition string
		optional  bool
	}
	required := map[string]requirement{} // service -> requirement
	for _, dependent := range plan.Project.Services {
		if !keep[dependent.Name] {
			continue
		}
		for depName, dep := range dependent.DependsOn {
			if !containsString(level, depName) {
				continue
			}
			switch dep.Condition {
			case "service_healthy", "service_completed_successfully":
				// Per compose spec, depends_on with required:false
				// means the dependent should start best-effort even
				// when the dep isn't ready. Honour an existing
				// requirement (someone else may need this dep
				// strictly), but downgrade to optional if no strict
				// requirement is in place.
				prev, exists := required[depName]
				if exists && !prev.optional {
					continue
				}
				required[depName] = requirement{
					condition: dep.Condition,
					optional:  !dep.Required,
				}
			}
		}
	}
	if len(required) == 0 {
		return nil
	}

	deadline := time.Now().Add(o.HealthTimeout)
	for svcName, req := range required {
		cid := containerIDs[svcName]
		if cid == "" {
			continue
		}
		// Whether the service itself declares an active healthcheck.
		// Distinguishes "image has no HEALTHCHECK" from "backend does
		// not surface health" below.
		declaresHealthcheck := false
		if cfg, ok := plan.Project.Services[svcName]; ok {
			declaresHealthcheck = cfg.HealthCheck != nil && !cfg.HealthCheck.Disable
		}
		if err := o.waitFor(ctx, svcName, cid, req.condition, declaresHealthcheck, deadline); err != nil {
			if req.optional {
				// Per compose spec: a non-required dependency that
				// fails to satisfy its condition does not block the
				// dependent's start. Swallow the gate error and move
				// on; the dependent will start best-effort.
				continue
			}
			return err
		}
	}
	return nil
}

// waitFor polls a service's condition until satisfied or deadline. Both
// conditions read container state from the backend's inspect; no native
// healthcheck is required either way.
func (o *Orchestrator) waitFor(
	ctx context.Context, svc, id, cond string,
	declaresHealthcheck bool, deadline time.Time,
) error {
	healthUnreported := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		details, err := o.rt.InspectContainer(ctx, id)
		if err == nil && details != nil {
			switch cond {
			case "service_healthy":
				// Treat HealthNone as satisfied: a container with
				// no HEALTHCHECK directive can still be a
				// service_healthy gate target (compose's behavior),
				// so falling back to State=Running keeps that case
				// working. For services that DO declare a
				// healthcheck, require Healthy explicitly.
				switch details.Health {
				case runtime.HealthHealthy:
					return nil
				case runtime.HealthNone:
					// HealthNone is ambiguous per
					// runtime.HealthStatus: either the image
					// declared no HEALTHCHECK, or the backend
					// does not surface health at all. When the
					// service declares one itself, "no status"
					// cannot mean "no healthcheck" — passing the
					// gate here would start dependents before the
					// check ever succeeded. Keep waiting and
					// report it at the deadline instead.
					if declaresHealthcheck {
						healthUnreported = details.State == runtime.StateRunning
						break
					}
					if details.State == runtime.StateRunning {
						return nil
					}
				case runtime.HealthUnhealthy:
					return fmt.Errorf(
						"compose: service %q reported unhealthy while waiting on service_healthy",
						svc,
					)
				}
			case "service_completed_successfully":
				if details.State == runtime.StateExited && details.ExitCode == 0 {
					return nil
				}
				if details.State == runtime.StateExited && details.ExitCode != 0 {
					return fmt.Errorf(
						"compose: %s exited with code %d while waiting for completion",
						svc, details.ExitCode,
					)
				}
			}
		}
		if time.Now().After(deadline) {
			if healthUnreported {
				return fmt.Errorf(
					"compose: service %q declares a healthcheck but the backend never reported a health status; service_healthy cannot be honored on this backend",
					svc,
				)
			}
			return &HealthTimeoutError{
				Service:   svc,
				Condition: cond,
				Waited:    o.HealthTimeout.String(),
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.PollInterval):
		}
	}
}

// serviceToRunSpec is the in-memory translation from compose's
// ServiceConfig to runtime.RunSpec — env / labels / mounts / command /
// entrypoint / user / workdir / init / cap_add / privileged /
// security_opt. The security fields are populated by ApplyRunOverride
// from merged feature+image metadata (e.g. docker-in-docker's
// privileged/init/capAdd), mirroring the flags the image path applies.
// RunArgs has no compose-typed equivalent and stays zero.
func serviceToRunSpec(
	plan *Plan,
	svc composetypes.ServiceConfig,
	projectLabels map[string]string,
	hash, imageDigest string,
	resolveContainer func(string) string,
) (runtime.RunSpec, error) {
	labels := copyLabels(plan.Labels)
	for k, v := range projectLabels {
		labels[k] = v
	}
	labels[LabelComposeService] = svc.Name
	labels[LabelComposeOneoff] = "False"
	labels[LabelConfigHash] = hash
	if imageDigest != "" {
		labels[LabelImageDigest] = imageDigest
	}
	for k, v := range svc.Labels {
		// User labels never override our convergence labels.
		if _, reserved := reservedLabels[k]; reserved {
			continue
		}
		labels[k] = v
	}

	env := map[string]string{}
	for k, vptr := range svc.Environment {
		if vptr != nil {
			env[k] = *vptr
		}
	}

	mounts := make([]runtime.MountSpec, 0, len(svc.Volumes))
	for _, v := range svc.Volumes {
		mounts = append(mounts, runtime.MountSpec{
			Type:     mountTypeOf(v.Type),
			Source:   mountSourceOf(v, plan.ProjectName),
			Target:   v.Target,
			ReadOnly: v.ReadOnly,
		})
	}

	// Namespace modes, resolved to HostConfig syntax. A service that
	// joins another container's network namespace (or opts out with
	// "none"/"host") must not also get per-network endpoint config —
	// Docker rejects the combination — so the project network is
	// skipped for it. Service-name DNS is then the joined namespace's
	// concern, matching `docker compose` semantics.
	networkMode, err := resolveNamespaceMode(svc.NetworkMode, resolveContainer)
	if err != nil {
		return runtime.RunSpec{}, fmt.Errorf("service %q network_mode: %w", svc.Name, err)
	}
	pidMode, err := resolveNamespaceMode(svc.Pid, resolveContainer)
	if err != nil {
		return runtime.RunSpec{}, fmt.Errorf("service %q pid: %w", svc.Name, err)
	}
	ipcMode, err := resolveNamespaceMode(svc.Ipc, resolveContainer)
	if err != nil {
		return runtime.RunSpec{}, fmt.Errorf("service %q ipc: %w", svc.Name, err)
	}
	networks := []string{plan.ProjectName + "_default"}
	if networkMode != "" {
		networks = nil
	}

	memBytes, nanoCPUs := resourcesOf(svc)
	return runtime.RunSpec{
		Image:         svc.Image,
		Name:          plan.ProjectName + "-" + svc.Name + "-1",
		Cmd:           []string(svc.Command),
		Entrypoint:    []string(svc.Entrypoint),
		User:          svc.User,
		WorkingDir:    svc.WorkingDir,
		Env:           env,
		Labels:        labels,
		Mounts:        mounts,
		Networks:      networks,
		NetworkMode:   networkMode,
		PidMode:       pidMode,
		IpcMode:       ipcMode,
		Ports:         portsOf(svc.Ports),
		RestartPolicy: restartPolicyOf(svc.Restart),
		HealthCheck:   healthCheckOf(svc.HealthCheck),
		Init:          svc.Init != nil && *svc.Init,
		CapAdd:        svc.CapAdd,
		Privileged:    svc.Privileged,
		SecurityOpt:   svc.SecurityOpt,
		MemoryBytes:   memBytes,
		NanoCPUs:      nanoCPUs,
	}, nil
}

// resolveNamespaceMode translates a compose namespace-mode value into
// Docker HostConfig syntax. `service:<x>` becomes `container:<id>`
// via the dependency's already-started container (serviceEdges makes
// the target a TopoSort dependency, so it lives in an earlier level);
// everything else — "", "host", "none", "container:<id>" — passes
// through verbatim.
func resolveNamespaceMode(v string, resolveContainer func(string) string) (string, error) {
	target := serviceRefTarget(v)
	if target == "" {
		return v, nil
	}
	id := resolveContainer(target)
	if id == "" {
		return "", fmt.Errorf("mode %q: service %q has no started container to join", v, target)
	}
	return "container:" + id, nil
}

// resourcesOf extracts the memory + CPU limits from a compose service.
// deploy.resources.limits (compose v3+) wins over the legacy top-level
// mem_limit / cpus fields when both are set, matching docker compose's
// own precedence. Zero values mean "unset" — the backend's default
// applies.
func resourcesOf(svc composetypes.ServiceConfig) (memBytes, nanoCPUs int64) {
	if d := svc.Deploy; d != nil {
		if lim := d.Resources.Limits; lim != nil {
			memBytes = int64(lim.MemoryBytes)
			if cpus := lim.NanoCPUs.Value(); cpus > 0 {
				nanoCPUs = int64(cpus * 1_000_000_000)
			}
		}
	}
	if memBytes == 0 {
		memBytes = int64(svc.MemLimit)
	}
	if nanoCPUs == 0 && svc.CPUS > 0 {
		nanoCPUs = int64(svc.CPUS * 1_000_000_000)
	}
	return memBytes, nanoCPUs
}

// healthCheckOf translates compose's HealthCheckConfig pointer into
// our runtime-neutral spec. Returns nil if the service didn't
// declare one (image's HEALTHCHECK applies as-is).
func healthCheckOf(in *composetypes.HealthCheckConfig) *runtime.HealthCheckSpec {
	if in == nil {
		return nil
	}
	out := &runtime.HealthCheckSpec{
		Test:    append([]string(nil), in.Test...),
		Disable: in.Disable,
	}
	if in.Interval != nil {
		out.Interval = time.Duration(*in.Interval)
	}
	if in.Timeout != nil {
		out.Timeout = time.Duration(*in.Timeout)
	}
	if in.Retries != nil {
		out.Retries = int(*in.Retries)
	}
	if in.StartPeriod != nil {
		out.StartPeriod = time.Duration(*in.StartPeriod)
	}
	if in.StartInterval != nil {
		out.StartInterval = time.Duration(*in.StartInterval)
	}
	return out
}

// portsOf translates compose's ServicePortConfig list into our
// runtime-neutral PortBinding shape.
func portsOf(in []composetypes.ServicePortConfig) []runtime.PortBinding {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.PortBinding, 0, len(in))
	for _, p := range in {
		out = append(out, runtime.PortBinding{
			HostIP:        p.HostIP,
			HostPort:      p.Published,
			ContainerPort: int(p.Target),
			Protocol:      p.Protocol,
		})
	}
	return out
}

// restartPolicyOf maps compose's restart: string onto our typed
// runtime.RestartPolicy. Unknown / empty values map to RestartNo.
func restartPolicyOf(s string) runtime.RestartPolicy {
	switch s {
	case "always":
		return runtime.RestartAlways
	case "on-failure":
		return runtime.RestartOnFailure
	case "unless-stopped":
		return runtime.RestartUnlessStopped
	default:
		return runtime.RestartNo
	}
}

var reservedLabels = map[string]struct{}{
	LabelComposeProject: {},
	LabelComposeService: {},
	LabelComposeOneoff:  {},
	LabelEngine:         {},
	LabelConfigHash:     {},
	LabelImageDigest:    {},
}

func mountTypeOf(s string) runtime.MountType {
	switch s {
	case composetypes.VolumeTypeBind:
		return runtime.MountBind
	case composetypes.VolumeTypeVolume:
		return runtime.MountVolume
	case composetypes.VolumeTypeTmpfs:
		return runtime.MountTmpfs
	}
	return runtime.MountBind
}

// mountSourceOf returns the host-side source for a service volume.
// Named volumes need their project-scoped name; binds and tmpfs
// pass through. Anonymous named volumes (Type=volume, Source="") fall
// through to an empty source — docker's convention is that the
// daemon assigns a random volume name on create. The mount still
// flows to the backend; ensureNamedVolumes intentionally skips them
// so we don't try to pre-create unnamed volumes.
func mountSourceOf(v composetypes.ServiceVolumeConfig, projectName string) string {
	if v.Type == composetypes.VolumeTypeVolume && v.Source != "" {
		return projectName + "_" + v.Source
	}
	return v.Source
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func makeKeepSet(plan *Plan) map[string]bool {
	keep := map[string]bool{}
	if len(plan.Services) == 0 {
		for name := range plan.Project.Services {
			keep[name] = true
		}
		return keep
	}
	// `docker compose up <names...>` starts the named services and
	// their transitive dependencies; a restricted Plan keeps that
	// contract, or a kept service would come up with its depends_on
	// (or `service:<x>` namespace target) absent.
	for _, name := range ServiceClosure(plan.Project, plan.Services) {
		keep[name] = true
	}
	return keep
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// orderContainersForTeardown returns containers in dependency-reverse
// order if the plan has a project; otherwise in name order for
// deterministic test output.
func orderContainersForTeardown(in []runtime.Container, plan *DownPlan) []runtime.Container {
	out := append([]runtime.Container(nil), in...)
	if plan.Project == nil {
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	levels, err := TopoSort(plan.Project)
	if err != nil {
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	// Build name -> level index for ordering. Higher level = later
	// dependent, must be torn down first.
	levelIdx := map[string]int{}
	for i, lvl := range levels {
		for _, s := range lvl {
			levelIdx[s] = i
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		li := levelIdx[serviceLabelOf(out[i])]
		lj := levelIdx[serviceLabelOf(out[j])]
		if li != lj {
			return li > lj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// serviceLabelOf reads the compose service name off a container.
// Prefers the com.docker.compose.service label (always present on
// orchestrator-created containers); falls back to the container
// name for resilience against backends that drop labels through
// ListContainers. Returns "" if neither is available — the
// surrounding sort will tie-break by container name in that case.
func serviceLabelOf(c runtime.Container) string {
	if v := c.Labels[LabelComposeService]; v != "" {
		return v
	}
	return c.Name
}
