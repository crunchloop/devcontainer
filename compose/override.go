package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// Override describes the engine-side additions we layer onto the
// user's compose project for the primary service. Each field is
// optional (zero value is "no change for that aspect").
type Override struct {
	// Service is the name of the primary service to override; matches
	// devcontainer.json's "service" field.
	Service string

	// Image, when set, replaces the primary service's image and
	// clears its build directive (mutually exclusive in compose).
	// Engine sets this to the feature-extended image tag built
	// upstream.
	Image string

	// ExtraBindMounts are added to the primary service's volumes.
	// Engine adds the workspace folder bind here, plus the bind
	// mounts declared in devcontainer.json and feature metadata
	// (cfg.Mounts). Other mount types go in ExtraMounts.
	//
	// IMPORTANT: compose v2's default merge strategy for sequence
	// fields is REPLACEMENT, not concatenation. WriteRunOverride
	// reads the existing service's volumes from the loaded project
	// and emits the full merged list so we don't drop the user's
	// declarations.
	ExtraBindMounts []BindMount

	// ExtraMounts are the non-bind mounts — named or anonymous volumes
	// and tmpfs — added to the primary service's volumes after
	// ExtraBindMounts. Engine fills it from devcontainer.json and
	// feature metadata (cfg.Mounts): docker-in-docker's
	// `dind-var-lib-docker-${devcontainerId}` → /var/lib/docker volume
	// is the case that motivated it, because without it the inner
	// dockerd's data root lands on the container's overlay root and
	// overlay-on-overlay fails every `docker run`.
	//
	// A named volume (Type volume, non-empty Source) is also declared
	// as a top-level project volume, so it is created as
	// `<project>_<source>` the same way a user-declared one is. A
	// mount whose target the user's compose service already declares
	// is skipped: the user's entry wins.
	ExtraMounts []Mount

	// ExtraEnvironment is merged into the primary service's
	// environment map. Compose merges maps natively, so we only
	// need to emit our additions.
	ExtraEnvironment map[string]string

	// Labels written on the primary service so Engine.Attach's
	// label-based lookup works (`dev.containers.id`, etc.).
	Labels map[string]string

	// Security/entrypoint options merged from feature + image metadata
	// (config.ResolvedConfig.{Privileged,Init,CapAdd,SecurityOpt}).
	// These mirror the `docker run --privileged/--init/--cap-add/
	// --security-opt` flags the image/Dockerfile path applies; on the
	// compose path upstream devcontainers/cli emits the equivalent
	// service fields into its generated override compose file. Without
	// them a feature like docker-in-docker (which declares
	// privileged/init/capAdd) silently fails on compose-source
	// devcontainers.
	//
	// Privileged / Init are pointers so nil means "no change" — we must
	// not clobber a user's `privileged: true` in their compose file when
	// no feature requested it. CapAdd / SecurityOpt are unioned with the
	// service's existing entries (dedup, user entries preserved).
	Privileged  *bool
	Init        *bool
	CapAdd      []string
	SecurityOpt []string

	// Entrypoints is the ordered chain of feature/image-metadata
	// entrypoint scripts (config.ResolvedConfig.Entrypoints). When
	// non-empty the primary service's entrypoint is replaced with a
	// generated wrapper that runs each in sequence then execs the
	// original entrypoint+command — mirroring devcontainers/cli's
	// generated compose override. Required for features like
	// docker-in-docker whose dockerd is launched by docker-init.sh.
	Entrypoints []string

	// OriginalEntrypoint is the entrypoint to preserve underneath the
	// wrapper: the service's own `entrypoint:` if it declared one, else
	// the image's ENTRYPOINT. The service's `command` is left untouched
	// (the wrapper execs entrypoint+command together). Only consulted
	// when Entrypoints is non-empty.
	OriginalEntrypoint []string
}

