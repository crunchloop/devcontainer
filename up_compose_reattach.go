package devcontainer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/compose"
	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/feature"
	"github.com/crunchloop/devcontainer/runtime"
)

// LabelComposeInputsHash is stamped on the primary service container
// of a compose-source devcontainer. Its value is composeInputsHash of
// everything that went into that container's image and run override
// before any feature was fetched: the container-shaping part of the
// resolved devcontainer.json (features as requested, with their
// options), the loaded compose project, the primary service's base
// image ID, and the engine options that reshape the project.
//
// A later Up whose freshly computed hash equals the stored one
// reattaches: it reads the merged metadata from the existing
// container's image label and starts the project's existing services,
// with no feature fetch and no image build — the compose counterpart
// of the image path's attachExisting. See reattachCompose.
const LabelComposeInputsHash = "dev.containers.compose-inputs-hash"

// composeInputsHashVersion is folded into the hash so a change to what
// the hash covers invalidates every stored value (the next Up takes the
// full path once and stamps the new form) instead of comparing
// incompatible digests.
const composeInputsHashVersion = 1

// maxLocalFeatureHashBytes bounds how much local feature content
// composeInputsHash reads. A local feature is a handful of scripts; a
// tree past this is not one, and the hash fails rather than reading it,
// which only costs the reattach (the full path still runs).
const maxLocalFeatureHashBytes = 64 << 20

// composeHashedConfig is the container-shaping subset of a freshly
// resolved (pre-feature-merge) ResolvedConfig. Fields that Up re-reads
// from devcontainer.json on every call and that never reach the
// container or its image — lifecycle commands, remoteEnv, ports
// attributes, customizations, the display name — are left out, so
// editing them does not cost a feature fetch and an image rebuild.
type composeHashedConfig struct {
	DevcontainerID           string
	Source                   *config.ComposeSource
	LocalWorkspaceFolder     string
	ContainerWorkspaceFolder string
	WorkspaceMount           *config.Mount
	ContainerUser            string
	RemoteUser               string
	UpdateRemoteUserUID      *bool
	ContainerEnv             map[string]string
	Mounts                   []config.Mount
	RunArgs                  []string
	Init                     *bool
	Privileged               *bool
	CapAdd                   []string
	SecurityOpt              []string
	OverrideCommand          *bool
	Features                 []composeHashedFeature
}

type composeHashedFeature struct {
	Ref        string
	SourceKind config.FeatureSourceKind
	Options    map[string]any
	// LocalContent is the content digest of a local feature's
	// directory. OCI and HTTPS features are identified by their
	// reference as written, matching the reference CLI, which does not
	// re-resolve feature tags when it starts an existing container.
	LocalContent string `json:",omitempty"`
}

type composeHashInput struct {
	Version int
	Config  composeHashedConfig
	// Project is the loaded compose model before any engine override.
	// The project's interpolation environment and working directory are
	// not hashed: they are inputs to the model, already reflected in it.
	Services composetypes.Services
	Networks composetypes.Networks
	Volumes  composetypes.Volumes
	Secrets  composetypes.Secrets
	Configs  composetypes.Configs
	// BaseImageID is the local image ID of the primary service's base
	// image (its image:, or the engine's build of its build:).
	BaseImageID string
	// Engine options that reshape the project or the feature layering.
	ComposeBackend            ComposeBackend
	DisableHostPortPublishing bool
	StrictFeatureVersionMatch bool
	// HostOwner is the workspace folder's owner, which selects the
	// updateRemoteUserUID reconciliation applied on Linux.
	HostOwner string
}

