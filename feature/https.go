package feature

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// httpsFilenameRegexp validates the filename component of an HTTPS
// feature URL. Per spec, feature artifacts hosted over HTTPS must be
// named devcontainer-feature-<id>.tgz so a server can serve multiple
// features from one directory without ambiguity.
var httpsFilenameRegexp = regexp.MustCompile(`^devcontainer-feature-[A-Za-z0-9_-]+\.tgz$`)

// httpsClient is the default HTTP client used by the HTTPS resolver.
// Single-shot DiskStore instances reuse it across calls; concurrent
// fetches share the connection pool.
var httpsClient = &http.Client{
	Timeout: 5 * time.Minute,
}

func (s *DiskStore) fetchHTTPS(ctx context.Context, ref string) (Fetched, error) {
	parsed, err := url.Parse(ref)
	if err != nil {
		return Fetched{}, fmt.Errorf("parse URL %q: %w", ref, err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return Fetched{}, fmt.Errorf("HTTPS feature must use http(s) scheme, got %q", parsed.Scheme)
	}
	filename := path.Base(parsed.Path)
	if !httpsFilenameRegexp.MatchString(filename) {
		return Fetched{}, fmt.Errorf("HTTPS feature URL %q: filename %q must match devcontainer-feature-<id>.tgz", ref, filename)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return Fetched{}, fmt.Errorf("build request: %w", err)
	}
	for _, k := range sortedKeys(s.httpsHeaders) {
		req.Header.Set(k, s.httpsHeaders[k])
	}

	resp, err := s.httpsDo(req)
	if err != nil {
		return Fetched{}, fmt.Errorf("GET %s: %w", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Fetched{}, fmt.Errorf("GET %s: status %d", ref, resp.StatusCode)
	}

	// Stream into a temp file while hashing so we can use the digest
	// as the cache key. Avoids buffering the whole tarball in memory.
	hasher := sha256.New()
	tmp, err := os.CreateTemp(s.cacheDir, "https-*.tgz.tmp")
	if err != nil {
		return Fetched{}, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		_ = tmp.Close()
		return Fetched{}, fmt.Errorf("stream body: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Fetched{}, err
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	// Cache layout: <cacheDir>/https/sha256-<hex>/extracted/
	entryDir := filepath.Join(s.cacheDir, "https", "sha256-"+digest[len("sha256:"):])
	extractedDir := filepath.Join(entryDir, "extracted")

	if _, err := os.Stat(filepath.Join(extractedDir, metadataFile)); err == nil {
		// Cache hit on a previous run.
		meta, err := parseMetadata(extractedDir)
		if err == nil {
			return Fetched{Dir: extractedDir, ResolvedRef: digest, Metadata: meta}, nil
		}
	}

	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		return Fetched{}, fmt.Errorf("mkdir %s: %w", entryDir, err)
	}
	// Persist the original tarball for debuggability / repeat extracts.
	blobPath := filepath.Join(entryDir, "blob.tgz")
	if err := os.Rename(tmpPath, blobPath); err != nil {
		return Fetched{}, fmt.Errorf("save blob: %w", err)
	}

	if err := os.RemoveAll(extractedDir); err != nil {
		return Fetched{}, err
	}
	f, err := os.Open(blobPath)
	if err != nil {
		return Fetched{}, err
	}
	defer f.Close()
	if err := extractTarball(f, extractedDir); err != nil {
		return Fetched{}, fmt.Errorf("extract %s: %w", ref, err)
	}

	if _, err := os.Stat(filepath.Join(extractedDir, installScript)); err != nil {
		return Fetched{}, fmt.Errorf("HTTPS feature %s: missing %s after extract: %w", ref, installScript, err)
	}
	meta, err := parseMetadata(extractedDir)
	if err != nil {
		return Fetched{}, err
	}
	return Fetched{Dir: extractedDir, ResolvedRef: digest, Metadata: meta}, nil
}

// httpsDo wraps the configured client so tests can substitute a
// transport without exporting one. Real callers should not override.
func (s *DiskStore) httpsDo(req *http.Request) (*http.Response, error) {
	c := s.httpsClient
	if c == nil {
		c = httpsClient
	}
	return c.Do(req)
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
