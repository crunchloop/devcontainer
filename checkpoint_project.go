package devcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crunchloop/devcontainer/compose"
	"github.com/crunchloop/devcontainer/runtime"
)

// projectManifestName is the self-describing index a project checkpoint
// writes into its archive directory. RestoreProject reads it to learn the
// service set and restore order without re-deriving them.
const projectManifestName = "project.json"

// ProjectCheckpointOptions configures Engine.CheckpointProject.
type ProjectCheckpointOptions struct {
	// ArchiveDir is a directory (created if absent) that receives one
	// archive per service container plus a manifest. Required. Point it at
	// durable, transferable storage — the set is self-contained, so
	// RestoreProject can run on another node by moving the directory.
	ArchiveDir string

	// StopAfter stops each container once its archive is written (the
	// spot-eviction path). False keeps them running ("backup" checkpoint).
	StopAfter bool

	// TCPEstablished requests checkpoint of established TCP connections —
	// recommended for a multi-service project, whose services hold live
	// inter-container connections (see design/checkpoint-restore.md §7).
	TCPEstablished bool
}

// ServiceCheckpoint records one service container's archive within a
// project checkpoint.
type ServiceCheckpoint struct {
	Service     string `json:"service"`
	ContainerID string `json:"containerId"`
	// Archive is the archive's basename within the project ArchiveDir.
	Archive string `json:"archive"`
	Size    int64  `json:"size"`
}

// ProjectCheckpointRef describes a written project checkpoint.
type ProjectCheckpointRef struct {
	Project    string              `json:"project"`
	ArchiveDir string              `json:"-"`
	Services   []ServiceCheckpoint `json:"services"`
}

// ProjectRestoreOptions configures Engine.RestoreProject.
type ProjectRestoreOptions struct {
	// ArchiveDir is the directory a prior CheckpointProject wrote (it reads
	// the manifest at projectManifestName). Required.
	ArchiveDir string

	// TCPEstablished must match the checkpoint when archives captured
	// established connections.
	TCPEstablished bool

	// IgnoreVolumes skips restoring volume content from the archives,
	// reusing whatever volumes already exist. Leave false for a
	// cross-node restore (the destination has no volumes, so content
	// must come from the archive). Set true for a same-node
	// restore-in-place, where the volumes still exist with current data
	// and re-extracting them would collide ("volume already exists").
	IgnoreVolumes bool

	// LocalEnv overrides os.Environ() for the reattached primary
	// workspace's substituter (parity with RestoreOptions.LocalEnv).
	LocalEnv map[string]string
}

// ProjectRestore is the result of restoring a multi-service project.
type ProjectRestore struct {
	Project string

	// Primary is the reattached devcontainer workspace — the service whose
	// restored container carries the dev.containers.id label. Nil if the
	// project had no devcontainer service (e.g. an all-sidecar set).
	Primary *Workspace

	// Services maps compose service name → restored container for every
	// service in the project (including the primary's container).
	Services map[string]*runtime.Container
}

