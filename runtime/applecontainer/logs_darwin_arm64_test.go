//go:build darwin && arm64

package applecontainer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/runtime"
)

// chattyContainer runs an alpine container whose init process emits
// known output on a known cadence. Lets us assert on the log stream
// shape without poking at non-deterministic kernel/runtime chatter.
func chattyContainer(t *testing.T, id, script string) *Runtime {
	t.Helper()
	rt := runtimeOrSkip(t)
	ctx := context.Background()

	_ = rt.RemoveContainer(ctx, id, runtime.RemoveOptions{Force: true})
	t.Cleanup(func() {
		_ = rt.RemoveContainer(ctx, id, runtime.RemoveOptions{Force: true})
	})

	cliRunStrict(t,
		"run", "--rm", "--name", "ac-alpine-warmup",
		"docker.io/library/alpine:latest", "/bin/true",
	)

	if _, err := rt.RunContainer(ctx, runtime.RunSpec{
		Image: "docker.io/library/alpine:latest",
		Name:  id,
		Cmd:   []string{"/bin/sh", "-c", script},
	}); err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if err := waitForState(t, rt, id, runtime.StateRunning, 5*time.Second); err != nil {
		t.Fatalf("waitForState: %v", err)
	}
	return rt
}

// TestLogs_NonFollow asserts the non-follow path reads everything
// emitted so far and returns.
func TestLogs_NonFollow(t *testing.T) {
	const id = "ac-logs-nonfollow"
	// Emit two short lines then sleep so the log is bounded.
	rt := chattyContainer(t, id, "echo hello-from-logs-1; echo hello-from-logs-2; sleep 60")

	// Poll under a bounded timeout until both markers appear (or fail).
	// Avoids a flaky fixed sleep on slow log-flush and an unbounded
	// context.Background() on regressions.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		buf bytes.Buffer
		got string
	)
	deadline := time.Now().Add(3 * time.Second)
	for {
		buf.Reset()
		if err := rt.ContainerLogs(ctx, id, &buf, false); err != nil {
			t.Fatalf("ContainerLogs: %v", err)
		}
		got = buf.String()
		if strings.Contains(got, "hello-from-logs-1") && strings.Contains(got, "hello-from-logs-2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("logs missing markers; got %q", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestLogs_FollowBlocksUntilCancel verifies follow mode keeps reading
// past EOF until ctx fires. Runs a container that emits a marker
// every second, then cancels after observing the first marker — we
// expect ContainerLogs to return ctx.Err() promptly.
func TestLogs_FollowBlocksUntilCancel(t *testing.T) {
	const id = "ac-logs-follow"
	rt := chattyContainer(t, id,
		`i=0; while true; do echo follow-marker-$i; i=$((i+1)); sleep 1; done`,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := newPipeWriter()

	var (
		wg     sync.WaitGroup
		logErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		logErr = rt.ContainerLogs(ctx, id, pw, true)
		pw.Close()
	}()

	// Block until we see the first marker line.
	if err := pr.waitFor("follow-marker-", 8*time.Second); err != nil {
		t.Fatalf("waiting for marker: %v (collected so far=%q)", err, pr.collected())
	}

	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ContainerLogs did not return within 5s of ctx cancel")
	}

	if !errors.Is(logErr, context.Canceled) {
		t.Errorf("logs err: want context.Canceled, got %v", logErr)
	}
}

// pipeWriter is a tiny in-memory streaming sink: a goroutine can
// write to it while another waits for a substring to appear. Avoids
// the racy hand-rolled bytes.Buffer + sleep loop most tests reach for.
type pipeWriter struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  []byte
	done bool
}

func newPipeWriter() (*pipeWriter, *pipeWriter) {
	p := &pipeWriter{}
	p.cond = sync.NewCond(&p.mu)
	return p, p
}

func (p *pipeWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	p.cond.Broadcast()
	return len(b), nil
}

func (p *pipeWriter) Close() error {
	p.mu.Lock()
	p.done = true
	p.cond.Broadcast()
	p.mu.Unlock()
	return nil
}

func (p *pipeWriter) waitFor(substr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	p.mu.Lock()
	defer p.mu.Unlock()
	for !p.matchLocked(substr) {
		if p.done {
			return errors.New("writer closed before substring appeared")
		}
		now := time.Now()
		if !now.Before(deadline) {
			return errors.New("timeout waiting for substring")
		}
		// Brief wait then re-check.
		p.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		p.mu.Lock()
	}
	return nil
}

func (p *pipeWriter) matchLocked(s string) bool {
	return bytes.Contains(p.buf, []byte(s))
}

func (p *pipeWriter) collected() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.buf)
}
