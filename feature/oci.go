package feature

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// devContainerFeatureMediaType is the OCI artifact config media type
// used by the devcontainers spec for feature publication.
const devContainerFeatureMediaType = "application/vnd.devcontainers"

func (s *DiskStore) fetchOCI(ctx context.Context, ref string) (Fetched, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return Fetched{}, fmt.Errorf("parse OCI ref %q: %w", ref, err)
	}

	img, err := remote.Image(parsed,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(s.ociKeychain),
	)
	if err != nil {
		return Fetched{}, fmt.Errorf("fetch OCI image %s: %w", ref, err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		return Fetched{}, fmt.Errorf("read OCI manifest %s: %w", ref, err)
	}
	if mt := string(manifest.Config.MediaType); !strings.HasPrefix(mt, devContainerFeatureMediaType) {
		return Fetched{}, fmt.Errorf("OCI ref %s: config media type %q is not a devcontainer feature", ref, mt)
	}

	digest, err := img.Digest()
	if err != nil {
		return Fetched{}, fmt.Errorf("OCI digest: %w", err)
	}
	pinned := parsed.Context().Name() + "@" + digest.String()

	// Cache layout: <cacheDir>/oci/sha256-<hex>/extracted/
	entryDir := filepath.Join(s.cacheDir, "oci", "sha256-"+digest.Hex)
	extractedDir := filepath.Join(entryDir, "extracted")

	// Cache hit: extracted dir already has the metadata file.
	if _, err := os.Stat(filepath.Join(extractedDir, metadataFile)); err == nil {
		meta, err := parseMetadata(extractedDir)
		if err == nil {
			return Fetched{Dir: extractedDir, ResolvedRef: pinned, Metadata: meta}, nil
		}
	}

	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		return Fetched{}, fmt.Errorf("mkdir %s: %w", entryDir, err)
	}

	layers, err := img.Layers()
	if err != nil {
		return Fetched{}, fmt.Errorf("OCI layers: %w", err)
	}
	if len(layers) == 0 {
		return Fetched{}, fmt.Errorf("OCI ref %s: image has no layers", ref)
	}
	// Spec: a feature artifact is a single layer. The layer's media
	// type is "application/vnd.devcontainers.layer.v1+tar" (plain tar,
	// not gzip). go-containerregistry's Uncompressed() returns the
	// inner tar bytes regardless of whether the layer was gzipped on
	// the wire — this is the correct call for feature extraction.
	rc, err := layers[0].Uncompressed()
	if err != nil {
		return Fetched{}, fmt.Errorf("OCI layer read: %w", err)
	}
	defer rc.Close()

	if err := os.RemoveAll(extractedDir); err != nil {
		return Fetched{}, err
	}
	if err := extractTar(rc, extractedDir); err != nil {
		return Fetched{}, fmt.Errorf("extract OCI feature %s: %w", ref, err)
	}
	if _, err := os.Stat(filepath.Join(extractedDir, installScript)); err != nil {
		return Fetched{}, fmt.Errorf("OCI feature %s: missing %s after extract: %w", ref, installScript, err)
	}
	meta, err := parseMetadata(extractedDir)
	if err != nil {
		return Fetched{}, err
	}

	return Fetched{Dir: extractedDir, ResolvedRef: pinned, Metadata: meta}, nil
}
