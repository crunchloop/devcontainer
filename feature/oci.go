package feature

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// devContainerFeatureMediaType is the OCI artifact config media type
// used by the devcontainers spec for feature publication.
const devContainerFeatureMediaType = "application/vnd.devcontainers"

// OCI cache policy. A feature artifact is content-addressed, so a
// cached copy is exactly as good as a download of the same digest; the
// only question is when the registry must be asked which digest a
// reference means. DiskStore answers it like this:
//
//   - Digest reference (name@sha256:…): local first. A cached copy of
//     that digest is used without contacting the registry.
//   - Exact version tag (name:X.Y.Z): local first. Published feature
//     versions are immutable by the spec's publishing convention (the
//     reference CLI publishes X.Y.Z once and moves only the floating
//     X, X.Y and latest tags), so the digest recorded the last time
//     this tag was resolved is used without contacting the registry.
//   - Floating tag (name:X, name:X.Y, name:latest, any other tag):
//     online first, as before, so a moved tag is picked up. If the
//     registry cannot be reached at all — no HTTP response: DNS,
//     connect, TLS or timeout failures — the digest recorded the last
//     time this tag was resolved is used. A registry that answers with
//     an error (not found, unauthorized, …) is authoritative and its
//     error is returned, as is any failure when nothing is recorded.
//
// Tag resolutions are recorded under <cacheDir>/oci/tags/ after every
// successful online fetch. The recorded digest is only used when its
// extracted content is still in the cache.
func (s *DiskStore) fetchOCI(ctx context.Context, ref string) (Fetched, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return Fetched{}, fmt.Errorf("parse OCI ref %q: %w", ref, err)
	}
	repo := parsed.Context().Name()

	switch r := parsed.(type) {
	case name.Digest:
		if f, ok := s.cachedOCI(repo, r.DigestStr()); ok {
			return f, nil
		}
	case name.Tag:
		if isExactVersionTag(r.TagStr()) {
			if f, ok := s.cachedTag(r); ok {
				return f, nil
			}
		}
	}

	img, err := remote.Image(parsed, s.remoteOptions(ctx)...)
	if err != nil {
		if tag, isTag := parsed.(name.Tag); isTag && ctx.Err() == nil && registryUnreachable(err) {
			if f, ok := s.cachedTag(tag); ok {
				return f, nil
			}
		}
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
	f, err := s.extractOCI(ref, repo, digest, img)
	if err != nil {
		return Fetched{}, err
	}
	if tag, isTag := parsed.(name.Tag); isTag {
		// Best effort: a failed record only costs a later offline or
		// local-first lookup, never this fetch.
		_ = s.recordTag(tag, digest.String())
	}
	return f, nil
}

// remoteOptions are the go-containerregistry options for every OCI
// registry call.
func (s *DiskStore) remoteOptions(ctx context.Context) []remote.Option {
	opts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(s.ociKeychain),
	}
	if s.ociTransport != nil {
		opts = append(opts, remote.WithTransport(s.ociTransport))
	}
	return opts
}

// exactVersionTag matches a full semantic version with no pre-release
// or build suffix: the only tag form DiskStore treats as immutable.
var exactVersionTag = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func isExactVersionTag(tag string) bool { return exactVersionTag.MatchString(tag) }

// registryUnreachable reports whether err means no HTTP response came
// back from the registry. A *transport.Error carries a response status
// (the registry answered), so it is not an offline condition.
func registryUnreachable(err error) bool {
	var te *transport.Error
	return !errors.As(err, &te)
}

// ociEntryDir is the cache entry for one feature digest:
// <cacheDir>/oci/sha256-<hex>.
func (s *DiskStore) ociEntryDir(hex string) string {
	return filepath.Join(s.cacheDir, "oci", "sha256-"+hex)
}

// cachedOCI returns the cached, extracted feature for repo@digest, if
// the cache holds a complete extraction of it.
func (s *DiskStore) cachedOCI(repo, digest string) (Fetched, bool) {
	h, err := v1.NewHash(digest)
	if err != nil || h.Algorithm != "sha256" {
		return Fetched{}, false
	}
	extractedDir := filepath.Join(s.ociEntryDir(h.Hex), "extracted")
	if _, err := os.Stat(filepath.Join(extractedDir, installScript)); err != nil {
		return Fetched{}, false
	}
	meta, err := parseMetadata(extractedDir)
	if err != nil {
		return Fetched{}, false
	}
	return Fetched{Dir: extractedDir, ResolvedRef: repo + "@" + h.String(), Metadata: meta}, true
}

// tagRecord is the on-disk record of one tag resolution.
type tagRecord struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

func (s *DiskStore) tagRecordPath(tag name.Tag) string {
	sum := sha256.Sum256([]byte(tag.Name()))
	return filepath.Join(s.cacheDir, "oci", "tags", hex.EncodeToString(sum[:])+".json")
}

// cachedTag returns the cached feature for the digest tag last
// resolved to, if both the record and its content are present.
func (s *DiskStore) cachedTag(tag name.Tag) (Fetched, bool) {
	b, err := os.ReadFile(s.tagRecordPath(tag))
	if err != nil {
		return Fetched{}, false
	}
	var rec tagRecord
	if err := json.Unmarshal(b, &rec); err != nil || rec.Ref != tag.Name() {
		return Fetched{}, false
	}
	return s.cachedOCI(tag.Context().Name(), rec.Digest)
}

// recordTag records that tag resolved to digest, atomically (write a
// temporary file, then rename over the record).
func (s *DiskStore) recordTag(tag name.Tag, digest string) error {
	path := s.tagRecordPath(tag)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(tagRecord{Ref: tag.Name(), Digest: digest})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tag-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// extractOCI returns the cached extraction of digest, or downloads and
// extracts img's single layer into the cache.
func (s *DiskStore) extractOCI(ref, repo string, digest v1.Hash, img v1.Image) (Fetched, error) {
	if f, ok := s.cachedOCI(repo, digest.String()); ok {
		return f, nil
	}
	pinned := repo + "@" + digest.String()

	// Cache layout: <cacheDir>/oci/sha256-<hex>/extracted/
	entryDir := filepath.Join(s.cacheDir, "oci", "sha256-"+digest.Hex)
	extractedDir := filepath.Join(entryDir, "extracted")

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

	// Extract beside the entry and rename into place, so the cache
	// never holds a partial extraction that the local-first lookups
	// above would take for a complete one.
	tmpDir, err := os.MkdirTemp(entryDir, ".extract-*")
	if err != nil {
		return Fetched{}, fmt.Errorf("mkdir extract tmpdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := extractTar(rc, tmpDir); err != nil {
		return Fetched{}, fmt.Errorf("extract OCI feature %s: %w", ref, err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, installScript)); err != nil {
		return Fetched{}, fmt.Errorf("OCI feature %s: missing %s after extract: %w", ref, installScript, err)
	}
	if _, err := parseMetadata(tmpDir); err != nil {
		return Fetched{}, err
	}
	if err := os.RemoveAll(extractedDir); err != nil {
		return Fetched{}, err
	}
	if err := os.Rename(tmpDir, extractedDir); err != nil {
		return Fetched{}, fmt.Errorf("install extracted feature %s: %w", ref, err)
	}
	meta, err := parseMetadata(extractedDir)
	if err != nil {
		return Fetched{}, err
	}

	return Fetched{Dir: extractedDir, ResolvedRef: pinned, Metadata: meta}, nil
}
