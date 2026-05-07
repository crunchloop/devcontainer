package config

import "encoding/json"

// ResolvedConfig is the merged and host-substituted devcontainer
// configuration produced by Resolve. Treat as read-only after Resolve
// returns; mutate only via DefensiveCopy.
//
// String fields may still contain unresolved ${containerEnv:*} placeholders;
// see HasPendingSubstitutions.
type ResolvedConfig struct {
	DevcontainerID string

	Source Source

	Name                     string
	LocalWorkspaceFolder     string
	ContainerWorkspaceFolder string
	WorkspaceMount           *Mount

	ContainerUser       string
	RemoteUser          string
	UpdateRemoteUserUID bool
	UserEnvProbe        UserEnvProbe

	ContainerEnv map[string]string
	RemoteEnv    map[string]string

	Mounts          []Mount
	RunArgs         []string
	Init            bool
	Privileged      bool
	CapAdd          []string
	SecurityOpt     []string
	OverrideCommand bool
	ShutdownAction  ShutdownAction

	Features []ResolvedFeature

	Lifecycle LifecycleCommands
	WaitFor   LifecyclePhase

	ForwardPorts         []PortSpec
	PortsAttributes      map[string]PortAttributes
	OtherPortsAttributes *PortAttributes

	HostRequirements *HostRequirements

	Customizations map[string]json.RawMessage

	Warnings []Warning
}

// Source identifies where the container comes from. Implementations are
// sealed: ImageSource, BuildSource, ComposeSource.
type Source interface {
	isSource()
	Kind() SourceKind
}

type SourceKind string

const (
	SourceImage   SourceKind = "image"
	SourceBuild   SourceKind = "build"
	SourceCompose SourceKind = "compose"
)

type ImageSource struct {
	Image string
}

func (*ImageSource) isSource()        {}
func (*ImageSource) Kind() SourceKind { return SourceImage }

type BuildSource struct {
	Dockerfile string
	Context    string
	Args       map[string]string
	Target     string
	CacheFrom  []string
	Options    map[string]string
}

func (*BuildSource) isSource()        {}
func (*BuildSource) Kind() SourceKind { return SourceBuild }

type ComposeSource struct {
	Files       []string
	Service     string
	RunServices []string
}

func (*ComposeSource) isSource()        {}
func (*ComposeSource) Kind() SourceKind { return SourceCompose }

// LifecyclePhase names the spec lifecycle phases plus initialize.
type LifecyclePhase string

const (
	LifecycleInitialize    LifecyclePhase = "initialize"
	LifecycleOnCreate      LifecyclePhase = "onCreate"
	LifecycleUpdateContent LifecyclePhase = "updateContent"
	LifecyclePostCreate    LifecyclePhase = "postCreate"
	LifecyclePostStart     LifecyclePhase = "postStart"
	LifecyclePostAttach    LifecyclePhase = "postAttach"
)

// LifecycleCommands holds the resolved command for each phase.
type LifecycleCommands struct {
	Initialize    LifecycleCommand
	OnCreate      LifecycleCommand
	UpdateContent LifecycleCommand
	PostCreate    LifecycleCommand
	PostStart     LifecycleCommand
	PostAttach    LifecycleCommand
}

// LifecycleCommand is one phase's command, in either single or parallel-named
// form. At most one of Single / Parallel is populated; both empty means the
// phase is unconfigured and must be skipped.
type LifecycleCommand struct {
	Single   *Command
	Parallel map[string]Command
}

func (l LifecycleCommand) IsEmpty() bool {
	return l.Single == nil && len(l.Parallel) == 0
}

// Command is a single executable invocation in either shell or exec form.
// Exactly one of Shell / Exec is populated.
type Command struct {
	Shell string
	Exec  []string
}

func (c Command) IsEmpty() bool {
	return c.Shell == "" && len(c.Exec) == 0
}

// ResolvedFeature is a feature reference plus user-supplied options. It
// is progressively populated through the pipeline:
//
//   - After config.Resolve: Ref, Options (raw user input), SourceKind
//     are populated. ResolvedRef, Dir, Metadata are empty;
//     AlreadyInstalled is false.
//   - After feature.Order on the partial set: position in the slice
//     reflects overrideFeatureInstallOrder if the user supplied it.
//   - After Engine.Up reads the base image's devcontainer.metadata
//     label: AlreadyInstalled may flip true; in that case fetch is
//     skipped and Dir/Metadata stay empty.
//   - After feature.Store.Fetch: Dir, ResolvedRef, Metadata populated.
//   - After feature.Order on the full set: position reflects the
//     dependsOn / installsAfter DAG.
type ResolvedFeature struct {
	Ref              string
	ResolvedRef      string
	Dir              string
	Metadata         FeatureMetadata
	Options          map[string]any
	SourceKind       FeatureSourceKind
	AlreadyInstalled bool
}

