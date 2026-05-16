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
//     polling loop is in place but only reads InspectContainer
//     fields the runtime already exposes; once Apple gains health
//     and exit-code surfacing the orchestrator code does not change.
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

	// BackendName identifies the backend in error messages. Empty
	// is allowed but reduces error-message clarity.
	BackendName string

	// HealthTimeout overrides DefaultHealthTimeout. Applied per
	// depends_on edge, not for the whole Up.
	HealthTimeout time.Duration

	// PollInterval is the cadence for health polling. Tests
	// override; production default below.
	PollInterval time.Duration
}

// NewOrchestrator constructs an Orchestrator with sane defaults.
func NewOrchestrator(rt runtime.Runtime, backendName string) *Orchestrator {
	return &Orchestrator{
		rt:            rt,
		BackendName:   backendName,
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
	if err := plan.Validate(o.BackendName, o.rt.Capabilities()); err != nil {
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
				id, err := o.ensureService(ctx, plan, svc, projectLabels)
				startMu.Lock()
				defer startMu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						firstSvc = svcName
					}
					return
				}
				res.ContainerIDs[svcName] = id
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

	// Project network.
	nets, err := o.rt.ListContainers(ctx, runtime.LabelFilter{
		Match: map[string]string{LabelComposeProject: plan.ProjectName},
	})
	_ = nets // ListNetworks would be needed for full parity; the
	// network's name follows our naming convention, so we look it
	// up via the spec the same way we created it. Skipping for v1:
	// RemoveNetwork by name isn't on the interface — backends use
	// IDs. The orchestrator therefore doesn't try to clean up the
	// network in Down today. Production callers that want network
	// cleanup pass the project, in which case we know the network
	// name; the backend's NetworkList(name=<...>) would surface it.
	// For minimal C6 scope this is a documented limitation; C8
	// flips on RemoveNetwork once the engine's Down call site is
	// wired.

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
) (string, error) {
	hash := ConfigHash(svc.Image, svc)

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
		// Inspect to read the stored hash off labels.
		details, ierr := o.rt.InspectContainer(ctx, c.ID)
		if ierr == nil && details != nil &&
			details.Labels[LabelConfigHash] == hash &&
			c.State == runtime.StateRunning {
			return c.ID, nil
		}
		// Different config or not running — recreate.
		_ = o.rt.StopContainer(ctx, c.ID, runtime.StopOptions{Timeout: 10 * time.Second})
		if err := o.rt.RemoveContainer(ctx, c.ID, runtime.RemoveOptions{Force: true}); err != nil {
			return "", fmt.Errorf("RemoveContainer(%s): %w", c.ID, err)
		}
	}

	spec := serviceToRunSpec(plan, svc, projectLabels, hash)
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
	required := map[string]string{} // service -> condition
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
				required[depName] = dep.Condition
			}
		}
	}
	if len(required) == 0 {
		return nil
	}

	deadline := time.Now().Add(o.HealthTimeout)
	for svcName, cond := range required {
		cid := containerIDs[svcName]
		if cid == "" {
			continue
		}
		if err := o.waitFor(ctx, svcName, cid, cond, deadline); err != nil {
			return err
		}
	}
	return nil
}

// waitFor polls a service's condition until satisfied or deadline.
func (o *Orchestrator) waitFor(
	ctx context.Context, svc, id, cond string, deadline time.Time,
) error {
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
// ServiceConfig to runtime.RunSpec. This is intentionally minimal
// for C6 — env / labels / mounts / command / entrypoint / user /
// workdir / init / cap_add. RunArgs / Privileged / SecurityOpt are
// not in compose's typed model (those are docker-cli concepts), so
// they stay at their zero values. Ports / restart / healthcheck
// are RunSpec gaps to fix in a later PR.
func serviceToRunSpec(
	plan *Plan,
	svc composetypes.ServiceConfig,
	projectLabels map[string]string,
	hash string,
) runtime.RunSpec {
	labels := copyLabels(plan.Labels)
	for k, v := range projectLabels {
		labels[k] = v
	}
	labels[LabelComposeService] = svc.Name
	labels[LabelComposeOneoff] = "False"
	labels[LabelConfigHash] = hash
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
		Ports:         portsOf(svc.Ports),
		RestartPolicy: restartPolicyOf(svc.Restart),
		HealthCheck:   healthCheckOf(svc.HealthCheck),
		Init:          svc.Init != nil && *svc.Init,
		CapAdd:        []string(svc.CapAdd),
	}
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
// pass through.
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
	for _, name := range plan.Services {
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

// serviceLabelOf reads the compose service name off a container
// from its labels. Returns "" if missing (defensive — should never
// happen for orchestrator-created containers).
func serviceLabelOf(c runtime.Container) string {
	// Container struct doesn't carry Labels; the docker
	// ListContainers translation drops them. PR14 widens the
	// Container struct or adds an explicit LabelLookup primitive;
	// for now the orchestrator's Down ordering degrades to
	// name-based if labels aren't readable here, which is harmless
	// for correctness (Down is idempotent either way).
	return c.Name
}
