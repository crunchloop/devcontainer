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
// devcontainer.metadata image label. The shape mirrors the merge-eligible
// surface of devcontainer.json + devcontainer-feature.json so that any
// layer (base image, feature, or prior resolved config) can contribute
// to any field.
//
// Fields that need format-tolerant decoding (lifecycle commands, mounts)
// stay as json.RawMessage and are decoded via the shared helpers in the
// config package.
type metadataEntry struct {
	ID          string `json:"id,omitempty"`
	Version     string `json:"version,omitempty"`
	ResolvedRef string `json:"resolvedRef,omitempty"`

	// Mergeable surface.
	RemoteUser           string            `json:"remoteUser,omitempty"`
	ContainerUser        string            `json:"containerUser,omitempty"`
	UserEnvProbe         string            `json:"userEnvProbe,omitempty"`
	WaitFor              string            `json:"waitFor,omitempty"`
	ShutdownAction       string            `json:"shutdownAction,omitempty"`
	UpdateRemoteUserUID  *bool             `json:"updateRemoteUserUID,omitempty"`
	ContainerEnv         map[string]string `json:"containerEnv,omitempty"`
	RemoteEnv            map[string]string `json:"remoteEnv,omitempty"`
	Init                 *bool             `json:"init,omitempty"`
	Privileged           *bool             `json:"privileged,omitempty"`
	OverrideCommand      *bool             `json:"overrideCommand,omitempty"`
	CapAdd               []string          `json:"capAdd,omitempty"`
	SecurityOpt          []string          `json:"securityOpt,omitempty"`
	Entrypoint           string            `json:"entrypoint,omitempty"`
	Mounts               json.RawMessage   `json:"mounts,omitempty"`
	HostRequirements     *rawHostReq       `json:"hostRequirements,omitempty"`
	OnCreateCommand      json.RawMessage   `json:"onCreateCommand,omitempty"`
	UpdateContentCommand json.RawMessage   `json:"updateContentCommand,omitempty"`
	PostCreateCommand    json.RawMessage   `json:"postCreateCommand,omitempty"`
	PostStartCommand     json.RawMessage   `json:"postStartCommand,omitempty"`
	PostAttachCommand    json.RawMessage   `json:"postAttachCommand,omitempty"`

	Customizations map[string]json.RawMessage `json:"customizations,omitempty"`
}

