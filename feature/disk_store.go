package feature

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/crunchloop/devcontainer/config"
)

// DiskStoreOptions configures DiskStore.
type DiskStoreOptions struct {
	// CacheDir is the on-disk root for cached OCI / HTTPS feature
	// artifacts. Default: os.UserCacheDir()/devcontainer-go/features.
	// Local-source features are not cached (the source path IS the cache).
	CacheDir string

	// OCIKeychain provides credentials for OCI feature pulls. Nil falls
	// back to authn.DefaultKeychain (ambient docker config / env vars /
	// credential helpers). DAP-style callers with short-lived tokens
	// supply a custom Keychain that returns fresh credentials per call.
	OCIKeychain authn.Keychain

	// HTTPSHeaders are additional headers to send on HTTPS feature
	// fetches. Useful for Authorization on private servers.
	HTTPSHeaders map[string]string

	// HTTPSClient overrides the default *http.Client used for HTTPS
	// fetches. If nil, the package's default client (5-minute timeout)
	// is used. Tests substitute this to drive httptest servers.
	HTTPSClient *http.Client
}

// DiskStore is the production Store implementation. Supports
// FeatureSourceLocal (PR6), FeatureSourceOCI (PR7), and
// FeatureSourceHTTPS (PR7).
type DiskStore struct {
	cacheDir     string
	ociKeychain  authn.Keychain
	httpsHeaders map[string]string
	httpsClient  *http.Client
}

// NewDiskStore constructs a DiskStore. The cache directory is created if
// it doesn't exist; an error is returned if it cannot be created.
func NewDiskStore(opts DiskStoreOptions) (*DiskStore, error) {
	dir := opts.CacheDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("user cache dir: %w", err)
		}
		dir = filepath.Join(base, "devcontainer-go", "features")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir %s: %w", dir, err)
	}
	kc := opts.OCIKeychain
	if kc == nil {
		kc = authn.DefaultKeychain
	}
	return &DiskStore{
		cacheDir:     dir,
		ociKeychain:  kc,
		httpsHeaders: opts.HTTPSHeaders,
		httpsClient:  opts.HTTPSClient,
	}, nil
}

// CacheDir returns the on-disk cache root. Useful for debugging and
// for the `rm -rf $cache` recovery path documented in design §8.
func (s *DiskStore) CacheDir() string { return s.cacheDir }

// Fetch resolves a feature reference per its source kind.
func (s *DiskStore) Fetch(ctx context.Context, ref string, kind config.FeatureSourceKind) (Fetched, error) {
	switch kind {
	case config.FeatureSourceLocal:
		return fetchLocal(ref)
	case config.FeatureSourceOCI:
		return s.fetchOCI(ctx, ref)
	case config.FeatureSourceHTTPS:
		return s.fetchHTTPS(ctx, ref)
	default:
		return Fetched{}, fmt.Errorf("unknown source kind: %s", kind)
	}
}

// fetchLocal validates and parses a local-path feature. The caller
// must have already resolved relative paths to absolute via
// filepath.Join(configDir, ref).
func fetchLocal(absPath string) (Fetched, error) {
	if !filepath.IsAbs(absPath) {
		return Fetched{}, fmt.Errorf("local feature path must be absolute: %s", absPath)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return Fetched{}, fmt.Errorf("local feature %s: %w", absPath, err)
	}
	if !info.IsDir() {
		return Fetched{}, fmt.Errorf("local feature %s: not a directory", absPath)
	}

	if _, err := os.Stat(filepath.Join(absPath, installScript)); err != nil {
		return Fetched{}, fmt.Errorf("local feature %s: missing %s: %w", absPath, installScript, err)
	}

	meta, err := parseMetadata(absPath)
	if err != nil {
		return Fetched{}, err
	}

	return Fetched{
		Dir:         absPath,
		ResolvedRef: absPath,
		Metadata:    meta,
	}, nil
}

// ResolveLocalRef converts a (possibly relative) local feature ref to
// an absolute path. Used by callers (the engine) before invoking
// Store.Fetch with FeatureSourceLocal. configDir is typically
// filepath.Dir(ResolveOptions.ConfigPath).
func ResolveLocalRef(configDir, ref string) string {
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}
	return filepath.Clean(filepath.Join(configDir, ref))
}