// RenderEntrypointWrapper builds the wrapper entrypoint array that runs
// each feature entrypoint in order, then execs the original
// entrypoint+command (`exec "$@"`), then falls back to a keep-alive
// loop. Mirrors devcontainers/cli's generated compose override entrypoint.
//
// escapeDollar controls $-escaping: the shellout path writes a YAML file
// that `docker compose` re-interpolates, so `$` must be doubled to `$$`
// to survive as a literal; the native path mutates the in-memory project
// (no interpolation) and must keep a single `$`.
func RenderEntrypointWrapper(entrypoints, original []string, escapeDollar bool) []string {
	script := "echo Container started\n" +
		"trap \"exit 0\" 15\n" +
		strings.Join(entrypoints, "\n") + "\n" +
		"exec \"$@\"\n" +
		"while sleep 1 & wait $!; do :; done"
	arr := append([]string{"/bin/sh", "-c", script, "-"}, original...)
	if escapeDollar {
		for i := range arr {
			arr[i] = strings.ReplaceAll(arr[i], "$", "$$")
		}
	}
	return arr
}

// BindMount describes one bind volume in the override.
type BindMount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// Mount describes one non-bind mount in the override: a named or
// anonymous volume, or a tmpfs. Bind mounts keep using BindMount.
type Mount struct {
	// Type is composetypes.VolumeTypeVolume or
	// composetypes.VolumeTypeTmpfs. Any other value is rejected by
	// ApplyRunOverride and WriteRunOverride.
	Type string

	// Source is the volume name, unscoped (the project prefix is added
	// the way compose adds it for a top-level volume). Empty means an
	// anonymous volume. Must be empty for tmpfs.
	Source string

	Target   string
	ReadOnly bool

	// NoCopy disables copying image content into a fresh volume
	// (volume only).
	NoCopy bool

	// TmpfsSize (bytes) and TmpfsMode (permission bits) size and mode
	// a tmpfs mount; zero leaves the daemon default (tmpfs only).
	TmpfsSize int64
	TmpfsMode uint32
}

// validateMounts rejects mounts the override cannot express, so a
// caller's mistake fails loudly instead of producing a bind or a
// mount with the wrong semantics.
func validateMounts(mounts []Mount) error {
	for _, m := range mounts {
		if m.Target == "" {
			return fmt.Errorf("compose override: %s mount with source %q has no target", m.Type, m.Source)
		}
		switch m.Type {
		case composetypes.VolumeTypeVolume:
		case composetypes.VolumeTypeTmpfs:
			if m.Source != "" {
				return fmt.Errorf("compose override: tmpfs mount at %q must not have a source (got %q)", m.Target, m.Source)
			}
		default:
			return fmt.Errorf("compose override: mount at %q has unsupported type %q (want %q or %q)",
				m.Target, m.Type, composetypes.VolumeTypeVolume, composetypes.VolumeTypeTmpfs)
		}
	}
	return nil
}

// serviceVolume converts m to the compose-go service volume it
// declares.
func (m Mount) serviceVolume() composetypes.ServiceVolumeConfig {
	v := composetypes.ServiceVolumeConfig{
		Type:     m.Type,
		Source:   m.Source,
		Target:   m.Target,
		ReadOnly: m.ReadOnly,
	}
	switch m.Type {
	case composetypes.VolumeTypeVolume:
		if m.NoCopy {
			v.Volume = &composetypes.ServiceVolumeVolume{NoCopy: true}
		}
	case composetypes.VolumeTypeTmpfs:
		if m.TmpfsSize != 0 || m.TmpfsMode != 0 {
			v.Tmpfs = &composetypes.ServiceVolumeTmpfs{
				Size: composetypes.UnitBytes(m.TmpfsSize),
				Mode: m.TmpfsMode,
			}
		}
	}
	return v
}

// namedVolume reports whether m is a named volume, which needs a
// top-level project volume declaration.
func (m Mount) namedVolume() bool {
	return m.Type == composetypes.VolumeTypeVolume && m.Source != ""
}

