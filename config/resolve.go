package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ResolveInput is the contextual data needed to produce a ResolvedConfig
// from a parsed devcontainer.json document.
type ResolveInput struct {
	// LocalWorkspaceFolder is the absolute host path containing the project.
	LocalWorkspaceFolder string

	// ConfigPath is the absolute path of the source devcontainer.json.
	// Used as Warning.Source and to resolve relative paths in build/compose.
	ConfigPath string

	// DevcontainerID is the stable workspace id. Callers may use the default
	// scheme via DevcontainerID(local, config) or supply their own.
	DevcontainerID string

	// LocalEnv is the host environment for ${localEnv:*} resolution. Callers
	// typically pass the result of os.Environ() turned into a map.
	LocalEnv map[string]string
}

// ResolveBytes parses the supplied devcontainer.json bytes and produces a
// merged, host-substituted ResolvedConfig. Image-label metadata and feature
// resolution are stubbed in this milestone; see PRD §13 / status.md for
// what still lands in M3.
func ResolveBytes(src []byte, input ResolveInput) (*ResolvedConfig, error) {
	raw, parseWarns, err := parseRaw(src, input.ConfigPath)
	if err != nil {
		return nil, err
	}
	cfg, err := resolveFromRaw(raw, input)
	if err != nil {
		return nil, err
	}
	cfg.Warnings = append(addSource(parseWarns, input.ConfigPath), cfg.Warnings...)
	return cfg, nil
}

