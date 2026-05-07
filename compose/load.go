package compose

import (
	"context"
	"fmt"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// LoadOptions configures Load.
type LoadOptions struct {
	// Files lists compose files in declaration order — earlier files
	// are overridden by later ones. Required.
	Files []string

	// WorkingDir is the directory compose-go uses as the base for
	// relative-path resolution inside the project (build contexts,
	// env_file paths, etc.). Required.
	WorkingDir string

	// ProjectName is set on the resulting types.Project. Engine derives
	// it from the workspace id; required.
	ProjectName string

	// Profiles activates compose profiles. Empty = no profiles
	// activated (which keeps services without `profiles:` running and
	// drops services that opt into a profile).
	Profiles []string

	// Env contains environment variables exposed to compose's $VAR
	// interpolation. Empty falls back to compose-go's default
	// behavior of reading os.Environ().
	Env []string
}

// Load parses the compose project described by opts. The returned
// *types.Project carries fully resolved interpolation, extends, and
// profile selection — caller doesn't need to repeat those.
//
// compose-go's Project is intentionally not modified by callers
// directly (immutability invariant in v2); use Override to produce
// override YAML rather than mutating in place.
func Load(ctx context.Context, opts LoadOptions) (*composetypes.Project, error) {
	if len(opts.Files) == 0 {
		return nil, fmt.Errorf("compose.Load: at least one file required")
	}
	if opts.WorkingDir == "" {
		return nil, fmt.Errorf("compose.Load: WorkingDir required")
	}
	if opts.ProjectName == "" {
		return nil, fmt.Errorf("compose.Load: ProjectName required")
	}

	projOpts, err := composecli.NewProjectOptions(
		opts.Files,
		composecli.WithName(opts.ProjectName),
		composecli.WithWorkingDirectory(opts.WorkingDir),
		composecli.WithProfiles(opts.Profiles),
		composecli.WithEnv(opts.Env),
		composecli.WithDotEnv,
	)
	if err != nil {
		return nil, fmt.Errorf("compose options: %w", err)
	}

	project, err := composecli.ProjectFromOptions(ctx, projOpts)
	if err != nil {
		return nil, fmt.Errorf("compose load: %w", err)
	}
	return project, nil
}

// PrimaryService returns the service named `name` from the project.
// Returns nil + error if the service doesn't exist (typical cause:
// devcontainer.json's "service" field doesn't match any service in
// the loaded compose project).
func PrimaryService(project *composetypes.Project, name string) (*composetypes.ServiceConfig, error) {
	if project == nil {
		return nil, fmt.Errorf("compose.PrimaryService: project is nil")
	}
	svc, err := project.GetService(name)
	if err != nil {
		return nil, fmt.Errorf("primary service %q: %w", name, err)
	}
	return &svc, nil
}
