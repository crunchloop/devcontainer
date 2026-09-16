package compose

import (
	"fmt"
	"sort"
	"strconv"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// In-memory counterparts of WriteBuildOverride / WriteRunOverride.
// The native orchestrator (compose.Orchestrator) reads a Plan whose
// *types.Project has already been mutated to reflect engine
// additions — no YAML round-trip, no tmpfiles, no merge surprises.
//
// The shell-out path in runtime/docker/compose.go keeps using the
// existing WriteBuildOverride / WriteRunOverride file emitters
// until PR17 deletes that path.

// ApplyBuildOverride mutates project so the named service's image is
// pinned to imageRef and any build: directive is cleared. Mirrors
// WriteBuildOverride's behavior; safe to call on a freshly loaded
// project. Engine.Up applies it to the primary service (with the
// feature-extended image) and to every sidecar it built.
//
// Returns an error if the service is missing.
func ApplyBuildOverride(project *composetypes.Project, service, imageRef string) error {
	if project == nil {
		return fmt.Errorf("ApplyBuildOverride: nil project")
	}
	if service == "" {
		return fmt.Errorf("ApplyBuildOverride: service required")
	}
	if imageRef == "" {
		return fmt.Errorf("ApplyBuildOverride: imageRef required")
	}
	svc, ok := project.Services[service]
	if !ok {
		return fmt.Errorf("ApplyBuildOverride: service %q not found in project", service)
	}
	svc.Image = imageRef
	// Compose v2 keeps Image and Build mutually exclusive at orchestration
	// time; clearing Build here mirrors the `build: !reset null` we emit
	// in the YAML override. compose-go represents the field as a pointer
	// so nil = unset.
	svc.Build = nil
	project.Services[service] = svc
	return nil
}

// ApplyRunOverride mutates project so the primary service has the
// workspace bind mount, container env, and labels merged in. Existing
// user-declared volumes / environment / labels are preserved (we
// append, we don't replace) — unlike the YAML write path, which
// re-emits everything to dodge compose's sequence-replace merge,
// mutating in memory is naturally additive.
//
// Returns an error if the primary service is missing.
func ApplyRunOverride(project *composetypes.Project, primaryService string, ov Override) error {
	if project == nil {
		return fmt.Errorf("ApplyRunOverride: nil project")
	}
	if primaryService == "" {
		return fmt.Errorf("ApplyRunOverride: primaryService required")
	}
	svc, ok := project.Services[primaryService]
	if !ok {
		return fmt.Errorf("ApplyRunOverride: primary service %q not found in project", primaryService)
	}

	// Dedupe by target. The user's compose file may already declare
	// the workspace bind mount (a common idiom for compose-source
	// devcontainers), in which case appending ours produces a
	// duplicate-mount-point error at ContainerCreate time. If the
	// target is already present, leave the user's source as-is; our
	// declarations override only when the target wasn't declared.
	existingTargets := make(map[string]struct{}, len(svc.Volumes))
	for _, v := range svc.Volumes {
		existingTargets[v.Target] = struct{}{}
	}
	for _, b := range ov.ExtraBindMounts {
		if _, exists := existingTargets[b.Target]; exists {
			continue
		}
		svc.Volumes = append(svc.Volumes, composetypes.ServiceVolumeConfig{
			Type:     composetypes.VolumeTypeBind,
			Source:   b.Source,
			Target:   b.Target,
			ReadOnly: b.ReadOnly,
		})
		existingTargets[b.Target] = struct{}{}
	}

	if len(ov.ExtraEnvironment) > 0 {
		if svc.Environment == nil {
			svc.Environment = composetypes.MappingWithEquals{}
		}
		for k, v := range ov.ExtraEnvironment {
			val := v
			svc.Environment[k] = &val
		}
	}

	if len(ov.Labels) > 0 {
		if svc.Labels == nil {
			svc.Labels = composetypes.Labels{}
		}
		for k, v := range ov.Labels {
			svc.Labels[k] = v
		}
	}

	// Security/entrypoint options from merged feature+image metadata.
	// Pointers: nil means "leave the service's own value untouched" so we
	// never downgrade a user's `privileged: true` when no feature asked
	// for it. Slices: union (dedup) with the service's existing entries.
	if ov.Privileged != nil {
		svc.Privileged = *ov.Privileged
	}
	if ov.Init != nil {
		svc.Init = ov.Init
	}
	svc.CapAdd = unionStrings(svc.CapAdd, ov.CapAdd)
	svc.SecurityOpt = unionStrings(svc.SecurityOpt, ov.SecurityOpt)

	// Feature-entrypoint chaining. Replace the service entrypoint with a
	// wrapper that runs each feature entrypoint then execs the original
	// entrypoint+command. Not escaped: the in-memory project is consumed
	// directly (no compose re-interpolation). The service's `command`
	// (svc.Command) is left untouched.
	if len(ov.Entrypoints) > 0 {
		svc.Entrypoint = composetypes.ShellCommand(
			RenderEntrypointWrapper(ov.Entrypoints, ov.OriginalEntrypoint, false),
		)
	}

	project.Services[primaryService] = svc
	return nil
}

// DroppedHostPort records one `ports:` entry removed by
// ApplyDropHostPorts, so the caller can report what it took away.
type DroppedHostPort struct {
	// Service is the compose service that declared the entry.
	Service string

	// HostIP is the address the entry asked to bind, empty for
	// compose's all-interfaces default.
	HostIP string

	// Published is the host-side port, empty when the entry left it
	// to the daemon (`ports: ["8080"]` — an ephemeral host port).
	Published string

	// Target is the container-side port the entry published.
	Target int

	// Protocol is "tcp" or "udp", empty for compose's tcp default.
	Protocol string
}

// String renders the entry in compose's own short syntax
// (`127.0.0.1:8080:8080/tcp`), so a diagnostic can quote the line the
// user wrote rather than a shape only this package knows.
func (d DroppedHostPort) String() string {
	proto := d.Protocol
	if proto == "" {
		proto = "tcp"
	}
	out := strconv.Itoa(d.Target) + "/" + proto
	if d.Published != "" {
		out = d.Published + ":" + out
	}
	if d.HostIP != "" {
		out = d.HostIP + ":" + out
	}
	return out
}

// ApplyDropHostPorts mutates project so no service publishes a host
// port, and returns every entry it removed in service-name order.
//
// For an engine whose daemon's "host" is a namespace shared with
// workloads it does not own — the engine running inside a Kubernetes
// pod next to sidecars — a published port is not the project's to
// take: it collides with whatever else already listens there, and
// nothing consumes it, because service-to-service traffic goes over
// the compose network by service name and an embedder forwarding a
// port out of the namespace dials the container on that network.
//
// Dropping the entries here rather than inside the orchestrator is
// deliberate: ConfigHash reads the project's ServiceConfig, so a
// container created with the bindings drifts from one created
// without them and is recreated, instead of being reused with its
// publishes intact.
//
// Entries are removed whole, including ones that left `published`
// unset — those bind an ephemeral host port, which is the same
// namespace and the same problem.
func ApplyDropHostPorts(project *composetypes.Project) []DroppedHostPort {
	if project == nil {
		return nil
	}

	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	var dropped []DroppedHostPort
	for _, name := range names {
		svc := project.Services[name]
		if len(svc.Ports) == 0 {
			continue
		}
		for _, p := range svc.Ports {
			dropped = append(dropped, DroppedHostPort{
				Service:   name,
				HostIP:    p.HostIP,
				Published: p.Published,
				Target:    int(p.Target),
				Protocol:  p.Protocol,
			})
		}
		svc.Ports = nil
		project.Services[name] = svc
	}
	return dropped
}
