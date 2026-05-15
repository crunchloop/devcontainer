package compose

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel for compose-source projects against backends that don't
// satisfy the runtime.Runtime compose primitives. Engine.Up checks
// this and surfaces it to the user with a clear "this backend
// doesn't support compose source" message.
var ErrComposeUnsupportedOnBackend = errors.New("compose: backend does not support compose source")

// UnsupportedFieldError is returned by Plan.Validate when the user's
// compose project uses fields the orchestrator does not implement.
// Lists every offending (service, field) pair so the user can fix
// them in one pass rather than discovering them one at a time.
//
// See design/compose-native.md §2.2 for the refused-field list.
type UnsupportedFieldError struct {
	// Fields lists the unsupported usage sites. Sorted for stable
	// error output.
	Fields []UnsupportedField
}

// UnsupportedField names one field usage that the orchestrator
// rejects. Service may be empty for project-level fields.
type UnsupportedField struct {
	Service string // "" for project-level (top-level secrets:, configs:, …)
	Field   string // canonical name, e.g. "secrets", "services.<x>.deploy"
	Reason  string // human-readable explanation (one short sentence)
}

func (e *UnsupportedFieldError) Error() string {
	if len(e.Fields) == 0 {
		return "compose: unsupported field (no detail)"
	}
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		switch {
		case f.Service == "":
			parts = append(parts, fmt.Sprintf("%s: %s", f.Field, f.Reason))
		default:
			parts = append(parts, fmt.Sprintf("service %q: %s: %s", f.Service, f.Field, f.Reason))
		}
	}
	return "compose: unsupported field(s): " + strings.Join(parts, "; ")
}

// sortFields returns a stable order for assert-friendly tests and
// readable error messages.
func sortFields(in []UnsupportedField) []UnsupportedField {
	out := append([]UnsupportedField(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Field < out[j].Field
	})
	return out
}

// UnsupportedFeatureOnBackendError is returned by Plan.Validate when
// the project uses a compose feature the active backend cannot
// satisfy — e.g. depends_on.condition: service_healthy against a
// backend whose Capabilities().Healthchecks is false.
//
// Distinct from UnsupportedFieldError (which lists fields we never
// implement) because the gating is backend-specific and may flip if
// the backend gains the capability later.
type UnsupportedFeatureOnBackendError struct {
	Backend    string // backend display name (e.g. "applecontainer")
	Capability string // Capabilities struct field name (e.g. "Healthchecks")
	Service    string // service that triggered the refusal
	Detail     string // one-sentence explanation
}

func (e *UnsupportedFeatureOnBackendError) Error() string {
	if e.Service != "" {
		return fmt.Sprintf(
			"compose: service %q uses %s, which the %s backend does not support: %s",
			e.Service, e.Capability, e.Backend, e.Detail,
		)
	}
	return fmt.Sprintf(
		"compose: project uses %s, which the %s backend does not support: %s",
		e.Capability, e.Backend, e.Detail,
	)
}

// VolumeSharedAcrossServicesError is returned by Plan.Validate when
// the project mounts a single named volume into 2+ services and the
// active backend's Capabilities().SharedVolumes is false (today:
// applecontainer, due to ext4-on-disk-image multi-attach
// restrictions per design probe 4).
type VolumeSharedAcrossServicesError struct {
	Volume   string
	Services []string // sorted
}

func (e *VolumeSharedAcrossServicesError) Error() string {
	return fmt.Sprintf(
		"compose: volume %q is mounted into %d services (%s); the active backend does not allow shared volumes",
		e.Volume, len(e.Services), strings.Join(e.Services, ", "),
	)
}

// PartialUpError signals that Up brought some services online and
// then failed before completing. Returned with the names of the
// services that did and didn't start so the caller (Engine.Up) can
// decide whether to retry or call Down.
//
// Per design §5.3, the orchestrator does NOT auto-rollback: the
// running services stay running so the user can exec into them
// and read logs.
type PartialUpError struct {
	Started []string // service names whose containers are running
	Failed  string   // service name that hit the error
	Err     error    // underlying failure
}

func (e *PartialUpError) Error() string {
	return fmt.Sprintf(
		"compose: partial Up — %d service(s) started [%s] but %q failed: %v",
		len(e.Started), strings.Join(e.Started, ","), e.Failed, e.Err,
	)
}

func (e *PartialUpError) Unwrap() error { return e.Err }

// HealthTimeoutError is returned by Up when a depends_on condition
// (service_healthy or service_completed_successfully) does not
// resolve within the configured timeout. The dependents of the
// timed-out service are not started; the started prerequisites are
// left running.
type HealthTimeoutError struct {
	Service   string
	Condition string // "service_healthy" or "service_completed_successfully"
	Waited    string // human-readable duration ("60s")
}

func (e *HealthTimeoutError) Error() string {
	return fmt.Sprintf(
		"compose: service %q did not satisfy %s within %s",
		e.Service, e.Condition, e.Waited,
	)
}

// CycleError is returned by topological sort when the depends_on
// graph contains a cycle. Cycle lists the services on the cycle,
// in the order they appear.
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("compose: depends_on cycle: %s", strings.Join(e.Cycle, " -> "))
}