type FeatureSourceKind string

const (
	FeatureSourceOCI   FeatureSourceKind = "oci"
	FeatureSourceHTTPS FeatureSourceKind = "https"
	FeatureSourceLocal FeatureSourceKind = "local"
)

// FeatureMetadata is the parsed devcontainer-feature.json contributed by a
// feature. Only fields we either consume or surface are modeled.
type FeatureMetadata struct {
	ID               string
	Version          string
	Name             string
	Description      string
	DocumentationURL string
	LicenseURL       string

	Options map[string]FeatureOption

	ContainerEnv map[string]string
	Mounts       []Mount
	Init         *bool
	Privileged   *bool
	CapAdd       []string
	SecurityOpt  []string
	Entrypoint   string

	InstallsAfter []string
	DependsOn     map[string]map[string]any

	OnCreateCommand      LifecycleCommand
	UpdateContentCommand LifecycleCommand
	PostCreateCommand    LifecycleCommand
	PostStartCommand     LifecycleCommand
	PostAttachCommand    LifecycleCommand

	Customizations map[string]json.RawMessage
}

// FeatureOption is one option declared by a feature, used to apply defaults
// and validate user-supplied values.
type FeatureOption struct {
	Type        string // "string" | "boolean"
	Default     any
	Enum        []any
	Proposals   []any
	Description string
}

// MountType is the docker mount type.
type MountType string

const (
	MountBind   MountType = "bind"
	MountVolume MountType = "volume"
	MountTmpfs  MountType = "tmpfs"
)

// Mount is the normalized object form of a mount entry. Spec CSV strings are
// parsed into this shape during merge.
type Mount struct {
	Type     MountType
	Source   string
	Target   string
	ReadOnly bool

	BindOptions   *BindOptions
	VolumeOptions *VolumeOptions
	TmpfsOptions  *TmpfsOptions
}

type BindOptions struct {
	Propagation    string
	CreateHostPath bool
}

type VolumeOptions struct {
	NoCopy       bool
	Labels       map[string]string
	DriverConfig *VolumeDriverConfig
}

type VolumeDriverConfig struct {
	Name    string
	Options map[string]string
}

type TmpfsOptions struct {
	SizeBytes int64
	Mode      uint32
}

// UserEnvProbe selects the shell strategy for capturing the user's
// interactive environment when computing remoteEnv.
type UserEnvProbe string

const (
	UserEnvProbeNone             UserEnvProbe = "none"
	UserEnvProbeLoginShell       UserEnvProbe = "loginShell"
	UserEnvProbeInteractiveShell UserEnvProbe = "interactiveShell"
	UserEnvProbeLoginInteractive UserEnvProbe = "loginInteractiveShell"
)

// ShutdownAction selects how the container is treated when the workspace is
// closed.
type ShutdownAction string

const (
	ShutdownNone          ShutdownAction = "none"
	ShutdownStop          ShutdownAction = "stop"
	ShutdownStopContainer ShutdownAction = "stopContainer"
	ShutdownStopCompose   ShutdownAction = "stopCompose"
)

// PortSpec describes a forwarded port. Host == 0 means "any free host port".
type PortSpec struct {
	Container int
	Host      int
	Label     string
}

type PortAttributes struct {
	Label            string
	Protocol         string
	OnAutoForward    string
	ElevateIfNeeded  bool
	RequireLocalPort bool
}

// HostRequirements is parsed and surfaced; v1 does not enforce.
type HostRequirements struct {
	CPUs    int
	Memory  string
	Storage string
	GPU     *GPURequirement
}

type GPURequirement struct {
	Optional bool
}

// WarningCode classifies a non-fatal diagnostic.
type WarningCode string

const (
	WarnUnknownField            WarningCode = "unknown_field"
	WarnDeprecatedKey           WarningCode = "deprecated_key"
	WarnUnsupportedFeatureField WarningCode = "unsupported_feature_field"
	WarnUnresolvedLocalEnv      WarningCode = "unresolved_local_env"
	WarnUnresolvedContainerEnv  WarningCode = "unresolved_container_env"
	WarnUnknownVariable         WarningCode = "unknown_variable"
	WarnDeepFeatureChain        WarningCode = "deep_feature_chain"
	WarnUnknownFeatureOption    WarningCode = "unknown_feature_option"
	WarnComposePortsIgnored     WarningCode = "compose_ports_ignored"
)

// Warning is a non-fatal diagnostic accumulated during parse, merge, or
// substitution. Path is a JSON pointer; Source identifies the contributing
// document (file path, image label reference, or feature id).
type Warning struct {
	Code    WarningCode
	Message string
	Path    string
	Source  string
}