// CheckpointProject checkpoints every container of a compose project — the
// project the given workspace belongs to — to per-service archives under
// opts.ArchiveDir, then writes a manifest describing the set.
//
// The checkpoint primitive is per-container (design/checkpoint-restore.md
// §3); CheckpointProject is the engine-level sequencer over it for a
// multi-service project (decision recorded in §9). It enumerates the
// project's containers by their com.docker.compose.project label and
// checkpoints each via the same CheckpointRuntime the single-container
// Engine.Checkpoint uses — so it inherits the same capability gate and
// typed errors, and is independent of how the project was brought up.
//
// On any per-service failure it returns that error WITHOUT writing the
// manifest, so a present manifest always implies a complete set (a partial
// RestoreProject then fails cleanly on the missing manifest). Returns
// ErrCheckpointUnsupported (wrapped) if the backend can't checkpoint.
func (e *Engine) CheckpointProject(ctx context.Context, ws *Workspace, opts ProjectCheckpointOptions) (ProjectCheckpointRef, error) {
	if err := ctxIfDone(ctx); err != nil {
		return ProjectCheckpointRef{}, err
	}
	if ws == nil || ws.Container == nil {
		return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: workspace has no container")
	}
	if opts.ArchiveDir == "" {
		return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: ArchiveDir is required")
	}
	project := ws.Container.Labels[compose.LabelComposeProject]
	if project == "" {
		return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: workspace is not a compose project (missing %s label) — use Engine.Checkpoint for a single container", compose.LabelComposeProject)
	}

	cr, ok := e.runtime.(runtime.CheckpointRuntime)
	if !ok || !e.runtime.Capabilities().Checkpoint {
		return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: %w", runtime.ErrCheckpointUnsupported)
	}

	containers, err := e.runtime.ListContainers(ctx, runtime.LabelFilter{
		Match: map[string]string{compose.LabelComposeProject: project},
	})
	if err != nil {
		return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: list project %q containers: %w", project, err)
	}
	if len(containers) == 0 {
		return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: no containers found for project %q", project)
	}
	// Deterministic order by service name so the manifest (and restore
	// order) are stable across runs.
	sort.Slice(containers, func(i, j int) bool {
		return serviceName(containers[i]) < serviceName(containers[j])
	})

	if err := os.MkdirAll(opts.ArchiveDir, 0o755); err != nil {
		return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: create archive dir: %w", err)
	}

	ref := ProjectCheckpointRef{Project: project, ArchiveDir: opts.ArchiveDir}
	seen := make(map[string]bool, len(containers))
	for _, c := range containers {
		svc := serviceName(c)
		// Distinct archive per service; a collision would silently
		// overwrite (and restore would collapse the entries). v1 assumes
		// one container per service — reject scaled services explicitly.
		if seen[svc] {
			return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: duplicate service name %q in project %q (scaled services are not supported)", svc, project)
		}
		seen[svc] = true
		archive := svc + ".tar"
		cref, err := cr.Checkpoint(ctx, c.ID, runtime.CheckpointSpec{
			ArchivePath:    filepath.Join(opts.ArchiveDir, archive),
			StopAfter:      opts.StopAfter,
			TCPEstablished: opts.TCPEstablished,
		})
		if err != nil {
			return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: service %q: %w", svc, err)
		}
		ref.Services = append(ref.Services, ServiceCheckpoint{
			Service: svc, ContainerID: c.ID, Archive: archive, Size: cref.Size,
		})
	}

	// Manifest last: its presence marks a complete checkpoint.
	if err := writeProjectManifest(opts.ArchiveDir, ref); err != nil {
		return ProjectCheckpointRef{}, fmt.Errorf("CheckpointProject: write manifest: %w", err)
	}
	return ref, nil
}

