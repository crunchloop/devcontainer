package config

import "encoding/json"

// rawConfig is the direct unmarshal of a single devcontainer.json file.
//
// Polymorphic spec fields stay as json.RawMessage so the merge stage can
// decide how to interpret them (lifecycle commands accept string |
// []string | map; mounts accept CSV string | object; ports accept int |
// "host:container" string; etc.). The merge stage produces the typed
// representation in ResolvedConfig.
//
// This type is internal: callers never see it. Adding a new spec field
// here is the first step before plumbing it through merge and resolve.
type rawConfig struct {
	Name string `json:"name,omitempty"`

	Image string    `json:"image,omitempty"`
	Build *rawBuild `json:"build,omitempty"`

	DockerComposeFile json.RawMessage `json:"dockerComposeFile,omitempty"`
	Service           string          `json:"service,omitempty"`
	RunServices       []string        `json:"runServices,omitempty"`

	WorkspaceFolder string          `json:"workspaceFolder,omitempty"`
	WorkspaceMount  json.RawMessage `json:"workspaceMount,omitempty"`

	Mounts json.RawMessage `json:"mounts,omitempty"`

	ContainerEnv map[string]string `json:"containerEnv,omitempty"`
	RemoteEnv    map[string]string `json:"remoteEnv,omitempty"`

	ContainerUser       string `json:"containerUser,omitempty"`
	RemoteUser          string `json:"remoteUser,omitempty"`
	UpdateRemoteUserUID *bool  `json:"updateRemoteUserUID,omitempty"`
	UserEnvProbe        string `json:"userEnvProbe,omitempty"`

	RunArgs         []string `json:"runArgs,omitempty"`
	Init            *bool    `json:"init,omitempty"`
	Privileged      *bool    `json:"privileged,omitempty"`
	CapAdd          []string `json:"capAdd,omitempty"`
	SecurityOpt     []string `json:"securityOpt,omitempty"`
	OverrideCommand *bool    `json:"overrideCommand,omitempty"`
	ShutdownAction  string   `json:"shutdownAction,omitempty"`

	Features                    map[string]json.RawMessage `json:"features,omitempty"`
	OverrideFeatureInstallOrder []string                   `json:"overrideFeatureInstallOrder,omitempty"`

	InitializeCommand    json.RawMessage `json:"initializeCommand,omitempty"`
	OnCreateCommand      json.RawMessage `json:"onCreateCommand,omitempty"`
	UpdateContentCommand json.RawMessage `json:"updateContentCommand,omitempty"`
	PostCreateCommand    json.RawMessage `json:"postCreateCommand,omitempty"`
	PostStartCommand     json.RawMessage `json:"postStartCommand,omitempty"`
	PostAttachCommand    json.RawMessage `json:"postAttachCommand,omitempty"`
	WaitFor              string          `json:"waitFor,omitempty"`

	ForwardPorts         json.RawMessage          `json:"forwardPorts,omitempty"`
	PortsAttributes      map[string]rawPortAttrs  `json:"portsAttributes,omitempty"`
	OtherPortsAttributes *rawPortAttrs            `json:"otherPortsAttributes,omitempty"`

	// AppPort is the deprecated predecessor of forwardPorts. Spec still
	// accepts it (int | string | array). Surfaced here so merge can warn
	// and translate where reasonable.
	AppPort json.RawMessage `json:"appPort,omitempty"`

	HostRequirements *rawHostRequirements       `json:"hostRequirements,omitempty"`
	Customizations   map[string]json.RawMessage `json:"customizations,omitempty"`
}

type rawBuild struct {
	Dockerfile string            `json:"dockerfile,omitempty"`
	Context    string            `json:"context,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	Target     string            `json:"target,omitempty"`
	CacheFrom  json.RawMessage   `json:"cacheFrom,omitempty"`
	Options    []string          `json:"options,omitempty"`
}

type rawPortAttrs struct {
	Label            string `json:"label,omitempty"`
	Protocol         string `json:"protocol,omitempty"`
	OnAutoForward    string `json:"onAutoForward,omitempty"`
	ElevateIfNeeded  *bool  `json:"elevateIfNeeded,omitempty"`
	RequireLocalPort *bool  `json:"requireLocalPort,omitempty"`
}

type rawHostRequirements struct {
	CPUs    int             `json:"cpus,omitempty"`
	Memory  string          `json:"memory,omitempty"`
	Storage string          `json:"storage,omitempty"`
	GPU     json.RawMessage `json:"gpu,omitempty"`
}
