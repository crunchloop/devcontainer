package compose

import (
	"fmt"
	"sort"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/runtime"
)

// Plan describes a compose-project Up request in a runtime-neutral
// shape. The orchestrator constructs one from a loaded project and
// drives the runtime through it; the caller (Engine.Up) builds the
// Plan, optionally calls ApplyBuildOverride / ApplyRunOverride to
// inject the feature-extended image + workspace mount, then calls
// Validate + Orchestrator.Up.
type Plan struct {
	// Project is the fully-loaded, interpolation-resolved compose
	// project from compose.Load. The orchestrator reads from it but
	// does not mutate it; mutation is the override functions' job.
	Project *composetypes.Project

	// ProjectName scopes all backend resources (network, volumes,
	// container labels) for the project. Required.
	ProjectName string

	// Services optionally restricts which services to bring up.
	// Empty = all services in the loaded project (after profile
	// selection performed by compose.Load).
	Services []string

	// Labels are stamped on every container the orchestrator
	// creates, in addition to the project's own labels. Engine
	// fills these in with the devcontainer ID label set so
	// Engine.Attach can find the primary container.
	Labels map[string]string
}

// DownPlan describes a teardown request. Used by Orchestrator.Down
// (and indirectly Engine.Down). Unlike Plan, this does not require
// the project file — if the user has destroyed their compose file
// since Up, we can still tear down via the project label scan.
type DownPlan struct {
	ProjectName string

	// RemoveVolumes removes named volumes labelled with the project
	// after container removal. Mirrors compose's `--volumes` flag.
	RemoveVolumes bool

	// RemoveImages removes locally-built images labelled with the
	// project after container removal. Mirrors compose's `--rmi local`.
	RemoveImages bool

	// Project is optional. When non-nil, the orchestrator uses its
	// depends_on graph for reverse-topological teardown order; when
	// nil it falls back to parallel teardown (Down is idempotent
	// either way).
	Project *composetypes.Project
}

// Validate inspects the Plan against the active backend's
// Capabilities and the refused-feature list, returning a typed
// error on the first kind of refusal encountered. Calls are
// side-effect-free; safe to invoke before any backend interaction.
//
// Validation order:
//  1. Hard refusals (§2.2 fields we never implement): one
//     UnsupportedFieldError listing every offending site.
//  2. Backend-gated features (depends_on conditions, namespace
//     sharing, restart policies, shared volumes): one
//     UnsupportedFeatureOnBackendError per offending feature, or
//     a typed VolumeSharedAcrossServicesError for the volume case.
//
// Each kind returns the FIRST error of that kind found; if no
// refusals trigger, Validate returns nil.
func (p *Plan) Validate(backendName string, caps runtime.Capabilities) error {
	if p == nil || p.Project == nil {
		return fmt.Errorf("compose.Plan.Validate: nil plan or project")
	}

	// Pass 1: hard refusals. Collect every offending field across
	// the project so the user can fix them in a single edit.
	if err := refuseUnsupportedFields(p.Project); err != nil {
		return err
	}

	// Pass 2: backend-gated features.
	if err := refuseBackendGated(backendName, caps, p.Project); err != nil {
		return err
	}

	return nil
}

// refuseUnsupportedFields walks the project and collects every use
// of a §2.2 always-refused compose field. Returns nil if clean.
func refuseUnsupportedFields(proj *composetypes.Project) error {
	var found []UnsupportedField

	// Project-level refusals.
	if len(proj.Secrets) > 0 {
		found = append(found, UnsupportedField{
			Field:  "secrets",
			Reason: "Swarm-only construct; not implemented",
		})
	}
	if len(proj.Configs) > 0 {
		found = append(found, UnsupportedField{
			Field:  "configs",
			Reason: "Swarm-only construct; not implemented",
		})
	}
	// Multiple named networks beyond the default: refused.
	// compose-go always synthesizes a "default" entry; we accept
	// that one and refuse the rest.
	for name := range proj.Networks {
		if name == "default" {
			continue
		}
		found = append(found, UnsupportedField{
			Field:  "networks." + name,
			Reason: "only the project's default network is supported",
		})
	}

	for name, svc := range proj.Services {
		if len(svc.Secrets) > 0 {
			found = append(found, UnsupportedField{
				Service: name, Field: "secrets",
				Reason: "Swarm-only construct; not implemented",
			})
		}
		if len(svc.Configs) > 0 {
			found = append(found, UnsupportedField{
				Service: name, Field: "configs",
				Reason: "Swarm-only construct; not implemented",
			})
		}
		if svc.Deploy != nil {
			found = append(found, UnsupportedField{
				Service: name, Field: "deploy",
				Reason: "Swarm orchestration; not implemented",
			})
		}
		if svc.Develop != nil {
			found = append(found, UnsupportedField{
				Service: name, Field: "develop",
				Reason: "file-sync feature; out of scope for our runtime",
			})
		}
		if len(svc.Links) > 0 {
			found = append(found, UnsupportedField{
				Service: name, Field: "links",
				Reason: "legacy; replaced by network DNS in compose v2",
			})
		}
		if svc.Scale != nil && *svc.Scale > 1 {
			found = append(found, UnsupportedField{
				Service: name, Field: "scale",
				Reason: "multi-replica services not supported",
			})
		}
	}

	if len(found) == 0 {
		return nil
	}
	return &UnsupportedFieldError{Fields: sortFields(found)}
}