// composeInputsHash returns the hex digest stored as
// LabelComposeInputsHash. cfg must be the freshly resolved config,
// before layerFeatures and applyMetadataMerge mutate it; project must
// be the loaded project before any Apply*Override.
func (e *Engine) composeInputsHash(cfg *config.ResolvedConfig, opts UpOptions, project *composetypes.Project, baseImageID string) (string, error) {
	src, ok := cfg.Source.(*config.ComposeSource)
	if !ok {
		return "", fmt.Errorf("compose inputs hash: not a compose source")
	}
	if baseImageID == "" {
		return "", fmt.Errorf("compose inputs hash: base image ID unknown")
	}
	configDir := composeWorkingDir(cfg, opts)
	features := make([]composeHashedFeature, 0, len(cfg.Features))
	for _, f := range cfg.Features {
		hf := composeHashedFeature{Ref: f.Ref, SourceKind: f.SourceKind, Options: f.Options}
		if f.SourceKind == config.FeatureSourceLocal {
			digest, err := hashDirContent(feature.ResolveLocalRef(configDir, f.Ref), maxLocalFeatureHashBytes)
			if err != nil {
				return "", fmt.Errorf("compose inputs hash: local feature %s: %w", f.Ref, err)
			}
			hf.LocalContent = digest
		}
		features = append(features, hf)
	}
	in := composeHashInput{
		Version: composeInputsHashVersion,
		Config: composeHashedConfig{
			DevcontainerID:           cfg.DevcontainerID,
			Source:                   src,
			LocalWorkspaceFolder:     cfg.LocalWorkspaceFolder,
			ContainerWorkspaceFolder: cfg.ContainerWorkspaceFolder,
			WorkspaceMount:           cfg.WorkspaceMount,
			ContainerUser:            cfg.ContainerUser,
			RemoteUser:               cfg.RemoteUser,
			UpdateRemoteUserUID:      cfg.UpdateRemoteUserUID,
			ContainerEnv:             cfg.ContainerEnv,
			Mounts:                   cfg.Mounts,
			RunArgs:                  cfg.RunArgs,
			Init:                     cfg.Init,
			Privileged:               cfg.Privileged,
			CapAdd:                   cfg.CapAdd,
			SecurityOpt:              cfg.SecurityOpt,
			OverrideCommand:          cfg.OverrideCommand,
			Features:                 features,
		},
		Services:                  project.Services,
		Networks:                  project.Networks,
		Volumes:                   project.Volumes,
		Secrets:                   project.Secrets,
		Configs:                   project.Configs,
		BaseImageID:               baseImageID,
		ComposeBackend:            e.opts.ComposeBackend,
		DisableHostPortPublishing: e.opts.DisableHostPortPublishing,
		StrictFeatureVersionMatch: e.opts.StrictFeatureVersionMatch,
	}
	if uid, gid, ok := statOwner(cfg.LocalWorkspaceFolder); ok {
		in.HostOwner = fmt.Sprintf("%d:%d", uid, gid)
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("compose inputs hash: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// hashDirContent digests every regular file and symlink under root
// (relative path, mode and bytes, in lexical order), reading at most
// limit bytes. A symlink is hashed as the content it resolves to, the
// same way feature.GenerateBuildContext copies it into the build
// context, so editing a linked file outside the feature directory
// changes the digest. A symlink to a directory, which the build
// context copy cannot handle either, fails the hash.
func hashDirContent(root string, limit int64) (string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0 {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	var total int64
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return "", err
		}
		fi, err := os.Stat(p) // follows a symlink to the content it names
		if err != nil {
			return "", err
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%s: not a regular file", rel)
		}
		total += fi.Size()
		if total > limit {
			return "", fmt.Errorf("content exceeds %d bytes", limit)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%o\x00%d\x00", filepath.ToSlash(rel), fi.Mode().Perm(), fi.Size()) // hash.Hash writes never fail
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, io.LimitReader(f, fi.Size()))
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// composePrimaryBaseRef is the image reference whose local ID stands
// for the primary service's base image in composeInputsHash: its
// image:, or the tag prepareComposeServiceImage builds its build: to.
func composePrimaryBaseRef(cfg *config.ResolvedConfig, svc *composetypes.ServiceConfig) string {
	if svc.Build != nil {
		return "dc-go-compose-base-" + cfg.DevcontainerID + ":latest"
	}
	return svc.Image
}

// errComposeReattachMiss marks a reattach precondition that did not
// hold. Up then takes the full path; it is never returned to callers.
var errComposeReattachMiss = errors.New("compose reattach not possible")

func reattachMiss(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errComposeReattachMiss}, args...)...)
}

// reattachCompose is the compose counterpart of attachExisting. When
// the project's primary container exists and carries an
// LabelComposeInputsHash equal to the hash of the current inputs, it
//
//   - reads the merged feature and image metadata from that
//     container's image label, as attachExisting does, instead of
//     fetching every feature and rebuilding the final image;
//   - rebuilds the run override from it (entrypoint wrapper, mounts,
//     env, privileged, capAdd, ...) exactly as the full path does; and
//   - brings the project up through the selected backend against the
//     existing images: the native orchestrator under the caller's
//     AdoptExisting with sidecar builds skipped when their images are
//     present, or `docker compose up --no-recreate` (which builds only
//     missing images) on the shell-out backend.
//
// It returns (nil, errComposeReattachMiss-wrapped) when a precondition
// does not hold — no primary container, no or different hash label,
// the image tag the container was created from now names another
// image — and the caller falls back to the full path. cfg is not
// mutated on a miss.
func (e *Engine) reattachCompose(
	ctx context.Context,
	cfg *config.ResolvedConfig,
	opts UpOptions,
	project *composetypes.Project,
	src *config.ComposeSource,
	primary *composetypes.ServiceConfig,
	projectName, workingDir string,
) (*Workspace, error) {
	if opts.override != nil {
		return nil, reattachMiss("image build requested")
	}
	if opts.PullPolicy == PullAlways {
		return nil, reattachMiss("pull policy is always")
	}
	containers, err := e.runtime.ListContainers(ctx, runtime.LabelFilter{Match: map[string]string{
		compose.LabelComposeProject: projectName,
		compose.LabelComposeService: src.Service,
		LabelDevcontainerID:         cfg.DevcontainerID,
	}})
	if err != nil {
		return nil, reattachMiss("list primary containers: %v", err)
	}
	if len(containers) != 1 {
		return nil, reattachMiss("%d primary containers", len(containers))
	}
	details, err := e.runtime.InspectContainer(ctx, containers[0].ID)
	if err != nil || details == nil {
		return nil, reattachMiss("inspect primary container: %v", err)
	}
	stored := details.Labels[LabelComposeInputsHash]
	if stored == "" {
		return nil, reattachMiss("primary container has no %s label", LabelComposeInputsHash)
	}
	base, err := e.runtime.InspectImage(ctx, composePrimaryBaseRef(cfg, primary))
	if err != nil || base == nil || base.ID == "" {
		return nil, reattachMiss("base image not present locally: %v", err)
	}
	current, err := e.composeInputsHash(cfg, opts, project, base.ID)
	if err != nil {
		return nil, reattachMiss("%v", err)
	}
	if current != stored {
		return nil, reattachMiss("inputs changed")
	}
	if details.ImageRef == "" {
		return nil, reattachMiss("primary container image reference unknown")
	}
	img, err := e.runtime.InspectImage(ctx, details.ImageRef)
	if err != nil || img == nil {
		return nil, reattachMiss("image %s not present locally: %v", details.ImageRef, err)
	}
	if img.ID != details.Image {
		// Someone re-tagged it (Engine.Build, a manual build): the tag
		// no longer names the image the container runs, and a later
		// recreate from it would silently switch images.
		return nil, reattachMiss("image %s now names %s, container runs %s", details.ImageRef, img.ID, details.Image)
	}

	// Committed: from here on cfg is merged from the label.
	var layers []config.FeatureMetadata
	if label := img.Labels[feature.MetadataLabel]; label != "" {
		parsed, err := feature.ParseMetadataLabel(label)
		if err != nil {
			return nil, fmt.Errorf("compose reattach: parse %s label of %s: %w", feature.MetadataLabel, details.ImageRef, err)
		}
		layers = parsed
	}
	for _, f := range cfg.Features {
		opts.bus.Emit(events.FeatureSkippedEvent{Ref: f.Ref, Reason: "compose_reattach"})
	}
	applyMetadataMerge(cfg, layers, opts.LocalEnv)

	// Reattach never re-runs secretsCommand: the container's env is
	// already baked (see UpOptions.RunSecretsCommand).
	runOverride := e.composeRunOverride(ctx, cfg, opts, project, src, details.ImageRef, nil, stored)
	return e.upComposeBackend(ctx, cfg, opts, project, src, projectName, workingDir, details.ImageRef, runOverride, true, true)
}