// WriteBuildOverride writes dst with the build override. Pins the
// primary service's image to ov.Image and clears any inherited
// build: directive via the YAML !reset tag (compose v2 idiom).
//
// Returns an error if ov.Image or ov.Service is empty.
func WriteBuildOverride(dst string, ov Override) error {
	if ov.Service == "" {
		return fmt.Errorf("WriteBuildOverride: ov.Service required")
	}
	if ov.Image == "" {
		return fmt.Errorf("WriteBuildOverride: ov.Image required")
	}

	// Hand-built YAML: the !reset tag is compose-v2 specific and
	// awkward to emit through gopkg.in/yaml.v3's struct path. Inline
	// templating keeps the output deterministic.
	body := fmt.Sprintf(`# Generated by devcontainer-go — do not edit by hand.
# Pins the primary service's image to a tag built by the engine and
# clears any base build: directive (mutually exclusive with image:).
services:
  %s:
    image: %s
    build: !reset null
`, yamlScalar(ov.Service), yamlScalar(ov.Image))

	return writeFile(dst, body)
}

// WriteRunOverride writes dst with the run-time override: workspace
// bind mount, container env, labels. Reads the primary service's
// existing volumes from `project` and emits the full merged list so
// compose v2's "sequence-replace" merge doesn't drop the user's
// declarations.
//
// `project` may be nil; in that case the override emits only our
// declarations (caller asserts no user-declared volumes exist).
func WriteRunOverride(dst string, project *composetypes.Project, ov Override) error {
	if ov.Service == "" {
		return fmt.Errorf("WriteRunOverride: ov.Service required")
	}

	if err := validateMounts(ov.ExtraMounts); err != nil {
		return err
	}
	merged, named, err := mergedVolumes(project, ov)
	if err != nil {
		return err
	}

	// cap_add / security_opt use compose v2's sequence-REPLACE merge, so
	// (like volumes) we union with the user's existing service entries
	// and re-emit the full list rather than risk dropping them.
	emit := ov
	if project != nil {
		if svc, err := PrimaryService(project, ov.Service); err == nil {
			emit.CapAdd = unionStrings(svc.CapAdd, ov.CapAdd)
			emit.SecurityOpt = unionStrings(svc.SecurityOpt, ov.SecurityOpt)
		}
	}

	doc := map[string]any{
		"services": map[string]any{
			ov.Service: buildServiceOverride(merged, emit),
		},
	}
	// Top-level declarations for the named volumes we added, so
	// `docker compose` creates them as `<project>_<name>`. A volume the
	// user's project already declares is left to their declaration;
	// compose merges top-level maps, so omitting it keeps theirs intact.
	if len(named) > 0 {
		vols := make(map[string]any, len(named))
		for _, name := range named {
			if project != nil {
				if _, declared := project.Volumes[name]; declared {
					continue
				}
			}
			vols[name] = map[string]any{}
		}
		if len(vols) > 0 {
			doc["volumes"] = vols
		}
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal run override: %w", err)
	}
	return writeFile(dst, "# Generated by devcontainer-go — do not edit by hand.\n"+string(body))
}

// mergedVolumes combines existing volumes from the user's primary
// service with ov.ExtraBindMounts and ov.ExtraMounts. Existing entries
// are preserved as-is (string CSV form or full object form, whichever
// the user used). New mounts are appended in the order supplied; an
// ExtraMounts entry whose target is already declared is skipped (the
// user's entry wins, matching ApplyRunOverride). Returns the merged
// list and the names of the named volumes it added.
func mergedVolumes(project *composetypes.Project, ov Override) ([]any, []string, error) {
	var existing []composetypes.ServiceVolumeConfig
	if project != nil {
		svc, err := PrimaryService(project, ov.Service)
		if err == nil {
			existing = svc.Volumes
		}
	}

	out := make([]any, 0, len(existing)+len(ov.ExtraBindMounts)+len(ov.ExtraMounts))
	targets := make(map[string]struct{}, cap(out))
	for _, v := range existing {
		out = append(out, serializeServiceVolume(v))
		targets[v.Target] = struct{}{}
	}
	for _, b := range ov.ExtraBindMounts {
		out = append(out, map[string]any{
			"type":      "bind",
			"source":    b.Source,
			"target":    b.Target,
			"read_only": b.ReadOnly,
		})
		targets[b.Target] = struct{}{}
	}
	var named []string
	for _, m := range ov.ExtraMounts {
		if _, exists := targets[m.Target]; exists {
			continue
		}
		targets[m.Target] = struct{}{}
		out = append(out, serializeServiceVolume(m.serviceVolume()))
		if m.namedVolume() && !slices.Contains(named, m.Source) {
			named = append(named, m.Source)
		}
	}
	return out, named, nil
}