type rawHostReq struct {
	CPUs    int    `json:"cpus,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Storage string `json:"storage,omitempty"`
}

// buildMetadataLabel returns the JSON array for the devcontainer.metadata
// image label given a build plan. Order: prior label entries from the
// base image (carried through verbatim), one entry per newly-installed
// feature in install order, followed by a final entry for the resolved
// devcontainer config.
//
// AlreadyInstalled features are not duplicated — their entries are
// already present in plan.BaseImageMetadata.
//
// The carry-through copies every mergeable field from each base entry,
// not a subset: rebuilds must be lossless so a future read of the label
// recovers the same merged config.
func buildMetadataLabel(plan BuildPlan) ([]byte, error) {
	entries := make([]metadataEntry, 0, len(plan.BaseImageMetadata)+len(plan.Features)+1)

	for _, b := range plan.BaseImageMetadata {
		entries = append(entries, metadataFromFeatureMetadata(b))
	}

	for _, f := range plan.Features {
		if f.AlreadyInstalled {
			continue
		}
		e := metadataFromFeatureMetadata(f.Metadata)
		e.ResolvedRef = f.ResolvedRef
		entries = append(entries, e)
	}

	// Final entry from the resolved config. Carries the user's mergeable
	// overrides so downstream consumers (other devcontainer-aware tools
	// reading the label) see the complete resolved state.
	final := metadataEntry{
		RemoteUser:    plan.RemoteUser,
		ContainerUser: plan.ContainerUser,
	}
	entries = append(entries, final)
	return json.Marshal(entries)
}

func metadataFromFeatureMetadata(m config.FeatureMetadata) metadataEntry {
	out := metadataEntry{
		ID:                  m.ID,
		Version:             m.Version,
		RemoteUser:          m.RemoteUser,
		ContainerUser:       m.ContainerUser,
		UserEnvProbe:        string(m.UserEnvProbe),
		WaitFor:             string(m.WaitFor),
		ShutdownAction:      string(m.ShutdownAction),
		UpdateRemoteUserUID: m.UpdateRemoteUserUID,
		ContainerEnv:        m.ContainerEnv,
		RemoteEnv:           m.RemoteEnv,
		Init:                m.Init,
		Privileged:          m.Privileged,
		OverrideCommand:     m.OverrideCommand,
		CapAdd:              m.CapAdd,
		SecurityOpt:         m.SecurityOpt,
		Entrypoint:          m.Entrypoint,
		Customizations:      m.Customizations,
	}
	if m.HostRequirements != nil {
		out.HostRequirements = &rawHostReq{
			CPUs:    m.HostRequirements.CPUs,
			Memory:  m.HostRequirements.Memory,
			Storage: m.HostRequirements.Storage,
		}
	}
	if len(m.Mounts) > 0 {
		out.Mounts = encodeMounts(m.Mounts)
	}
	out.OnCreateCommand = encodeLifecycleCommand(m.OnCreateCommand)
	out.UpdateContentCommand = encodeLifecycleCommand(m.UpdateContentCommand)
	out.PostCreateCommand = encodeLifecycleCommand(m.PostCreateCommand)
	out.PostStartCommand = encodeLifecycleCommand(m.PostStartCommand)
	out.PostAttachCommand = encodeLifecycleCommand(m.PostAttachCommand)
	return out
}

// encodeMounts produces the array-of-objects form. Spec also accepts CSV
// strings on input, but we always emit objects for clarity.
func encodeMounts(mounts []config.Mount) json.RawMessage {
	type mountObj struct {
		Type     string `json:"type,omitempty"`
		Source   string `json:"source,omitempty"`
		Target   string `json:"target,omitempty"`
		ReadOnly bool   `json:"readonly,omitempty"`
	}
	objs := make([]mountObj, len(mounts))
	for i, m := range mounts {
		objs[i] = mountObj{
			Type:     string(m.Type),
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		}
	}
	b, _ := json.Marshal(objs)
	return b
}

// encodeLifecycleCommand emits the canonical form. Single-shell → string;
// single-exec → array; parallel → object. Empty → omitted (nil).
func encodeLifecycleCommand(c config.LifecycleCommand) json.RawMessage {
	if c.Single != nil {
		if c.Single.Shell != "" {
			b, _ := json.Marshal(c.Single.Shell)
			return b
		}
		if len(c.Single.Exec) > 0 {
			b, _ := json.Marshal(c.Single.Exec)
			return b
		}
	}
	if len(c.Parallel) > 0 {
		obj := make(map[string]json.RawMessage, len(c.Parallel))
		for name, cmd := range c.Parallel {
			if cmd.Shell != "" {
				b, _ := json.Marshal(cmd.Shell)
				obj[name] = b
			} else {
				b, _ := json.Marshal(cmd.Exec)
				obj[name] = b
			}
		}
		b, _ := json.Marshal(obj)
		return b
	}
	return nil
}

// ParseMetadataLabel parses the JSON array stored at the
// devcontainer.metadata image label. Returns (nil, nil) for an empty
// or missing label. Unparseable labels return an error so callers can
// surface diagnostics; callers may treat the error as "no label" if
// they want to be permissive.
//
// All entries are returned, including the no-ID "resolved config" entry
// written by a previous build — it carries the user's mergeable
// overrides and must not be dropped or those overrides are lost on
// rebuild.
func ParseMetadataLabel(label string) ([]config.FeatureMetadata, error) {
	if label == "" {
		return nil, nil
	}
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
		fm, err := e.toFeatureMetadata()
		if err != nil {
			return nil, err
		}
		out = append(out, fm)
	}
	return out, nil
}

func (e metadataEntry) toFeatureMetadata() (config.FeatureMetadata, error) {
	out := config.FeatureMetadata{
		ID:                  e.ID,
		Version:             e.Version,
		RemoteUser:          e.RemoteUser,
		ContainerUser:       e.ContainerUser,
		UserEnvProbe:        config.UserEnvProbe(e.UserEnvProbe),
		WaitFor:             config.LifecyclePhase(e.WaitFor),
		ShutdownAction:      config.ShutdownAction(e.ShutdownAction),
		UpdateRemoteUserUID: e.UpdateRemoteUserUID,
		ContainerEnv:        e.ContainerEnv,
		RemoteEnv:           e.RemoteEnv,
		Init:                e.Init,
		Privileged:          e.Privileged,
		OverrideCommand:     e.OverrideCommand,
		CapAdd:              e.CapAdd,
		SecurityOpt:         e.SecurityOpt,
		Entrypoint:          e.Entrypoint,
		Customizations:      e.Customizations,
	}
	if e.HostRequirements != nil {
		out.HostRequirements = &config.HostRequirements{
			CPUs:    e.HostRequirements.CPUs,
			Memory:  e.HostRequirements.Memory,
			Storage: e.HostRequirements.Storage,
		}
	}
	if len(e.Mounts) > 0 {
		mounts, _, err := config.DecodeMounts(e.Mounts)
		if err != nil {
			return config.FeatureMetadata{}, err
		}
		out.Mounts = mounts
	}
	if cmd, err := config.DecodeLifecycleCommand(e.OnCreateCommand); err != nil {
		return config.FeatureMetadata{}, err
	} else {
		out.OnCreateCommand = cmd
	}
	if cmd, err := config.DecodeLifecycleCommand(e.UpdateContentCommand); err != nil {
		return config.FeatureMetadata{}, err
	} else {
		out.UpdateContentCommand = cmd
	}
	if cmd, err := config.DecodeLifecycleCommand(e.PostCreateCommand); err != nil {
		return config.FeatureMetadata{}, err
	} else {
		out.PostCreateCommand = cmd
	}
	if cmd, err := config.DecodeLifecycleCommand(e.PostStartCommand); err != nil {
		return config.FeatureMetadata{}, err
	} else {
		out.PostStartCommand = cmd
	}
	if cmd, err := config.DecodeLifecycleCommand(e.PostAttachCommand); err != nil {
		return config.FeatureMetadata{}, err
	} else {
		out.PostAttachCommand = cmd
	}
	return out, nil
}