// RestoreProject restores every service archive recorded in the manifest
// under opts.ArchiveDir and reattaches the project. The shared network
// re-forms as the containers come back (per-container restore re-attaches
// networking; design §7) — restore order is the manifest's (service-name)
// order, which is forgiving for reconnecting services.
//
// The service whose restored container carries the dev.containers.id label
// is reattached as the full Primary *Workspace (re-inspect + rebuild
// config + bind substituter), the same as Engine.Restore; the rest are
// returned as restored containers. Returns ErrCheckpointUnsupported
// (wrapped) if the backend can't, and a *runtime.RestoreFailedError on a
// per-service restore failure.
func (e *Engine) RestoreProject(ctx context.Context, opts ProjectRestoreOptions) (*ProjectRestore, error) {
	if err := ctxIfDone(ctx); err != nil {
		return nil, err
	}
	if opts.ArchiveDir == "" {
		return nil, fmt.Errorf("RestoreProject: ArchiveDir is required")
	}

	cr, ok := e.runtime.(runtime.CheckpointRuntime)
	if !ok || !e.runtime.Capabilities().Checkpoint {
		return nil, fmt.Errorf("RestoreProject: %w", runtime.ErrCheckpointUnsupported)
	}

	manifest, err := readProjectManifest(opts.ArchiveDir)
	if err != nil {
		return nil, fmt.Errorf("RestoreProject: %w", err)
	}

	// A cross-node restore lands on a fresh store with no project network.
	// The checkpointed containers were attached to <project>_default, and
	// libpod restore fails ("network not found") unless it exists first.
	// Recreate it before restoring any container, mirroring how
	// compose.Orchestrator.Up names + labels it (CreateNetwork is
	// idempotent — a label-matching network is reused, so same-node
	// restore is unaffected). Custom/extra compose networks aren't yet
	// recorded in the manifest — see design/checkpoint-restore-fixes.md.
	if _, err := e.runtime.CreateNetwork(ctx, runtime.NetworkSpec{
		Name: manifest.Project + "_default",
		Labels: map[string]string{
			compose.LabelComposeProject: manifest.Project,
			compose.LabelEngine:         compose.EngineDisplayName,
		},
	}); err != nil {
		return nil, fmt.Errorf("RestoreProject: recreate project network: %w", err)
	}

	out := &ProjectRestore{Project: manifest.Project, Services: map[string]*runtime.Container{}}
	for _, svc := range manifest.Services {
		// Restore (--import) re-creates the container under its archived,
		// deterministic compose name. On a fresh/cross-node store nothing
		// pre-exists. On a same-node restore the checkpoint left the source
		// container *stopped* under this name (StopAfter stops, it does not
		// remove), which collides with re-create ("that ID is already in
		// use"). Clear a non-running leftover — its full state is in the
		// archive — but refuse to clobber a *running* container: that would
		// be destroying a live service, not restoring it. RemoveVolumes
		// stays false so the service's data volume survives for reuse.
		name := manifest.Project + "-" + svc.Service + "-1"
		d, ierr := e.runtime.InspectContainer(ctx, name)
		switch {
		case ierr != nil:
			// Absent is the normal fresh/cross-node case — proceed. Any
			// other inspect failure (daemon/API/permission) is surfaced
			// rather than masked behind a downstream restore error.
			var notFound *runtime.ContainerNotFoundError
			if !errors.As(ierr, &notFound) {
				return nil, fmt.Errorf("RestoreProject: service %q: inspect existing container %q: %w", svc.Service, name, ierr)
			}
		case d != nil:
			if d.State == runtime.StateRunning {
				return nil, fmt.Errorf("RestoreProject: service %q: a running container %q already exists — stop it before restoring", svc.Service, name)
			}
			if rerr := e.runtime.RemoveContainer(ctx, name, runtime.RemoveOptions{Force: true}); rerr != nil {
				return nil, fmt.Errorf("RestoreProject: service %q: clearing stale container %q: %w", svc.Service, name, rerr)
			}
		}

		c, err := cr.Restore(ctx, runtime.RestoreSpec{
			ArchivePath:    filepath.Join(opts.ArchiveDir, svc.Archive),
			TCPEstablished: opts.TCPEstablished,
			IgnoreVolumes:  opts.IgnoreVolumes,
		})
		if err != nil {
			return nil, fmt.Errorf("RestoreProject: service %q: %w", svc.Service, err)
		}
		out.Services[svc.Service] = c

		// Reattach the devcontainer service as the Primary workspace. Its
		// restored container is the one carrying our id label (sidecars
		// carry only compose labels), so inspect to find out.
		details, err := e.inspectStable(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("RestoreProject: inspect restored %q (%s): %w", svc.Service, c.ID, err)
		}
		if id := details.Labels[LabelDevcontainerID]; id != "" && out.Primary == nil {
			out.Primary = e.reattachWorkspace(ctx, details, WorkspaceID(id), opts.LocalEnv)
		}
	}
	return out, nil
}

// serviceName returns a container's compose service name, falling back to
// its container name when the label is absent (a non-compose-managed
// container that nonetheless shares the project label).
func serviceName(c runtime.Container) string {
	if s := c.Labels[compose.LabelComposeService]; s != "" {
		return s
	}
	return c.Name
}

func writeProjectManifest(dir string, ref ProjectCheckpointRef) error {
	b, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename: rename is atomic, so a present project.json is
	// always complete — never a half-written file from an interrupted run.
	final := filepath.Join(dir, projectManifestName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func readProjectManifest(dir string) (ProjectCheckpointRef, error) {
	b, err := os.ReadFile(filepath.Join(dir, projectManifestName))
	if err != nil {
		return ProjectCheckpointRef{}, fmt.Errorf("read project manifest in %q: %w", dir, err)
	}
	var ref ProjectCheckpointRef
	if err := json.Unmarshal(b, &ref); err != nil {
		return ProjectCheckpointRef{}, fmt.Errorf("parse project manifest: %w", err)
	}
	if len(ref.Services) == 0 {
		return ProjectCheckpointRef{}, fmt.Errorf("project manifest has no services")
	}
	// Archive entries are joined onto ArchiveDir at restore; a tampered
	// manifest must not escape it. Require a plain basename.
	for _, s := range ref.Services {
		if s.Archive == "" || s.Archive != filepath.Base(s.Archive) || strings.Contains(s.Archive, "..") {
			return ProjectCheckpointRef{}, fmt.Errorf("project manifest has an unsafe archive entry %q", s.Archive)
		}
	}
	ref.ArchiveDir = dir
	return ref, nil
}
