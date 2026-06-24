package config

// MergeMetadata folds image-metadata layers into cfg following spec
// last-write-wins semantics.
//
// `layers` is the chain in install order:
//
//   - base image label entries (one per feature originally installed,
//     plus the final resolved-config entry from the prior build)
//   - each newly-installed feature's parsed devcontainer-feature.json
//
// The user's devcontainer.json is *already* in cfg by the time this is
// called (Resolve has populated it). Per spec, user-supplied values win
// over feature metadata, and feature metadata wins over base image
// metadata. So:
//
//   - Scalars (string / *bool / typed string aliases): only filled in
//     when cfg has no explicit value (empty string, nil pointer). Within
//     `layers`, later entries beat earlier ones.
//   - Maps (containerEnv, remoteEnv): unioned across all layers, then
//     user values overlaid on top — user wins on key collisions, but
//     base/feature keys the user did not set survive.
//   - String slices (capAdd, securityOpt): unioned with deduplication.
//     Order: base layers first, then features, then user (preserves
//     install order; dedup keeps the first occurrence).
//   - Mounts: appended in chain order; duplicates by Target are removed
//     keeping the LAST occurrence (so user-supplied mount targets win).
//   - Lifecycle hooks: appended per phase in chain order, preserving any
//     user-supplied command at the end. Empty hooks are skipped.
//   - HostRequirements: filled if cfg.HostRequirements is nil; otherwise
//     cfg keeps its value (user wins).
//   - Customizations: not merged here — each tool reads its own namespace
//     and decides how to combine layers. We pass the user's map through
//     unchanged.
//
// `subCtx` supplies values for ${devcontainerId}, ${localWorkspaceFolder},
// ${containerWorkspaceFolder}, ${localEnv:*} placeholders that appear in
// layer-contributed string fields (mount sources, env values, lifecycle
// commands, …). Variables in the user's devcontainer.json are already
// substituted by ResolveBytes; this pass extends the invariant to
// feature- and base-image-contributed strings so they reach the runtime
// without literal ${...} tokens. Pass a zero SubstitutionContext to skip
// substitution (useful for tests with no placeholders).
//
// Idempotent: calling MergeMetadata with the same layers twice produces
// the same result.
func MergeMetadata(cfg *ResolvedConfig, subCtx SubstitutionContext, layers []FeatureMetadata) {
	if cfg == nil || len(layers) == 0 {
		return
	}
	layers = substituteLayers(layers, subCtx)

	cfg.RemoteUser = pickString(cfg.RemoteUser, layers, func(l FeatureMetadata) string { return l.RemoteUser })
	cfg.ContainerUser = pickString(cfg.ContainerUser, layers, func(l FeatureMetadata) string { return l.ContainerUser })
	cfg.UserEnvProbe = UserEnvProbe(pickString(string(cfg.UserEnvProbe), layers, func(l FeatureMetadata) string { return string(l.UserEnvProbe) }))
	cfg.WaitFor = LifecyclePhase(pickString(string(cfg.WaitFor), layers, func(l FeatureMetadata) string { return string(l.WaitFor) }))
	cfg.ShutdownAction = ShutdownAction(pickString(string(cfg.ShutdownAction), layers, func(l FeatureMetadata) string { return string(l.ShutdownAction) }))

	cfg.Init = pickBool(cfg.Init, layers, func(l FeatureMetadata) *bool { return l.Init })
	cfg.Privileged = pickBool(cfg.Privileged, layers, func(l FeatureMetadata) *bool { return l.Privileged })
	cfg.OverrideCommand = pickBool(cfg.OverrideCommand, layers, func(l FeatureMetadata) *bool { return l.OverrideCommand })
	cfg.UpdateRemoteUserUID = pickBool(cfg.UpdateRemoteUserUID, layers, func(l FeatureMetadata) *bool { return l.UpdateRemoteUserUID })

	if cfg.HostRequirements == nil {
		for i := len(layers) - 1; i >= 0; i-- {
			if layers[i].HostRequirements != nil {
				cp := *layers[i].HostRequirements
				cfg.HostRequirements = &cp
				break
			}
		}
	}

	// Slices: union with dedup. Layers contribute first, user (already in
	// cfg) appended last so an explicit user entry stays after dedup.
	cfg.CapAdd = unionDedup(layerStrings(layers, func(l FeatureMetadata) []string { return l.CapAdd }), cfg.CapAdd)
	cfg.SecurityOpt = unionDedup(layerStrings(layers, func(l FeatureMetadata) []string { return l.SecurityOpt }), cfg.SecurityOpt)

	// Entrypoints: accumulate every non-empty layer entrypoint in chain
	// order (base-image label entries first, features last). Unlike the
	// scalar fields these do NOT follow last-wins — every feature that
	// declares an entrypoint must run. Assigned fresh (not appended) so
	// MergeMetadata stays idempotent; devcontainer.json contributes none.
	var entrypoints []string
	for _, l := range layers {
		if l.Entrypoint != "" {
			entrypoints = append(entrypoints, l.Entrypoint)
		}
	}
	cfg.Entrypoints = entrypoints

	// Maps: layered union with user overlay last.
	cfg.ContainerEnv = mergeStringMaps(layerMaps(layers, func(l FeatureMetadata) map[string]string { return l.ContainerEnv }), cfg.ContainerEnv)
	cfg.RemoteEnv = mergeStringMaps(layerMaps(layers, func(l FeatureMetadata) map[string]string { return l.RemoteEnv }), cfg.RemoteEnv)

	// Mounts: chain-append, then dedup by Target keeping the LAST entry
	// so the user's explicit mount wins over a feature/base contribution
	// at the same target.
	mounts := make([]Mount, 0)
	for _, l := range layers {
		mounts = append(mounts, l.Mounts...)
	}
	mounts = append(mounts, cfg.Mounts...)
	cfg.Mounts = dedupMountsByTarget(mounts)

	// Lifecycle hooks: append-per-phase. cfg already has the user's hook
	// (singleton slice from Resolve, possibly empty). Layers go BEFORE
	// the user so base/feature hooks run first, user hook last.
	cfg.Lifecycle.OnCreate = prependHooks(layers, func(l FeatureMetadata) LifecycleCommand { return l.OnCreateCommand }, cfg.Lifecycle.OnCreate)
	cfg.Lifecycle.UpdateContent = prependHooks(layers, func(l FeatureMetadata) LifecycleCommand { return l.UpdateContentCommand }, cfg.Lifecycle.UpdateContent)
	cfg.Lifecycle.PostCreate = prependHooks(layers, func(l FeatureMetadata) LifecycleCommand { return l.PostCreateCommand }, cfg.Lifecycle.PostCreate)
	cfg.Lifecycle.PostStart = prependHooks(layers, func(l FeatureMetadata) LifecycleCommand { return l.PostStartCommand }, cfg.Lifecycle.PostStart)
	cfg.Lifecycle.PostAttach = prependHooks(layers, func(l FeatureMetadata) LifecycleCommand { return l.PostAttachCommand }, cfg.Lifecycle.PostAttach)
}

