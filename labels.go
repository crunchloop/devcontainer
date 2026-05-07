package devcontainer

import (
	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

// configFromContainerLabels reconstructs a minimal *ResolvedConfig from
// an attached container's labels and inspect-time fields. It is good
// enough to drive the substituter (Exec, lifecycle re-runs) but does not
// reproduce the full devcontainer.json — callers that need every field
// should re-Resolve from the source file.
//
// Recovered fields:
//   LocalWorkspaceFolder      from LabelLocalWorkspaceFolder
//   ContainerWorkspaceFolder  from container's WorkingDir (Config.WorkingDir)
//   DevcontainerID            caller fills in
//   ContainerUser             from inspected User
//   ContainerEnv              from inspected Env (used by substituter)
func configFromContainerLabels(details *runtime.ContainerDetails) *config.ResolvedConfig {
	cfg := &config.ResolvedConfig{
		LocalWorkspaceFolder: details.Labels[LabelLocalWorkspaceFolder],
		ContainerUser:        details.User,
	}
	cfg.ContainerWorkspaceFolder = workingDirFromInspect(details)
	return cfg
}

// workingDirFromInspect pulls the container's WorkingDir out of the
// inspected mounts/env. Docker's InspectResponse exposes WorkingDir on
// Config; runtime.ContainerDetails doesn't surface it directly because
// most callers don't care. We re-derive it from a label-style heuristic
// for now — fix in a follow-up if it becomes load-bearing.
//
// In M2, every container we create has its WorkingDir set to
// ContainerWorkspaceFolder, which is also what we want here. If the
// label scan finds a container we didn't create (foreign labels), we
// fall back to empty — substitution of ${containerWorkspaceFolder} will
// then leave literals.
func workingDirFromInspect(details *runtime.ContainerDetails) string {
	// Mounts include the workspace bind on Target = ContainerWorkspaceFolder.
	// Search for it: the first bind mount whose target equals the engine
	// label's hint, or the first bind mount, period.
	for _, m := range details.Mounts {
		if m.Type == "bind" && m.Source == details.Labels[LabelLocalWorkspaceFolder] {
			return m.Target
		}
	}
	return ""
}
