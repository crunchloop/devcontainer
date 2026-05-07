// Package runtime defines the container backend abstraction used by the
// devcontainer engine.
//
// The Runtime interface speaks containers and images only — it is
// deliberately ignorant of devcontainer-spec semantics (lifecycle phases,
// features, substitution). Spec semantics live in the parent
// devcontainer package; this layer is what swaps out for k8s, podman, or
// CLI-shim implementations in the future.
//
// The Docker implementation lives in runtime/docker.
package runtime