// substituteLayers returns copies of the input layers with every
// substitution-bearing string field resolved against ctx. Layers are
// often shared (feature metadata is cached) — we never mutate them.
// A zero ctx leaves placeholders intact (ResolveString returns the
// literal when the relevant field is empty).
func substituteLayers(in []FeatureMetadata, ctx SubstitutionContext) []FeatureMetadata {
	out := make([]FeatureMetadata, len(in))
	for i, l := range in {
		out[i] = substituteLayer(l, ctx)
	}
	return out
}

func substituteLayer(l FeatureMetadata, ctx SubstitutionContext) FeatureMetadata {
	sub := func(s string) string {
		if s == "" {
			return s
		}
		v, _ := ResolveString(s, ctx)
		return v
	}

	l.RemoteUser = sub(l.RemoteUser)
	l.ContainerUser = sub(l.ContainerUser)
	l.Entrypoint = sub(l.Entrypoint)

	if len(l.ContainerEnv) > 0 {
		m := make(map[string]string, len(l.ContainerEnv))
		for k, v := range l.ContainerEnv {
			m[k] = sub(v)
		}
		l.ContainerEnv = m
	}
	if len(l.RemoteEnv) > 0 {
		m := make(map[string]string, len(l.RemoteEnv))
		for k, v := range l.RemoteEnv {
			m[k] = sub(v)
		}
		l.RemoteEnv = m
	}
	if len(l.Mounts) > 0 {
		ms := make([]Mount, len(l.Mounts))
		copy(ms, l.Mounts)
		for i := range ms {
			ms[i].Source = sub(ms[i].Source)
			ms[i].Target = sub(ms[i].Target)
		}
		l.Mounts = ms
	}
	if len(l.CapAdd) > 0 {
		s := make([]string, len(l.CapAdd))
		for i, v := range l.CapAdd {
			s[i] = sub(v)
		}
		l.CapAdd = s
	}
	if len(l.SecurityOpt) > 0 {
		s := make([]string, len(l.SecurityOpt))
		for i, v := range l.SecurityOpt {
			s[i] = sub(v)
		}
		l.SecurityOpt = s
	}

	l.OnCreateCommand = substituteLifecycleCommand(l.OnCreateCommand, sub)
	l.UpdateContentCommand = substituteLifecycleCommand(l.UpdateContentCommand, sub)
	l.PostCreateCommand = substituteLifecycleCommand(l.PostCreateCommand, sub)
	l.PostStartCommand = substituteLifecycleCommand(l.PostStartCommand, sub)
	l.PostAttachCommand = substituteLifecycleCommand(l.PostAttachCommand, sub)

	return l
}