// refuseBackendGated checks features whose support flips with
// Capabilities. Returns the first error encountered.
func refuseBackendGated(backendName string, caps runtime.Capabilities, proj *composetypes.Project) error {
	for name, svc := range proj.Services {
		// depends_on conditions
		for _, dep := range svc.DependsOn {
			switch dep.Condition {
			case "service_healthy":
				if !caps.Healthchecks {
					return &UnsupportedFeatureOnBackendError{
						Backend:    backendName,
						Capability: "Healthchecks",
						Service:    name,
						Detail:     "depends_on.condition: service_healthy requires backend healthcheck support",
					}
				}
			case "service_completed_successfully":
				if !caps.ExitCodes {
					return &UnsupportedFeatureOnBackendError{
						Backend:    backendName,
						Capability: "ExitCodes",
						Service:    name,
						Detail:     "depends_on.condition: service_completed_successfully requires backend exit-code surfacing",
					}
				}
			}
		}
		// network_mode: service:<x> / host / none — all require
		// kernel namespace sharing this backend doesn't model.
		if needsNamespaceSharing(svc.NetworkMode) && !caps.NamespaceSharing {
			return &UnsupportedFeatureOnBackendError{
				Backend:    backendName,
				Capability: "NamespaceSharing",
				Service:    name,
				Detail:     fmt.Sprintf("network_mode %q requires kernel namespace sharing this backend lacks", svc.NetworkMode),
			}
		}
		// pid: service:<x> / host
		if needsNamespaceSharing(svc.Pid) && !caps.NamespaceSharing {
			return &UnsupportedFeatureOnBackendError{
				Backend:    backendName,
				Capability: "NamespaceSharing",
				Service:    name,
				Detail:     fmt.Sprintf("pid %q requires kernel namespace sharing this backend lacks", svc.Pid),
			}
		}
		// ipc: service:<x> / host
		if needsNamespaceSharing(svc.Ipc) && !caps.NamespaceSharing {
			return &UnsupportedFeatureOnBackendError{
				Backend:    backendName,
				Capability: "NamespaceSharing",
				Service:    name,
				Detail:     fmt.Sprintf("ipc %q requires kernel namespace sharing this backend lacks", svc.Ipc),
			}
		}
	}

	// Shared volumes: any single named volume mounted into 2+
	// services. Anonymous and bind mounts are not affected.
	if !caps.SharedVolumes {
		if err := refuseSharedVolumes(proj); err != nil {
			return err
		}
	}
	return nil
}

// needsNamespaceSharing returns true when a network/pid/ipc field
// value refers to another container's namespace.
func needsNamespaceSharing(v string) bool {
	switch v {
	case "host", "none":
		return true
	}
	if isServiceNetworkMode(v) {
		return true
	}
	const p = "container:"
	return len(v) > len(p) && v[:len(p)] == p
}

// refuseSharedVolumes returns the first volume mounted by 2+
// services as a VolumeSharedAcrossServicesError. Walks every
// service's `volumes:` field looking for `type: volume` entries
// against the project's top-level `volumes:`.
func refuseSharedVolumes(proj *composetypes.Project) error {
	users := make(map[string]map[string]struct{}) // volume -> set(service)
	for svcName, svc := range proj.Services {
		for _, vol := range svc.Volumes {
			if vol.Type != composetypes.VolumeTypeVolume {
				continue
			}
			// vol.Source is the top-level volume name. Sanity-check
			// it actually maps to one — compose-go normalizes this
			// during Load, so the lookup is just defensive.
			if _, ok := proj.Volumes[vol.Source]; !ok {
				continue
			}
			set, ok := users[vol.Source]
			if !ok {
				set = map[string]struct{}{}
				users[vol.Source] = set
			}
			set[svcName] = struct{}{}
		}
	}
	for volName, set := range users {
		if len(set) < 2 {
			continue
		}
		services := make([]string, 0, len(set))
		for s := range set {
			services = append(services, s)
		}
		sort.Strings(services)
		return &VolumeSharedAcrossServicesError{
			Volume:   volName,
			Services: services,
		}
	}
	return nil
}
