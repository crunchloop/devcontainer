package feature

import (
	"encoding/json"

	"github.com/crunchloop/devcontainer/config"
)

// MetadataLabel is the image-label key that carries the accumulated
// devcontainer metadata across base image + feature layers + final
// resolved config. Spec-defined.
const MetadataLabel = "devcontainer.metadata"

// metadataEntry is one element of the JSON array stored at the
// devcontainer.metadata image label. The shape is intentionally
// permissive — feature metadata varies in what it declares, and the
// label is consumed by ourselves and possibly other devcontainer-aware
// tools downstream.
type metadataEntry struct {
	ID               string                     `json:"id,omitempty"`
	Version          string                     `json:"version,omitempty"`
	ResolvedRef      string                     `json:"resolvedRef,omitempty"`
	ContainerEnv     map[string]string          `json:"containerEnv,omitempty"`
	Init             *bool                      `json:"init,omitempty"`
	Privileged       *bool                      `json:"privileged,omitempty"`
	CapAdd           []string                   `json:"capAdd,omitempty"`
	SecurityOpt      []string                   `json:"securityOpt,omitempty"`
	Entrypoint       string                     `json:"entrypoint,omitempty"`
	RemoteUser       string                     `json:"remoteUser,omitempty"`
	ContainerUser    string                     `json:"containerUser,omitempty"`
	Customizations   map[string]json.RawMessage `json:"customizations,omitempty"`
}

// buildMetadataLabel returns the JSON array for the devcontainer.metadata
// image label given a build plan. Order: prior label entries from the
// base image (carried through), one entry per newly-installed feature
// in install order, followed by a final entry for the resolved
// devcontainer config.
//
// AlreadyInstalled features are not duplicated — their entries are
// already present in plan.BaseImageMetadata.
func buildMetadataLabel(plan BuildPlan) ([]byte, error) {
	entries := make([]metadataEntry, 0, len(plan.BaseImageMetadata)+len(plan.Features)+1)

	// Base image label entries first, so they remain in their original
	// install order even after rebuilds layer more features on top.
	for _, b := range plan.BaseImageMetadata {
		entries = append(entries, metadataEntry{
			ID:           b.ID,
			Version:      b.Version,
			ContainerEnv: b.ContainerEnv,
			Init:         b.Init,
			Privileged:   b.Privileged,
			CapAdd:       b.CapAdd,
			SecurityOpt:  b.SecurityOpt,
			Entrypoint:   b.Entrypoint,
		})
	}

	for _, f := range plan.Features {
		if f.AlreadyInstalled {
			// Already in the base image's label — don't duplicate.
			continue
		}
		entries = append(entries, metadataEntry{
			ID:             f.Metadata.ID,
			Version:        f.Metadata.Version,
			ResolvedRef:    f.ResolvedRef,
			ContainerEnv:   f.Metadata.ContainerEnv,
			Init:           f.Metadata.Init,
			Privileged:     f.Metadata.Privileged,
			CapAdd:         f.Metadata.CapAdd,
			SecurityOpt:    f.Metadata.SecurityOpt,
			Entrypoint:     f.Metadata.Entrypoint,
			Customizations: f.Metadata.Customizations,
		})
	}
	// Final entry from the resolved config (lightweight subset).
	entries = append(entries, metadataEntry{
		RemoteUser:    plan.RemoteUser,
		ContainerUser: plan.ContainerUser,
	})
	return json.Marshal(entries)
}

// ParseMetadataLabel parses the JSON array stored at the
// devcontainer.metadata image label. Returns (nil, nil) for an empty
// or missing label. Unparseable labels return an error so callers can
// surface diagnostics; callers may treat the error as "no label" if
// they want to be permissive (PR9 will).
func ParseMetadataLabel(label string) ([]config.FeatureMetadata, error) {
	if label == "" {
		return nil, nil
	}
	// Accept either array form (canonical) or single-object (legacy).
	if label[0] == '{' {
		var single struct{ ID, Version, ResolvedRef string }
		if err := json.Unmarshal([]byte(label), &single); err != nil {
			return nil, err
		}
		return []config.FeatureMetadata{{ID: single.ID, Version: single.Version}}, nil
	}
	var entries []metadataEntry
	if err := json.Unmarshal([]byte(label), &entries); err != nil {
		return nil, err
	}
	out := make([]config.FeatureMetadata, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue // skip the final "resolved config" entry which has no ID
		}
		out = append(out, config.FeatureMetadata{
			ID:           e.ID,
			Version:      e.Version,
			ContainerEnv: e.ContainerEnv,
			Init:         e.Init,
			Privileged:   e.Privileged,
			CapAdd:       e.CapAdd,
			SecurityOpt:  e.SecurityOpt,
			Entrypoint:   e.Entrypoint,
		})
	}
	return out, nil
}