func resolveFromRaw(raw *rawConfig, input ResolveInput) (*ResolvedConfig, error) {
	configDir := filepath.Dir(input.ConfigPath)

	out := &ResolvedConfig{
		DevcontainerID:       input.DevcontainerID,
		Name:                 raw.Name,
		LocalWorkspaceFolder: input.LocalWorkspaceFolder,
		ContainerUser:        raw.ContainerUser,
		RemoteUser:           raw.RemoteUser,
		ContainerEnv:         raw.ContainerEnv,
		RemoteEnv:            raw.RemoteEnv,
		RunArgs:              raw.RunArgs,
		CapAdd:               raw.CapAdd,
		SecurityOpt:          raw.SecurityOpt,
		Customizations:       raw.Customizations,
	}

	src, srcWarns, err := determineSource(raw, configDir)
	if err != nil {
		return nil, &ConfigInvalidError{Path: input.ConfigPath, Message: err.Error()}
	}
	out.Source = src
	out.Warnings = append(out.Warnings, srcWarns...)

	// Optional bools and probe are passed through verbatim. Spec defaults
	// (Init=false, Privileged=false, OverrideCommand=true,
	// UpdateRemoteUserUID=false, UserEnvProbe=loginInteractiveShell) are
	// applied by ResolvedConfig.Finalize, which runs after the
	// metadata-merge pipeline so feature/base-image metadata can still
	// contribute values when the user devcontainer.json does not.
	out.UpdateRemoteUserUID = raw.UpdateRemoteUserUID
	out.Init = raw.Init
	out.Privileged = raw.Privileged
	out.OverrideCommand = raw.OverrideCommand
	out.UserEnvProbe = UserEnvProbe(raw.UserEnvProbe)
	out.ShutdownAction = ShutdownAction(raw.ShutdownAction)

	wsMount, err := decodeWorkspaceMount(raw.WorkspaceMount)
	if err != nil {
		return nil, &ConfigInvalidError{Path: input.ConfigPath, Message: err.Error()}
	}
	out.WorkspaceMount = wsMount

	mounts, mountWarns, err := decodeMounts(raw.Mounts)
	if err != nil {
		return nil, &ConfigInvalidError{Path: input.ConfigPath, Message: err.Error()}
	}
	out.Mounts = mounts
	out.Warnings = append(out.Warnings, addSource(mountWarns, input.ConfigPath)...)

	ports, portWarns, err := decodeForwardPorts(raw.ForwardPorts)
	if err != nil {
		return nil, &ConfigInvalidError{Path: input.ConfigPath, Message: err.Error()}
	}
	out.ForwardPorts = ports
	out.Warnings = append(out.Warnings, addSource(portWarns, input.ConfigPath)...)

	if len(raw.AppPort) > 0 {
		out.Warnings = append(out.Warnings, Warning{
			Code:    WarnDeprecatedKey,
			Message: "appPort is deprecated; use forwardPorts",
			Path:    "/appPort",
			Source:  input.ConfigPath,
		})
	}

	if _, isCompose := out.Source.(*ComposeSource); isCompose && len(out.ForwardPorts) > 0 {
		out.Warnings = append(out.Warnings, Warning{
			Code:    WarnComposePortsIgnored,
			Message: "forwardPorts is informational on compose source; declare ports in your compose file's ports: directive",
			Path:    "/forwardPorts",
			Source:  input.ConfigPath,
		})
	}

	out.PortsAttributes = convertPortsAttributes(raw.PortsAttributes)
	out.OtherPortsAttributes = convertPortAttributes(raw.OtherPortsAttributes)
	out.HostRequirements = convertHostRequirements(raw.HostRequirements)

	lifecycle, lcWarns, err := decodeLifecycleCommands(raw)
	if err != nil {
		return nil, &ConfigInvalidError{Path: input.ConfigPath, Message: err.Error()}
	}
	out.Lifecycle = lifecycle
	out.Warnings = append(out.Warnings, addSource(lcWarns, input.ConfigPath)...)

	secretsCmd, err := decodeLifecycleCommand(raw.SecretsCommand)
	if err != nil {
		return nil, &ConfigInvalidError{Path: input.ConfigPath, Message: fmt.Sprintf("/secretsCommand: %v", err)}
	}
	out.SecretsCommand = secretsCmd

	// WaitFor passes through verbatim; the spec default (postCreate, or
	// updateContent if any layer contributes one) is applied by Finalize
	// after the metadata-merge pipeline.
	out.WaitFor = LifecyclePhase(raw.WaitFor)

	containerFolder := raw.WorkspaceFolder
	if containerFolder == "" {
		containerFolder = "/workspaces/" + filepath.Base(input.LocalWorkspaceFolder)
	}
	subCtx := SubstitutionContext{
		LocalWorkspaceFolder: input.LocalWorkspaceFolder,
		DevcontainerID:       input.DevcontainerID,
		LocalEnv:             input.LocalEnv,
	}
	resolvedFolder, folderWarns := ResolveString(containerFolder, subCtx)
	out.Warnings = append(out.Warnings, taggedWarnings(folderWarns, input.ConfigPath, "/workspaceFolder")...)
	subCtx.ContainerWorkspaceFolder = resolvedFolder
	out.ContainerWorkspaceFolder = resolvedFolder

	substituteAll(out, subCtx, input.ConfigPath)

	if len(raw.Features) > 0 {
		feats, fwarns := buildPartialFeatures(raw.Features, raw.OverrideFeatureInstallOrder, input.ConfigPath)
		out.Features = feats
		out.Warnings = append(out.Warnings, fwarns...)
	}

	return out, nil
}

// buildPartialFeatures parses the features map into ResolvedFeature
// entries with Ref, Options, and SourceKind populated. Metadata, Dir,
// ResolvedRef, and AlreadyInstalled remain empty until the engine fetches
// each feature in the build path.
//
// overrideOrder applies overrideFeatureInstallOrder per spec: matching
// entries (by id, ignoring tag/digest) lead the slice in declaration
// order; remaining features follow alphabetically by ref for determinism.
// Full DAG ordering with dependsOn/installsAfter happens after fetch.
func buildPartialFeatures(rawFeatures map[string]json.RawMessage, overrideOrder []string, source string) ([]ResolvedFeature, []Warning) {
	if len(rawFeatures) == 0 {
		return nil, nil
	}

	type entry struct {
		ref     string
		feature ResolvedFeature
	}
	all := make([]entry, 0, len(rawFeatures))
	var warnings []Warning

	for ref, rawOpts := range rawFeatures {
		opts, w := parseFeatureOptions(rawOpts, ref, source)
		warnings = append(warnings, w...)
		all = append(all, entry{
			ref: ref,
			feature: ResolvedFeature{
				Ref:        ref,
				Options:    opts,
				SourceKind: classifyFeatureRef(ref),
			},
		})
	}

	// Sort baseline alphabetically for determinism.
	sort.Slice(all, func(i, j int) bool { return all[i].ref < all[j].ref })

	// Apply overrideFeatureInstallOrder: matching ids first in declaration order.
	out := make([]ResolvedFeature, 0, len(all))
	taken := make(map[string]bool, len(all))
	for _, ord := range overrideOrder {
		for _, e := range all {
			if !taken[e.ref] && featureIDMatch(e.ref, ord) {
				out = append(out, e.feature)
				taken[e.ref] = true
			}
		}
	}
	for _, e := range all {
		if !taken[e.ref] {
			out = append(out, e.feature)
		}
	}
	return out, warnings
}

