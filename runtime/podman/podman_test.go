package podman

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/runtime"
)

// testRuntime wires a *Runtime's libpod client at an httptest server.
func testRuntime(t *testing.T, h http.HandlerFunc) (*Runtime, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &Runtime{lp: &libpodClient{hc: ts.Client(), baseURL: ts.URL}, checkpointOK: true}, ts
}

func TestCapabilities_GatesCheckpoint(t *testing.T) {
	if !(&Runtime{checkpointOK: true}).Capabilities().Checkpoint {
		t.Fatal("checkpointOK=true should set Capabilities().Checkpoint")
	}
	if (&Runtime{checkpointOK: false}).Capabilities().Checkpoint {
		t.Fatal("checkpointOK=false should clear Capabilities().Checkpoint")
	}
}

func TestProbeCheckpoint(t *testing.T) {
	okPing := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	badPing := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	yes := func(context.Context) bool { return true }
	no := func(context.Context) bool { return false }

	cases := []struct {
		name  string
		ping  http.HandlerFunc
		probe func(context.Context) bool
		want  bool
	}{
		{"reachable, no probe → reachability only", okPing, nil, true},
		{"reachable, probe asserts criu present", okPing, yes, true},
		{"reachable, probe reports criu missing", okPing, no, false},
		{"unreachable short-circuits before probe", badPing, yes, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(tc.ping)
			defer ts.Close()
			lp := &libpodClient{hc: ts.Client(), baseURL: ts.URL}
			if got := probeCheckpoint(context.Background(), lp, tc.probe); got != tc.want {
				t.Fatalf("probeCheckpoint = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckpoint_RequestAndArchive(t *testing.T) {
	var gotPath string
	var gotQuery map[string][]string
	rt, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte("FAKE-TAR-BYTES"))
	})

	dir := t.TempDir()
	arch := filepath.Join(dir, "ckpt.tar")
	ref, err := rt.Checkpoint(context.Background(), "c1", runtime.CheckpointSpec{ArchivePath: arch, StopAfter: true, TCPEstablished: true})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if gotPath != "/containers/c1/checkpoint" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotQuery["export"][0] != "true" || gotQuery["tcpestablished"][0] != "true" || gotQuery["leaverunning"][0] != "false" {
		t.Fatalf("query = %v", gotQuery)
	}
	b, _ := os.ReadFile(arch)
	if string(b) != "FAKE-TAR-BYTES" || ref.Size != int64(len("FAKE-TAR-BYTES")) {
		t.Fatalf("archive=%q size=%d", b, ref.Size)
	}
}

func TestCheckpoint_ErrorWrapsTyped(t *testing.T) {
	rt, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"criu boom","response":500}`))
	})
	_, err := rt.Checkpoint(context.Background(), "c1", runtime.CheckpointSpec{ArchivePath: filepath.Join(t.TempDir(), "a.tar")})
	var cfe *runtime.CheckpointFailedError
	if !errors.As(err, &cfe) || !strings.Contains(cfe.Stderr, "criu boom") {
		t.Fatalf("want CheckpointFailedError with message, got %v", err)
	}
}

func TestRestore_SendsBodyAndParsesID(t *testing.T) {
	var gotPath, gotBody string
	var gotQuery map[string][]string
	rt, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"Id":"restored-abc","runtime_restore_duration":0}`))
	})

	dir := t.TempDir()
	arch := filepath.Join(dir, "ckpt.tar")
	_ = os.WriteFile(arch, []byte("ARCHIVE"), 0o600)

	c, err := rt.Restore(context.Background(), runtime.RestoreSpec{ArchivePath: arch, Name: "ws", TCPEstablished: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if c.ID != "restored-abc" || c.State != runtime.StateRunning {
		t.Fatalf("container = %+v", c)
	}
	if gotPath != "/containers/import/restore" || gotQuery["import"][0] != "true" || gotQuery["name"][0] != "ws" {
		t.Fatalf("path=%q query=%v", gotPath, gotQuery)
	}
	if gotBody != "ARCHIVE" {
		t.Fatalf("body = %q (archive should be uploaded)", gotBody)
	}
}

func TestRestore_ErrorWrapsTyped(t *testing.T) {
	rt, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"spec.dump missing","response":500}`))
	})
	dir := t.TempDir()
	arch := filepath.Join(dir, "ckpt.tar")
	_ = os.WriteFile(arch, []byte("x"), 0o600)
	_, err := rt.Restore(context.Background(), runtime.RestoreSpec{ArchivePath: arch})
	var rfe *runtime.RestoreFailedError
	if !errors.As(err, &rfe) {
		t.Fatalf("want RestoreFailedError, got %v", err)
	}
}

func TestBuildQuery(t *testing.T) {
	q := buildQuery(runtime.BuildSpec{
		ContextPath: "/ctx", Dockerfile: "/ctx/Dockerfile", Tag: "img:1",
		Args: map[string]string{"A": "1"}, Target: "dev", NoCache: true, Platform: "linux/amd64",
	})
	if q.Get("dockerfile") != `["Dockerfile"]` {
		t.Errorf("dockerfile = %q", q.Get("dockerfile"))
	}
	if q.Get("t") != "img:1" || q.Get("buildargs") != `{"A":"1"}` || q.Get("target") != "dev" || q.Get("nocache") != "1" || q.Get("platform") != "linux/amd64" {
		t.Errorf("query = %v", q)
	}
}

func TestBuildImage_StreamsAndParsesID(t *testing.T) {
	const id = "c6f20fb73390b3ee69f99e99b7491af1214c79ab1106a1f7b117f52056eecdee"
	var gotBody []byte
	var gotDockerfile string
	rt, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		gotDockerfile = r.URL.Query().Get("dockerfile")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"stream":"STEP 1/2\n"}`+"\n")
		_, _ = io.WriteString(w, `{"stream":"Successfully tagged localhost/img:1\n"}`+"\n")
		_, _ = io.WriteString(w, `{"stream":"`+id+`\n"}`+"\n")
	})

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644)

	events := make(chan runtime.BuildEvent, 64)
	ref, err := rt.BuildImage(context.Background(), runtime.BuildSpec{ContextPath: dir, Tag: "img:1"}, events)
	close(events)
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if ref.ID != id || !reflect.DeepEqual(ref.Tags, []string{"img:1"}) {
		t.Fatalf("ref = %+v", ref)
	}
	if gotDockerfile != `["Dockerfile"]` {
		t.Fatalf("dockerfile param = %q", gotDockerfile)
	}
	if len(gotBody) == 0 {
		t.Fatalf("context tar was not uploaded as body")
	}
	var logs int
	for e := range events {
		if e.Kind == runtime.BuildEventLog {
			logs++
		}
	}
	if logs != 3 {
		t.Fatalf("expected 3 log events, got %d", logs)
	}
}

func TestParseBuildStream_Error(t *testing.T) {
	_, err := parseBuildStream(strings.NewReader(`{"stream":"step\n"}`+"\n"+`{"error":"build kaboom"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "build kaboom") {
		t.Fatalf("want build error, got %v", err)
	}
}
