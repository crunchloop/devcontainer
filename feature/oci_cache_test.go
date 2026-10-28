package feature

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/crunchloop/devcontainer/config"
)

// Tests for the OCI cache policy documented on fetchOCI: digest refs
// and exact version tags are local-first, floating tags are online
// first with an offline fallback to the recorded resolution, and a
// registry that answers with an error is authoritative.

// registryMode selects what gatedTransport does with a request.
type registryMode int32

const (
	modeOnline      registryMode = iota // pass through
	modeUnreachable                     // fail like a refused connection
	modeDenied                          // answer 403 for everything
)

// gatedTransport counts registry calls and can make the registry
// unreachable or have it refuse every request.
type gatedTransport struct {
	mode  atomic.Int32
	calls atomic.Int64
}

func (g *gatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	g.calls.Add(1)
	switch registryMode(g.mode.Load()) {
	case modeUnreachable:
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused (test)")}
	case modeDenied:
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Status:     "403 Forbidden",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"errors":[{"code":"DENIED","message":"denied (test)"}]}`)),
			Request:    req,
		}, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}

type cacheFixture struct {
	host  string
	gate  *gatedTransport
	store *DiskStore
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	gate := &gatedTransport{}
	store, err := NewDiskStore(DiskStoreOptions{
		CacheDir:     t.TempDir(),
		OCIKeychain:  authn.NewMultiKeychain(),
		OCITransport: gate,
	})
	if err != nil {
		t.Fatalf("NewDiskStore: %v", err)
	}
	return &cacheFixture{host: strings.TrimPrefix(srv.URL, "http://"), gate: gate, store: store}
}

func (c *cacheFixture) ref(tag string) string { return c.host + "/features/demo:" + tag }

// push publishes a feature artifact whose install.sh echoes marker
// under tag, and returns its manifest digest.
func (c *cacheFixture) push(t *testing.T, tag, marker string) v1.Hash {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{
		{"devcontainer-feature.json", `{"id":"demo","version":"1.2.3"}`},
		{"install.sh", "#!/bin/sh\necho " + marker + "\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(
		mutate.ConfigMediaType(empty.Image, types.MediaType(devContainerFeatureMediaType)),
		static.NewLayer(buf.Bytes(), "application/vnd.devcontainers.layer.v1+tar"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(c.ref(tag))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, remote.WithTransport(http.DefaultTransport)); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (c *cacheFixture) fetch(t *testing.T, ref string) (Fetched, error) {
	t.Helper()
	return c.store.Fetch(context.Background(), ref, config.FeatureSourceOCI)
}

func (c *cacheFixture) mustFetch(t *testing.T, ref string) Fetched {
	t.Helper()
	f, err := c.fetch(t, ref)
	if err != nil {
		t.Fatalf("Fetch %s: %v", ref, err)
	}
	return f
}

func (c *cacheFixture) set(m registryMode) {
	c.gate.mode.Store(int32(m))
	c.gate.calls.Store(0)
}

func wantDigest(t *testing.T, f Fetched, d v1.Hash) {
	t.Helper()
	if !strings.HasSuffix(f.ResolvedRef, "@"+d.String()) {
		t.Fatalf("ResolvedRef = %s, want digest %s", f.ResolvedRef, d)
	}
}

func TestOCICache_DigestRefIsLocalFirst(t *testing.T) {
	c := newCacheFixture(t)
	d := c.push(t, "1.2.3", "v1")
	c.mustFetch(t, c.ref("1.2.3"))

	c.set(modeUnreachable)
	f := c.mustFetch(t, c.host+"/features/demo@"+d.String())
	wantDigest(t, f, d)
	if n := c.gate.calls.Load(); n != 0 {
		t.Fatalf("digest ref with cached content made %d registry calls, want 0", n)
	}
}

func TestOCICache_DigestRefNotCachedGoesOnline(t *testing.T) {
	c := newCacheFixture(t)
	d := c.push(t, "1.2.3", "v1")
	f := c.mustFetch(t, c.host+"/features/demo@"+d.String())
	wantDigest(t, f, d)
	if c.gate.calls.Load() == 0 {
		t.Fatal("uncached digest ref made no registry call")
	}
}

func TestOCICache_ExactVersionTagIsLocalFirst(t *testing.T) {
	c := newCacheFixture(t)
	d := c.push(t, "1.2.3", "v1")
	wantDigest(t, c.mustFetch(t, c.ref("1.2.3")), d)

	// Online, but the recorded resolution of an exact version is used
	// without asking the registry.
	c.set(modeOnline)
	wantDigest(t, c.mustFetch(t, c.ref("1.2.3")), d)
	if n := c.gate.calls.Load(); n != 0 {
		t.Fatalf("cached exact version made %d registry calls, want 0", n)
	}

	c.set(modeUnreachable)
	wantDigest(t, c.mustFetch(t, c.ref("1.2.3")), d)
}

func TestOCICache_FloatingTagIsOnlineFirst(t *testing.T) {
	c := newCacheFixture(t)
	d1 := c.push(t, "1", "v1")
	wantDigest(t, c.mustFetch(t, c.ref("1")), d1)

	d2 := c.push(t, "1", "v2")
	if d1 == d2 {
		t.Fatal("test artifacts share a digest")
	}
	c.set(modeOnline)
	wantDigest(t, c.mustFetch(t, c.ref("1")), d2)
	if c.gate.calls.Load() == 0 {
		t.Fatal("floating tag resolved without asking the registry")
	}
}

func TestOCICache_FloatingTagOfflineUsesRecordedResolution(t *testing.T) {
	c := newCacheFixture(t)
	d := c.push(t, "latest", "v1")
	wantDigest(t, c.mustFetch(t, c.ref("latest")), d)

	c.set(modeUnreachable)
	f := c.mustFetch(t, c.ref("latest"))
	wantDigest(t, f, d)
	if f.Metadata.ID != "demo" {
		t.Fatalf("Metadata.ID = %q, want demo", f.Metadata.ID)
	}
}

func TestOCICache_OfflineWithoutRecordFails(t *testing.T) {
	c := newCacheFixture(t)
	c.push(t, "1", "v1")
	c.push(t, "1.2.3", "v1")

	c.set(modeUnreachable)
	for _, tag := range []string{"1", "1.2.3"} {
		if _, err := c.fetch(t, c.ref(tag)); err == nil {
			t.Fatalf("offline fetch of never-resolved %s succeeded", tag)
		}
	}
}

func TestOCICache_RegistryErrorIsAuthoritative(t *testing.T) {
	c := newCacheFixture(t)
	c.push(t, "1", "v1")
	c.mustFetch(t, c.ref("1"))

	// The registry answers, with a refusal: that is not an offline
	// condition, so the recorded resolution must not paper over it.
	c.set(modeDenied)
	if _, err := c.fetch(t, c.ref("1")); err == nil {
		t.Fatal("fetch succeeded although the registry refused it")
	}
}

func TestOCICache_RecordNeedsContent(t *testing.T) {
	c := newCacheFixture(t)
	d := c.push(t, "1.2.3", "v1")
	f := c.mustFetch(t, c.ref("1.2.3"))
	wantDigest(t, f, d)

	// Evict the content but keep the tag record: the local-first path
	// must fall through to the registry instead of returning a
	// dangling directory.
	if err := os.RemoveAll(c.store.ociEntryDir(d.Hex)); err != nil {
		t.Fatal(err)
	}
	c.set(modeOnline)
	wantDigest(t, c.mustFetch(t, c.ref("1.2.3")), d)
	if c.gate.calls.Load() == 0 {
		t.Fatal("evicted content was not re-fetched")
	}
}

func TestIsExactVersionTag(t *testing.T) {
	for tag, want := range map[string]bool{
		"2.17.0":      true,
		"10.0.12":     true,
		"2":           false,
		"2.17":        false,
		"latest":      false,
		"2.17.0-rc.1": false,
		"v2.17.0":     false,
		"dev":         false,
	} {
		if got := isExactVersionTag(tag); got != want {
			t.Errorf("isExactVersionTag(%q) = %v, want %v", tag, got, want)
		}
	}
}