// parseFeatureOptions decodes the value of a features map entry. Spec
// allows three forms:
//   - object: {"version": "1.2.3", "extras": "all"}
//   - string: "1.2.3" → applied as the "version" option (resolved when
//     metadata is fetched and confirms a version option exists)
//   - empty object / null: no user options
//
// Defaults are applied later (after fetching the feature's metadata).
func parseFeatureOptions(raw json.RawMessage, ref, source string) (map[string]any, []Warning) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Try object first.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj, nil
	}
	// Then string (shorthand).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return map[string]any{"version": s}, nil
	}
	// Then null / empty.
	var n any
	if err := json.Unmarshal(raw, &n); err == nil && n == nil {
		return nil, nil
	}
	return nil, []Warning{{
		Code:    WarnUnknownField,
		Message: fmt.Sprintf("features[%q]: value must be an object, string, or null", ref),
		Path:    "/features/" + ref,
		Source:  source,
	}}
}

// classifyFeatureRef maps a features-map key to its source kind based
// on the ref shape. Matches the spec's rules:
//   - "./..." or "../..." or absolute path → Local
//   - "https://..." → HTTPS
//   - everything else → OCI (the canonical "ghcr.io/..." form)
func classifyFeatureRef(ref string) FeatureSourceKind {
	switch {
	case strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") || strings.HasPrefix(ref, "/"):
		return FeatureSourceLocal
	case strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "http://"):
		return FeatureSourceHTTPS
	default:
		return FeatureSourceOCI
	}
}

// featureIDMatch reports whether a feature ref matches an
// overrideFeatureInstallOrder entry. The spec compares "the Feature id
// (without the semantic version)". For OCI refs we strip everything
// after the last colon (tag) and after any '@' (digest).
func featureIDMatch(ref, override string) bool {
	return featureID(ref) == featureID(override)
}

func featureID(ref string) string {
	// Strip @digest first, then trailing :tag.
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	// For OCI refs, the last ':' separates the tag. Local paths and
	// HTTPS URLs typically don't have trailing :tag, so this is safe.
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		// Don't strip the scheme of an HTTPS ref.
		if !strings.Contains(ref[:i], "://") || strings.LastIndex(ref, "/") > i {
			ref = ref[:i]
		}
	}
	return ref
}

