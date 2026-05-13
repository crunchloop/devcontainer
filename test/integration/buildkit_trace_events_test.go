//go:build integration

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/events"
)

// TestBuildKit_PerVertexEventsEmitted asserts that BuildKit's
// per-vertex progress (moby.buildkit.trace aux records) is decoded and
// surfaced as BuildLayerEvent + BuildLogEvent for the dockerfile build.
//
// Regression guard for runtime/docker/buildkit_trace.go: if the aux
// decoder is removed or streamBuildOutput stops routing aux records to
// it, this test goes silent — zero dockerfile layer events and the RUN
// marker never appears in the log stream.
//
// The Dockerfile embeds a unique per-run marker in the RUN step so the
// build can't satisfy that vertex from cache; otherwise the test would
// still pass via "CACHED" layer events but the log assertion would
// flake based on prior runs.
func TestBuildKit_PerVertexEventsEmitted(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	eng, rt := newEngine(t)
	defer rt.Close()

	dir := t.TempDir()
	marker := fmt.Sprintf("BUILDKIT_TRACE_PROBE_%d", time.Now().UnixNano())
	mustWrite(t, filepath.Join(dir, "Dockerfile"), fmt.Sprintf(`
FROM alpine:3.20
RUN echo %s
`, marker))
	mustWrite(t, filepath.Join(dir, ".devcontainer", "devcontainer.json"), `{
		"build": { "dockerfile": "Dockerfile", "context": ".." }
	}`)

	evCh := make(chan events.Event, 1024)
	var (
		mu              sync.Mutex
		dockerfileLayer int
		dockerfileLogs  []string
	)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for ev := range evCh {
			switch e := ev.(type) {
			case events.BuildLayerEvent:
				if e.Source == events.BuildSourceDockerfile {
					mu.Lock()
					dockerfileLayer++
					mu.Unlock()
				}
			case events.BuildLogEvent:
				if e.Source == events.BuildSourceDockerfile {
					mu.Lock()
					dockerfileLogs = append(dockerfileLogs, e.Line)
					mu.Unlock()
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	wsObj, err := eng.Up(ctx, devcontainer.UpOptions{
		LocalWorkspaceFolder: dir,
		Recreate:             true,
		SkipLifecycle:        true,
		Events:               evCh,
	})
	// Per UpOptions.Events doc: close only after Up returns.
	close(evCh)
	<-consumerDone
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() { _ = eng.Down(context.Background(), wsObj, devcontainer.DownOptions{Remove: true}) }()

	mu.Lock()
	gotLayer := dockerfileLayer
	gotLogs := append([]string(nil), dockerfileLogs...)
	mu.Unlock()

	// A 2-step BuildKit graph emits multiple vertex transitions (resolve
	// image config, FROM, internal load-build-context, RUN — start +
	// done per vertex). 2 is a conservative floor that proves the
	// decoder is firing at all.
	if gotLayer < 2 {
		t.Errorf("expected >=2 BuildLayerEvent from dockerfile build, got %d (aux decoder dead?)", gotLayer)
	}

	var sawMarker bool
	for _, line := range gotLogs {
		if strings.Contains(line, marker) {
			sawMarker = true
			break
		}
	}
	if !sawMarker {
		t.Errorf("RUN marker %q missing from %d log events (VertexLog decoder dead?): %v",
			marker, len(gotLogs), gotLogs)
	}
}
