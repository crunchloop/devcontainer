//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/events"
	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/docker"
)

// TestBuildSource_DockerfileOutsideWorkspaceRefused: a hostile
// devcontainer.json names a secret-looking host file outside the
// workspace as its Dockerfile. Sent to the daemon, its first line
// would come back in the parse error ("unknown instruction: …") and
// end up in the build output. The build must fail before the daemon
// builds or pulls anything, and the secret must appear in no event and
// no error.
//
// The absolute path is caught while resolving the config; the symlink
// passes that lexical check and is caught by the runtime after
// resolving symlinks.
func TestBuildSource_DockerfileOutsideWorkspaceRefused(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	secret := fmt.Sprintf("DCGO-SECRET-%d", time.Now().UnixNano())
	secretFile := filepath.Join(t.TempDir(), "credentials")
	mustWrite(t, secretFile, secret+"\n")

	cases := []struct {
		name    string
		setup   func(t *testing.T, ws string) string // returns build.dockerfile
		wantErr func(error) bool
	}{
		{
			name:  "absolute path",
			setup: func(t *testing.T, ws string) string { return secretFile },
			wantErr: func(err error) bool {
				var invalid *config.ConfigInvalidError
				return errors.As(err, &invalid)
			},
		},
		{
			name: "symlink in the workspace",
			setup: func(t *testing.T, ws string) string {
				if err := os.Symlink(secretFile, filepath.Join(ws, ".devcontainer", "Dockerfile")); err != nil {
					t.Fatal(err)
				}
				return "Dockerfile"
			},
			wantErr: func(err error) bool {
				var refused *runtime.DockerfileRefusedError
				return errors.As(err, &refused)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, rt, requests := newRecordingEngine(t)
			defer rt.Close()

			ws := t.TempDir()
			mustWrite(t, filepath.Join(ws, "app", "app.txt"), "app\n")
			if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o755); err != nil {
				t.Fatal(err)
			}
			dockerfile := tc.setup(t, ws)
			mustWrite(t, filepath.Join(ws, ".devcontainer", "devcontainer.json"),
				fmt.Sprintf(`{"build": {"dockerfile": %q, "context": "../app"}}`, dockerfile))

			evCh := make(chan events.Event, 1024)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			const tag = "dc-go-build-test-refused:v1"
			_, err := eng.Build(ctx, devcontainer.BuildOptions{
				LocalWorkspaceFolder: ws,
				ImageName:            tag,
				Events:               evCh,
			})
			t.Cleanup(func() { _ = rt.RemoveImage(context.Background(), tag) })

			if err == nil {
				t.Fatal("Build succeeded, want refusal")
			}
			if !tc.wantErr(err) {
				t.Errorf("err = %v (%T), want the typed refusal", err, err)
			}
			if !strings.Contains(err.Error(), dockerfile) {
				t.Errorf("error %q does not name the configured path %q", err, dockerfile)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the secret: %q", err)
			}

			close(evCh)
			for ev := range evCh {
				if s := fmt.Sprintf("%#v", ev); strings.Contains(s, secret) {
					t.Errorf("event leaks the secret: %s", s)
				}
			}

			if len(requests()) == 0 {
				t.Fatal("proxy recorded no requests; the recorder is not in the path")
			}
			for _, req := range requests() {
				if strings.HasSuffix(req, "/build") || strings.Contains(req, "/images/create") {
					t.Errorf("daemon reached before the refusal: %s", req)
				}
			}
		})
	}
}

// newRecordingEngine is newEngine with the runtime talking to the
// daemon through a proxy that records each request's method and path,
// so a test can assert what never reached the daemon.
func newRecordingEngine(t *testing.T) (*devcontainer.Engine, *docker.Runtime, func() []string) {
	t.Helper()
	daemon := "/var/run/docker.sock"
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		if !strings.HasPrefix(host, "unix://") {
			t.Skipf("recording proxy needs a unix DOCKER_HOST, have %q", host)
		}
		daemon = strings.TrimPrefix(host, "unix://")
	}

	// Short path: unix socket paths are limited to ~104 bytes.
	dir, err := os.MkdirTemp("", "dcgo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu   sync.Mutex
		seen []string
	)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = "docker"
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", daemon)
			},
		},
		FlushInterval: -1,
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt, err := docker.New(ctx, docker.Options{Host: "unix://" + sock})
	if err != nil {
		t.Skipf("Docker daemon unavailable through proxy: %v", err)
	}
	eng, err := devcontainer.New(devcontainer.EngineOptions{Runtime: rt})
	if err != nil {
		_ = rt.Close()
		t.Fatalf("New: %v", err)
	}
	return eng, rt, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}