func determineSource(raw *rawConfig, configDir string) (Source, []Warning, error) {
	hasImage := raw.Image != ""
	hasBuild := raw.Build != nil
	hasCompose := len(raw.DockerComposeFile) > 0

	count := 0
	for _, b := range []bool{hasImage, hasBuild, hasCompose} {
		if b {
			count++
		}
	}
	if count == 0 {
		return nil, nil, fmt.Errorf("must specify exactly one of image, build, or dockerComposeFile")
	}
	if count > 1 {
		return nil, nil, fmt.Errorf("must specify exactly one of image, build, or dockerComposeFile")
	}

	switch {
	case hasImage:
		return &ImageSource{Image: raw.Image}, nil, nil
	case hasBuild:
		cacheFrom, _ := decodeStringOrStringArray(raw.Build.CacheFrom)
		ctxPath := raw.Build.Context
		if ctxPath == "" {
			ctxPath = "."
		}
		if !filepath.IsAbs(ctxPath) {
			ctxPath = filepath.Join(configDir, ctxPath)
		}
		return &BuildSource{
			Dockerfile: raw.Build.Dockerfile,
			Context:    ctxPath,
			Args:       raw.Build.Args,
			Target:     raw.Build.Target,
			CacheFrom:  cacheFrom,
		}, nil, nil
	case hasCompose:
		files, err := decodeStringOrStringArray(raw.DockerComposeFile)
		if err != nil {
			return nil, nil, fmt.Errorf("dockerComposeFile: %w", err)
		}
		abs := make([]string, len(files))
		for i, f := range files {
			if filepath.IsAbs(f) {
				abs[i] = f
			} else {
				abs[i] = filepath.Join(configDir, f)
			}
		}
		return &ComposeSource{
			Files:       abs,
			Service:     raw.Service,
			RunServices: raw.RunServices,
		}, nil, nil
	}
	return nil, nil, fmt.Errorf("unreachable")
}

func decodeLifecycleCommands(raw *rawConfig) (LifecycleCommands, []Warning, error) {
	out := LifecycleCommands{}
	pairs := []struct {
		dst  *[]LifecycleCommand
		data json.RawMessage
		path string
	}{
		{&out.Initialize, raw.InitializeCommand, "/initializeCommand"},
		{&out.OnCreate, raw.OnCreateCommand, "/onCreateCommand"},
		{&out.UpdateContent, raw.UpdateContentCommand, "/updateContentCommand"},
		{&out.PostCreate, raw.PostCreateCommand, "/postCreateCommand"},
		{&out.PostStart, raw.PostStartCommand, "/postStartCommand"},
		{&out.PostAttach, raw.PostAttachCommand, "/postAttachCommand"},
	}
	for _, p := range pairs {
		cmd, err := decodeLifecycleCommand(p.data)
		if err != nil {
			return LifecycleCommands{}, nil, fmt.Errorf("%s: %w", p.path, err)
		}
		if cmd.IsEmpty() {
			continue
		}
		*p.dst = []LifecycleCommand{cmd}
	}
	return out, nil, nil
}

func convertPortsAttributes(in map[string]rawPortAttrs) map[string]PortAttributes {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]PortAttributes, len(in))
	for k, v := range in {
		out[k] = *convertPortAttributes(&v)
	}
	return out
}

func convertPortAttributes(in *rawPortAttrs) *PortAttributes {
	if in == nil {
		return nil
	}
	return &PortAttributes{
		Label:            in.Label,
		Protocol:         in.Protocol,
		OnAutoForward:    in.OnAutoForward,
		ElevateIfNeeded:  derefBool(in.ElevateIfNeeded, false),
		RequireLocalPort: derefBool(in.RequireLocalPort, false),
	}
}

func convertHostRequirements(in *rawHostRequirements) *HostRequirements {
	if in == nil {
		return nil
	}
	return &HostRequirements{
		CPUs:    in.CPUs,
		Memory:  in.Memory,
		Storage: in.Storage,
		// GPU is polymorphic (bool | "optional" | object) — defer modeling
		// until we have a real consumer. Field present but always nil here.
	}
}

func derefBool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func addSource(ws []Warning, src string) []Warning {
	if len(ws) == 0 {
		return ws
	}
	out := make([]Warning, len(ws))
	for i, w := range ws {
		w.Source = src
		out[i] = w
	}
	return out
}

func taggedWarnings(ws []Warning, src, path string) []Warning {
	if len(ws) == 0 {
		return ws
	}
	out := make([]Warning, len(ws))
	for i, w := range ws {
		w.Source = src
		w.Path = path
		out[i] = w
	}
	return out
}

