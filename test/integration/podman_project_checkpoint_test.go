//go:build integration && linux

// Multi-service project checkpoint/restore through the engine orchestrator
// (Engine.CheckpointProject / RestoreProject) against a live Podman.
//
// Codifies the Phase-0 multi-service spike: two containers sharing a
// compose-project label on a user-defined network — a TCP "server" and a
// "client" that connects to it by name every second and counts successful
// round-trips into /count.txt. We checkpoint the whole project, remove
// both, restore the whole project, and assert (a) every service comes
// back, (b) the devcontainer service reattaches as the Primary workspace
// with its id recovered from the preserved label, and (c) the client's
// counter resumes climbing — proving memory resumed AND the inter-service
// link (service-name DNS over the shared network) re-formed.
//
// Linux-only (Podman); skipped unless PODMAN_SOCKET is set:
//
//	PODMAN_SOCKET=unix:///run/podman/podman.sock \
//	  go test -tags integration -run PodmanProject -count=1 ./test/integration
package integration

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	devcontainer "github.com/crunchloop/devcontainer"
	"github.com/crunchloop/devcontainer/compose"
	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/podman"
)

func TestPodmanProject_CheckpointRestore_MultiService(t *testing.T) {
	if testing.Short() {
		t.Skip("integration tests skipped with -short")
	}
	image := imageOrDefault()

	eng, rt := newPodmanEngine(t)

	const (
		project   = "dcckpt-itest"
		netName   = "dcckpt-itest-net"
		serverNm  = "dcckptserver"
		clientNm  = "dcckptclient"
		clientWID = "ws-dcckpt-client"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	if _, err := rt.PullImage(ctx, image, nil); err != nil {
		t.Fatalf("PullImage: %v", err)
	}

	// Fresh network for the project (idempotent across reruns).
	if _, err := rt.CreateNetwork(ctx, runtime.NetworkSpec{
		Name:   netName,
		Labels: map[string]string{compose.LabelComposeProject: project},
	}); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	t.Cleanup(func() { _ = rt.RemoveNetwork(context.Background(), netName) })

	// server: a trivial TCP server on :9000.
	runService(t, ctx, rt, runtime.RunSpec{
		Image:           image,
		Name:            serverNm,
		Networks:        []string{netName},
		OverrideCommand: false,
		Cmd:             []string{"node", "-e", "require('net').createServer(s=>s.end('ok')).listen(9000,()=>console.log('listening'))"},
		Labels: map[string]string{
			compose.LabelComposeProject: project,
			compose.LabelComposeService: "server",
		},
	})

	// client: connect to the server by name each second, count successful
	// round-trips into /count.txt.
	clientScript := "const net=require('net'),fs=require('fs');let n=0;" +
		"setInterval(()=>{const s=net.connect(9000,'" + serverNm + "');" +
		"s.on('connect',()=>{n++;try{fs.writeFileSync('/count.txt',String(n))}catch(e){}s.end()});" +
		"s.on('error',()=>{})},1000);"
	client := runService(t, ctx, rt, runtime.RunSpec{
		Image:           image,
		Name:            clientNm,
		Networks:        []string{netName},
		OverrideCommand: false,
		Cmd:             []string{"node", "-e", clientScript},
		Labels: map[string]string{
			compose.LabelComposeProject:            project,
			compose.LabelComposeService:            "client",
			devcontainer.LabelDevcontainerID:       clientWID,
			devcontainer.LabelLocalWorkspaceFolder: "/work",
		},
	})

	// Let the link establish.
	time.Sleep(8 * time.Second)
	before := readCount(t, ctx, rt, client.ID)
	if before <= 0 {
		t.Fatalf("client counter not advancing before checkpoint (%d) — link never formed", before)
	}

	// Build the anchor workspace from the client (the devcontainer service).
	details, err := rt.InspectContainer(ctx, client.ID)
	if err != nil {
		t.Fatalf("InspectContainer(client): %v", err)
	}
	ws := &devcontainer.Workspace{Container: details}

	dir := t.TempDir()
	ref, err := eng.CheckpointProject(ctx, ws, devcontainer.ProjectCheckpointOptions{
		ArchiveDir: dir, StopAfter: true, TCPEstablished: true,
	})
	if err != nil {
		t.Fatalf("CheckpointProject: %v", err)
	}
	if len(ref.Services) != 2 {
		t.Fatalf("checkpointed %d services, want 2 (%+v)", len(ref.Services), ref.Services)
	}

	// Migration shape: both sources gone. Network stays so restore can
	// re-attach to it.
	for _, nm := range []string{clientNm, serverNm} {
		if err := rt.RemoveContainer(ctx, nm, runtime.RemoveOptions{Force: true}); err != nil {
			t.Fatalf("RemoveContainer(%s): %v", nm, err)
		}
	}
	time.Sleep(2 * time.Second)

	pr, err := eng.RestoreProject(ctx, devcontainer.ProjectRestoreOptions{ArchiveDir: dir, TCPEstablished: true})
	if err != nil {
		t.Fatalf("RestoreProject: %v", err)
	}
	t.Cleanup(func() {
		for _, c := range pr.Services {
			if c != nil {
				_ = rt.RemoveContainer(context.Background(), c.ID, runtime.RemoveOptions{Force: true})
			}
		}
	})

	if pr.Services["server"] == nil || pr.Services["client"] == nil {
		t.Fatalf("restored services = %v, want server+client", pr.Services)
	}
	if pr.Primary == nil || pr.Primary.ID != clientWID {
		t.Fatalf("Primary = %+v, want reattached workspace id %q", pr.Primary, clientWID)
	}

	// Link + memory resumed: the counter climbs past its pre-checkpoint value.
	time.Sleep(6 * time.Second)
	after := readCount(t, ctx, rt, pr.Services["client"].ID)
	if after <= before {
		t.Fatalf("counter did not resume/relink: before=%d after=%d (cold restart or broken DNS)", before, after)
	}
	t.Logf("multi-service C/R OK: before=%d after=%d primary=%s services=%d", before, after, pr.Primary.ID, len(pr.Services))
}

func imageOrDefault() string {
	// Reuse the runtime-level test's override knob.
	if v := os.Getenv("PODMAN_TEST_IMAGE"); v != "" {
		return v
	}
	return "docker.io/library/node:20-slim"
}

func runService(t *testing.T, ctx context.Context, rt *podman.Runtime, spec runtime.RunSpec) *runtime.Container {
	t.Helper()
	_ = rt.RemoveContainer(ctx, spec.Name, runtime.RemoveOptions{Force: true})
	c, err := rt.RunContainer(ctx, spec)
	if err != nil {
		t.Fatalf("RunContainer(%s): %v", spec.Name, err)
	}
	t.Cleanup(func() { _ = rt.RemoveContainer(context.Background(), spec.Name, runtime.RemoveOptions{Force: true}) })
	if err := rt.StartContainer(ctx, c.ID); err != nil {
		t.Fatalf("StartContainer(%s): %v", spec.Name, err)
	}
	return c
}

func readCount(t *testing.T, ctx context.Context, rt *podman.Runtime, id string) int {
	t.Helper()
	res, err := rt.ExecContainer(ctx, id, runtime.ExecOptions{Cmd: []string{"cat", "/count.txt"}})
	if err != nil || res.ExitCode != 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		return 0
	}
	return n
}
