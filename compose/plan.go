package compose

import (
	"fmt"

	"github.com/crunchloop/devcontainer/runtime"

	composetypes "github.com/compose-spec/compose-go/v2/types"
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

	// AdoptExisting reuses any existing (project, service) container
	// unconditionally — start if stopped, attach if running, never
	// recreate on a config-hash / image-digest mismatch. The resume
	// contract: reattach on-disk state exactly, do not reconcile drift.
	AdoptExisting bool
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

// Validate inspects the Plan against the refused-feature list and the
// backend's Capabilities, returning a typed error on the first refusal
// found. Calls are side-effect-free; safe to invoke before any backend
// interaction.
//
// Validation order:
//  1. Hard refusals (§2.2 fields we never implement): one
//     UnsupportedFieldError listing every offending site.
//  2. depends_on conditions the backend cannot honour, per its
//     Capabilities: one UnsupportedFeatureOnBackendError. Refused
//     here rather than at the gate because neither absence is
//     detectable there — a backend reporting no exit code reports
//     zero, indistinguishable from a clean exit, and a service may
//     inherit its healthcheck from the image, which the compose file
//     cannot see.
func (p *Plan) Validate(caps runtime.Capabilities) error {
	if p == nil || p.Project == nil {
		return fmt.Errorf("compose.Plan.Validate: nil plan or project")
	}
	if err := refuseUnsupportedFields(p.Project); err != nil {
		return err
	}
	return refuseUnsupportedConditions(caps, p.Project)
}

// refuseUnsupportedConditions refuses depends_on conditions the active
// backend cannot honour. Only service_completed_successfully is gated:
// service_healthy is enforced by Orchestrator.waitFor, which can tell
// "no health reported" from "no healthcheck declared" at the gate.
func refuseUnsupportedConditions(caps runtime.Capabilities, proj *composetypes.Project) error {
	for name, svc := range proj.Services {
		for _, dep := range svc.DependsOn {
			switch dep.Condition {
			case "service_healthy":
				if !caps.Healthchecks {
					return &UnsupportedFeatureOnBackendError{
						Capability: "Healthchecks",
						Service:    name,
						Detail:     "depends_on.condition: service_healthy requires backend healthcheck support",
					}
				}
			case "service_completed_successfully":
				if !caps.ExitCodes {
					return &UnsupportedFeatureOnBackendError{
						Capability: "ExitCodes",
						Service:    name,
						Detail:     "depends_on.condition: service_completed_successfully requires backend exit-code surfacing",
					}
				}
			}
		}
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
			found = append(found, deployUnsupported(name, svc.Deploy)...)
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

// deployUnsupported collects refusals for sub-fields of deploy: that
// this orchestrator can't honor. We accept deploy when it only carries
// resources.limits with memory/cpus — that's how compose v3+ users
// express per-service resource limits and it maps cleanly onto
// RunSpec.MemoryBytes / RunSpec.NanoCPUs. Everything else inside
// deploy: (replicas, mode, placement, update_config, rollback_config,
// restart_policy, endpoint_mode, labels, resources.reservations,
// non-memory/cpu limits) is Swarm-flavored and refused with a specific
// reason so the user sees what they need to drop.
func deployUnsupported(service string, d *composetypes.DeployConfig) []UnsupportedField {
	var out []UnsupportedField
	if m := d.Mode; m != "" && m != "replicated" {
		out = append(out, UnsupportedField{
			Service: service, Field: "deploy.mode",
			Reason: "only the implicit single-replica mode is supported",
		})
	}
	if r := d.Replicas; r != nil && *r != 1 {
		out = append(out, UnsupportedField{
			Service: service, Field: "deploy.replicas",
			Reason: "multi-replica services are not supported",
		})
	}
	if len(d.Labels) > 0 {
		out = append(out, UnsupportedField{
			Service: service, Field: "deploy.labels",
			Reason: "use service-level labels instead",
		})
	}
	if d.UpdateConfig != nil {
		out = append(out, UnsupportedField{
			Service: service, Field: "deploy.update_config",
			Reason: "Swarm rolling-update; not implemented",
		})
	}
	if d.RollbackConfig != nil {
		out = append(out, UnsupportedField{
			Service: service, Field: "deploy.rollback_config",
			Reason: "Swarm rolling-update; not implemented",
		})
	}
	if d.RestartPolicy != nil {
		out = append(out, UnsupportedField{
			Service: service, Field: "deploy.restart_policy",
			Reason: "use the top-level restart: field instead",
		})
	}
	if d.EndpointMode != "" {
		out = append(out, UnsupportedField{
			Service: service, Field: "deploy.endpoint_mode",
			Reason: "Swarm load balancing; not implemented",
		})
	}
	if len(d.Placement.Constraints) > 0 || len(d.Placement.Preferences) > 0 || d.Placement.MaxReplicas != 0 {
		out = append(out, UnsupportedField{
			Service: service, Field: "deploy.placement",
			Reason: "Swarm scheduling; not implemented",
		})
	}
	out = append(out, resourcesUnsupported(service, d.Resources)...)
	return out
}

// resourcesUnsupported refuses anything inside deploy.resources beyond
// limits.memory and limits.cpus. Reservations are silently dropped on
// our runtimes today (docker honors them but we don't currently
// translate them), so refusing them surfaces the silent loss to the
// user.
func resourcesUnsupported(service string, r composetypes.Resources) []UnsupportedField {
	var out []UnsupportedField
	if r.Reservations != nil {
		out = append(out, UnsupportedField{
			Service: service, Field: "deploy.resources.reservations",
			Reason: "soft-limit reservations are not honored on this runtime",
		})
	}
	if r.Limits != nil {
		if r.Limits.Pids != 0 {
			out = append(out, UnsupportedField{
				Service: service, Field: "deploy.resources.limits.pids",
				Reason: "pids limit is not implemented",
			})
		}
		if len(r.Limits.Devices) > 0 {
			out = append(out, UnsupportedField{
				Service: service, Field: "deploy.resources.limits.devices",
				Reason: "device requests are not implemented",
			})
		}
		if len(r.Limits.GenericResources) > 0 {
			out = append(out, UnsupportedField{
				Service: service, Field: "deploy.resources.limits.generic_resources",
				Reason: "generic resources are not implemented",
			})
		}
	}
	return out
}