// substituteAll walks string-bearing fields of out and applies host-context
// substitution in place. Customizations (json.RawMessage) are NOT walked —
// callers decode their own namespace and run substitution there if they
// want it. ${containerEnv:*} references survive unchanged for the runtime
// to resolve later.
func substituteAll(out *ResolvedConfig, ctx SubstitutionContext, source string) {
	subStr := func(s, path string) string {
		v, ws := ResolveString(s, ctx)
		out.Warnings = append(out.Warnings, taggedWarnings(ws, source, path)...)
		return v
	}
	subSlice := func(in []string, pathPrefix string) []string {
		for i := range in {
			in[i] = subStr(in[i], fmt.Sprintf("%s/%d", pathPrefix, i))
		}
		return in
	}
	subMap := func(in map[string]string, pathPrefix string) {
		for k, v := range in {
			in[k] = subStr(v, fmt.Sprintf("%s/%s", pathPrefix, k))
		}
	}

	out.Name = subStr(out.Name, "/name")
	out.ContainerUser = subStr(out.ContainerUser, "/containerUser")
	out.RemoteUser = subStr(out.RemoteUser, "/remoteUser")

	subMap(out.ContainerEnv, "/containerEnv")
	subMap(out.RemoteEnv, "/remoteEnv")

	out.RunArgs = subSlice(out.RunArgs, "/runArgs")
	out.CapAdd = subSlice(out.CapAdd, "/capAdd")
	out.SecurityOpt = subSlice(out.SecurityOpt, "/securityOpt")

	for i := range out.Mounts {
		out.Mounts[i].Source = subStr(out.Mounts[i].Source, fmt.Sprintf("/mounts/%d/source", i))
		out.Mounts[i].Target = subStr(out.Mounts[i].Target, fmt.Sprintf("/mounts/%d/target", i))
	}
	if out.WorkspaceMount != nil {
		out.WorkspaceMount.Source = subStr(out.WorkspaceMount.Source, "/workspaceMount/source")
		out.WorkspaceMount.Target = subStr(out.WorkspaceMount.Target, "/workspaceMount/target")
	}

	substituteLifecycle(&out.Lifecycle, subStr)
	substituteCommand(&out.SecretsCommand, "/secretsCommand", subStr)

	switch s := out.Source.(type) {
	case *ImageSource:
		s.Image = subStr(s.Image, "/image")
	case *BuildSource:
		s.Dockerfile = subStr(s.Dockerfile, "/build/dockerfile")
		s.Context = subStr(s.Context, "/build/context")
		s.Target = subStr(s.Target, "/build/target")
		subMap(s.Args, "/build/args")
		s.CacheFrom = subSlice(s.CacheFrom, "/build/cacheFrom")
	case *ComposeSource:
		s.Files = subSlice(s.Files, "/dockerComposeFile")
		s.Service = subStr(s.Service, "/service")
		s.RunServices = subSlice(s.RunServices, "/runServices")
	}
}

func substituteLifecycle(lc *LifecycleCommands, sub func(s, path string) string) {
	pairs := []struct {
		cmds *[]LifecycleCommand
		path string
	}{
		{&lc.Initialize, "/initializeCommand"},
		{&lc.OnCreate, "/onCreateCommand"},
		{&lc.UpdateContent, "/updateContentCommand"},
		{&lc.PostCreate, "/postCreateCommand"},
		{&lc.PostStart, "/postStartCommand"},
		{&lc.PostAttach, "/postAttachCommand"},
	}
	for _, p := range pairs {
		for i := range *p.cmds {
			substituteCommand(&(*p.cmds)[i], p.path, sub)
		}
	}
}

func substituteCommand(cmd *LifecycleCommand, path string, sub func(s, p string) string) {
	if cmd.Single != nil {
		if cmd.Single.Shell != "" {
			cmd.Single.Shell = sub(cmd.Single.Shell, path)
		}
		for i := range cmd.Single.Exec {
			cmd.Single.Exec[i] = sub(cmd.Single.Exec[i], fmt.Sprintf("%s/%d", path, i))
		}
	}
	for name, c := range cmd.Parallel {
		if c.Shell != "" {
			c.Shell = sub(c.Shell, fmt.Sprintf("%s/%s", path, name))
		}
		for i := range c.Exec {
			c.Exec[i] = sub(c.Exec[i], fmt.Sprintf("%s/%s/%d", path, name, i))
		}
		cmd.Parallel[name] = c
	}
}