// serializeServiceVolume converts a compose-go ServiceVolumeConfig
// into the wire form yaml.Marshal will emit. We use the full object
// form even if the user originally wrote a string ("a:b" CSV) — losing
// the original form is fine since compose accepts either.
func serializeServiceVolume(v composetypes.ServiceVolumeConfig) map[string]any {
	out := map[string]any{
		"type":   v.Type,
		"source": v.Source,
		"target": v.Target,
	}
	if v.Source == "" {
		// Anonymous volume / tmpfs: an empty source is not a valid
		// long-syntax value.
		delete(out, "source")
	}
	if v.ReadOnly {
		out["read_only"] = true
	}
	if v.Volume != nil && v.Volume.NoCopy {
		out["volume"] = map[string]any{"nocopy": true}
	}
	if v.Tmpfs != nil {
		tmpfs := map[string]any{}
		if v.Tmpfs.Size != 0 {
			tmpfs["size"] = int64(v.Tmpfs.Size)
		}
		if v.Tmpfs.Mode != 0 {
			tmpfs["mode"] = v.Tmpfs.Mode
		}
		if len(tmpfs) > 0 {
			out["tmpfs"] = tmpfs
		}
	}
	return out
}

func buildServiceOverride(volumes []any, ov Override) map[string]any {
	svc := map[string]any{}
	if len(volumes) > 0 {
		svc["volumes"] = volumes
	}
	if len(ov.ExtraEnvironment) > 0 {
		// Use a sorted map for deterministic output. yaml.v3 sorts
		// map keys alphabetically by default, but emitting a
		// []yaml.MapItem-equivalent here gives us explicit control
		// for golden tests.
		env := make(map[string]any, len(ov.ExtraEnvironment))
		for _, k := range sortedKeys(ov.ExtraEnvironment) {
			env[k] = ov.ExtraEnvironment[k]
		}
		svc["environment"] = env
	}
	if len(ov.Labels) > 0 {
		labels := make(map[string]any, len(ov.Labels))
		for _, k := range sortedKeys(ov.Labels) {
			labels[k] = ov.Labels[k]
		}
		svc["labels"] = labels
	}
	if ov.Privileged != nil {
		svc["privileged"] = *ov.Privileged
	}
	if ov.Init != nil {
		svc["init"] = *ov.Init
	}
	if len(ov.CapAdd) > 0 {
		svc["cap_add"] = append([]string(nil), ov.CapAdd...)
	}
	if len(ov.SecurityOpt) > 0 {
		svc["security_opt"] = append([]string(nil), ov.SecurityOpt...)
	}
	if len(ov.Entrypoints) > 0 {
		// Escaped: this YAML is re-interpolated by `docker compose`.
		svc["entrypoint"] = RenderEntrypointWrapper(ov.Entrypoints, ov.OriginalEntrypoint, true)
	}
	return svc
}

// unionStrings returns existing followed by any entries in add not
// already present, preserving order and dropping duplicates. Used to
// merge feature-contributed cap_add / security_opt into a service's
// own entries without clobbering either.
func unionStrings(existing, add []string) []string {
	if len(add) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(add))
	out := make([]string, 0, len(existing)+len(add))
	for _, s := range existing {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range add {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// yamlScalar quotes a value if it contains characters that would
// require quoting in YAML. Conservative: any non-alphanumeric / dot /
// dash / underscore / slash / colon / @ character triggers quoting.
// (Image refs and service names are well-behaved, but we play it safe.)
func yamlScalar(s string) string {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '/' ||
			c == ':' || c == '@') {
			return fmt.Sprintf("%q", s)
		}
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeFile writes body to path, creating the parent directory if
// needed. Mode 0644 so users can inspect the generated YAML.
func writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
