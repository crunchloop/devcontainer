package events

const (
	TypeFeatureResolveStart = "feature.resolve_start"
	TypeFeatureResolved     = "feature.resolved"
	TypeFeatureSkipped      = "feature.skipped"
)

// FeatureResolveStartEvent fires immediately before a feature is fetched
// (OCI pull, HTTPS download, or local read).
type FeatureResolveStartEvent struct {
	Base
	Ref string
}

func (FeatureResolveStartEvent) EventType() string { return TypeFeatureResolveStart }

// FeatureResolvedEvent fires after a feature is fetched (or cache hit).
// Digest is the content-addressed identifier (sha256 for OCI/HTTPS, empty
// for local paths). FromCache is true when no network/disk read was needed
// beyond the cache lookup.
type FeatureResolvedEvent struct {
	Base
	Ref       string
	Digest    string
	FromCache bool
}

func (FeatureResolvedEvent) EventType() string { return TypeFeatureResolved }

// FeatureSkippedEvent fires when a requested feature is already present in
// the base image's devcontainer.metadata label and is therefore not
// re-installed. Reason is a short tag, e.g. "already_installed".
type FeatureSkippedEvent struct {
	Base
	Ref    string
	Reason string
}

func (FeatureSkippedEvent) EventType() string { return TypeFeatureSkipped }
