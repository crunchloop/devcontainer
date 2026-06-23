package compose

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/crunchloop/devcontainer/runtime"
)

func TestHealthProbeCmd(t *testing.T) {
	tests := []struct {
		name string
		hc   *runtime.HealthCheckSpec
		want []string
	}{
		{"nil", nil, nil},
		{"disabled", &runtime.HealthCheckSpec{Test: []string{"CMD", "true"}, Disable: true}, nil},
		{"empty", &runtime.HealthCheckSpec{Test: nil}, nil},
		{"none", &runtime.HealthCheckSpec{Test: []string{"NONE"}}, nil},
		{"cmd", &runtime.HealthCheckSpec{Test: []string{"CMD", "rabbitmq-diagnostics", "-q", "ping"}}, []string{"rabbitmq-diagnostics", "-q", "ping"}},
		{"cmd-no-args", &runtime.HealthCheckSpec{Test: []string{"CMD"}}, nil},
		{"cmd-shell", &runtime.HealthCheckSpec{Test: []string{"CMD-SHELL", "pg_isready -U postgres"}}, []string{"/bin/sh", "-c", "pg_isready -U postgres"}},
		{"bare-string-defensive", &runtime.HealthCheckSpec{Test: []string{"curl", "-f", "localhost"}}, []string{"/bin/sh", "-c", "curl -f localhost"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := healthProbeCmd(tt.hc)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("healthProbeCmd(%v) = %v, want %v", tt.hc, got, tt.want)
			}
		})
	}
}

// selfProbingMock is a mockRuntime that opts into orchestrator-driven
// health probing, exercising the selfHealthProber detection.
type selfProbingMock struct{ *mockRuntime }

func (selfProbingMock) PreferSelfProbedHealth() bool { return true }

func TestNewOrchestratorDetectsSelfProbe(t *testing.T) {
	if NewOrchestrator(newMockRuntime(), "docker").selfProbe {
		t.Error("docker mock (no selfHealthProber) should not self-probe")
	}
	if !NewOrchestrator(selfProbingMock{newMockRuntime()}, "podman").selfProbe {
		t.Error("podman-like mock should self-probe")
	}
}

func TestWaitForSelfProbeHealthy(t *testing.T) {
	rt := newMockRuntime()
	var execed int
	rt.OnExec = func(id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
		execed++
		// Succeed only on the 2nd probe — proves we poll the command,
		// not the native health status.
		if execed >= 2 {
			return runtime.ExecResult{ExitCode: 0}, nil
		}
		return runtime.ExecResult{ExitCode: 1}, nil
	}
	orch := NewOrchestrator(selfProbingMock{rt}, "podman")
	orch.PollInterval = time.Millisecond

	hc := &runtime.HealthCheckSpec{Test: []string{"CMD", "rabbitmq-diagnostics", "-q", "ping"}}
	err := orch.waitFor(context.Background(), "rabbitmq", "c1", "service_healthy", hc, time.Now().Add(2*time.Second))
	if err != nil {
		t.Fatalf("waitFor = %v, want nil (probe should pass on 2nd try)", err)
	}
	if execed < 2 {
		t.Fatalf("expected >= 2 probe execs, got %d", execed)
	}
}

func TestWaitForSelfProbeTimeout(t *testing.T) {
	rt := newMockRuntime()
	rt.OnExec = func(id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
		return runtime.ExecResult{ExitCode: 1}, nil // never healthy
	}
	orch := NewOrchestrator(selfProbingMock{rt}, "podman")
	orch.PollInterval = time.Millisecond
	orch.HealthTimeout = 30 * time.Millisecond

	hc := &runtime.HealthCheckSpec{Test: []string{"CMD", "false"}}
	err := orch.waitFor(context.Background(), "svc", "c1", "service_healthy", hc, time.Now().Add(30*time.Millisecond))
	var hte *HealthTimeoutError
	if !errors.As(err, &hte) {
		t.Fatalf("waitFor = %v, want *HealthTimeoutError", err)
	}
}

// A service with no/NONE healthcheck, self-probe mode: gate is satisfied
// by the container merely running (mirrors native HealthNone behavior).
func TestWaitForSelfProbeNoHealthcheckRunning(t *testing.T) {
	rt := newMockRuntime()
	c, err := rt.RunContainer(context.Background(), runtime.RunSpec{Name: "svc"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.StartContainer(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	rt.OnExec = func(id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
		t.Fatal("should not exec a probe when there is no healthcheck")
		return runtime.ExecResult{}, nil
	}
	orch := NewOrchestrator(selfProbingMock{rt}, "podman")
	orch.PollInterval = time.Millisecond

	err = orch.waitFor(context.Background(), "svc", c.ID, "service_healthy", nil, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("waitFor = %v, want nil (running container, no healthcheck)", err)
	}
}
