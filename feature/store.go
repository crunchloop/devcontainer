package feature

import (
	"context"
	"errors"

	"github.com/crunchloop/devcontainer/config"
)

// Store fetches the artifact bytes for one feature reference and returns
// the unpacked-on-disk path plus parsed metadata. Implementations cache
// fetched artifacts so repeat calls for the same ref hit local disk.
//
// Local refs (paths starting with ./, ../, or /) must be resolved to
// absolute paths by the caller before invocation; Store does not know
// about the source devcontainer.json directory.
type Store interface {
	Fetch(ctx context.Context, ref string, kind config.FeatureSourceKind) (Fetched, error)
}

// Fetched is the result of resolving one feature.
type Fetched struct {
	// Dir is the absolute path to the directory containing
	// devcontainer-feature.json and install.sh.
	Dir string

	// ResolvedRef is the pinned reference: a digest for OCI, the
	// content hash (sha256) for HTTPS, or the absolute path for Local.
	// This is what gets recorded on the devcontainer.metadata label so
	// rebuilds use the exact same bytes even if a tag has moved.
	ResolvedRef string

	// Metadata is the parsed devcontainer-feature.json.
	Metadata config.FeatureMetadata
}

// ErrNotImplemented is returned by stores for source kinds they don't
// (yet) support. PR6's DiskStore returns this for OCI and HTTPS.
var ErrNotImplemented = errors.New("feature: store does not support this source kind")