func substituteLifecycleCommand(cmd LifecycleCommand, sub func(string) string) LifecycleCommand {
	if cmd.Single != nil {
		c := *cmd.Single
		c.Shell = sub(c.Shell)
		if len(c.Exec) > 0 {
			ex := make([]string, len(c.Exec))
			for i, v := range c.Exec {
				ex[i] = sub(v)
			}
			c.Exec = ex
		}
		cmd.Single = &c
	}
	if len(cmd.Parallel) > 0 {
		p := make(map[string]Command, len(cmd.Parallel))
		for k, c := range cmd.Parallel {
			c.Shell = sub(c.Shell)
			if len(c.Exec) > 0 {
				ex := make([]string, len(c.Exec))
				for i, v := range c.Exec {
					ex[i] = sub(v)
				}
				c.Exec = ex
			}
			p[k] = c
		}
		cmd.Parallel = p
	}
	return cmd
}

// pickString returns userVal when non-empty; otherwise the last non-empty
// value across layers (last-write-wins among layers).
func pickString(userVal string, layers []FeatureMetadata, get func(FeatureMetadata) string) string {
	if userVal != "" {
		return userVal
	}
	out := ""
	for _, l := range layers {
		if v := get(l); v != "" {
			out = v
		}
	}
	return out
}

// pickBool returns userVal when non-nil; otherwise the last non-nil value
// across layers.
func pickBool(userVal *bool, layers []FeatureMetadata, get func(FeatureMetadata) *bool) *bool {
	if userVal != nil {
		return userVal
	}
	var out *bool
	for _, l := range layers {
		if v := get(l); v != nil {
			out = v
		}
	}
	return out
}

func layerStrings(layers []FeatureMetadata, get func(FeatureMetadata) []string) []string {
	var out []string
	for _, l := range layers {
		out = append(out, get(l)...)
	}
	return out
}

func layerMaps(layers []FeatureMetadata, get func(FeatureMetadata) map[string]string) []map[string]string {
	var out []map[string]string
	for _, l := range layers {
		if m := get(l); len(m) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// unionDedup concatenates a, b in order, then removes later duplicates.
// First occurrence wins.
func unionDedup(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// mergeStringMaps returns a fresh map combining layerMaps in order, then
// userMap on top. Returns nil if all inputs are empty (avoid allocating
// empty maps for the common no-metadata, no-user-env case).
func mergeStringMaps(layerMaps []map[string]string, userMap map[string]string) map[string]string {
	total := len(userMap)
	for _, m := range layerMaps {
		total += len(m)
	}
	if total == 0 {
		return nil
	}
	out := make(map[string]string, total)
	for _, m := range layerMaps {
		for k, v := range m {
			out[k] = v
		}
	}
	for k, v := range userMap {
		out[k] = v
	}
	return out
}

// dedupMountsByTarget walks the input forward but keeps the LAST entry
// per target. Implementation: walk backward, keep first-seen-target,
// reverse the result so input order is preserved (modulo the dropped
// duplicates).
func dedupMountsByTarget(in []Mount) []Mount {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	rev := make([]Mount, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		m := in[i]
		// Mounts without a Target (malformed) are kept as-is — we don't
		// have a key to dedup on, and silently dropping them masks bugs.
		if m.Target == "" {
			rev = append(rev, m)
			continue
		}
		if seen[m.Target] {
			continue
		}
		seen[m.Target] = true
		rev = append(rev, m)
	}
	out := make([]Mount, len(rev))
	for i, m := range rev {
		out[len(rev)-1-i] = m
	}
	return out
}

// prependHooks places per-layer hooks before any user-supplied hook(s)
// already in `userHooks`. Empty layer hooks are skipped.
func prependHooks(layers []FeatureMetadata, get func(FeatureMetadata) LifecycleCommand, userHooks []LifecycleCommand) []LifecycleCommand {
	out := make([]LifecycleCommand, 0, len(layers)+len(userHooks))
	for _, l := range layers {
		c := get(l)
		if c.IsEmpty() {
			continue
		}
		out = append(out, c)
	}
	out = append(out, userHooks...)
	if len(out) == 0 {
		return nil
	}
	return out
}
