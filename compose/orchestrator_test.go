package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/crunchloop/devcontainer/runtime"
)

// mockRuntime is a purpose-built fake for orchestrator tests. It
// records every primitive call, lets tests inject state changes
// (container exit codes, label-stored hashes), and supports
// concurrent access from the orchestrator's within-level parallel
// service starts.
//
// Capabilities default to docker-baseline (all true). Override via
// the Caps field per test.
type mockRuntime struct {
	mu sync.Mutex

	Caps runtime.Capabilities

	// Resources
	networks map[string]map[string]string // name -> labels
	volumes  map[string]map[string]string // name -> labels
	containers map[string]*mockContainer  // id -> container

	// Call log for assertions
	createNetworkCalls int
	createVolumeCalls  int
	runCalls           int
	startCalls         int
	stopCalls          int
	removeCalls        int

	// Hooks (set to override default behavior).
	OnRunContainer  func(spec runtime.RunSpec) (*runtime.Container, error)
	OnInspect       func(id string, base *runtime.ContainerDetails) *runtime.ContainerDetails
}

type mockContainer struct {
	id     string
	name   string
	image  string
	labels map[string]string
	state  runtime.State
	exit   int
}

func newMockRuntime() *mockRuntime {
	return &mockRuntime{
		Caps:       runtime.Capabilities{Healthchecks: true, ExitCodes: true, NamespaceSharing: true, RestartPolicies: true, SharedVolumes: true},
		networks:   map[string]map[string]string{},
		volumes:    map[string]map[string]string{},
		containers: map[string]*mockContainer{},
	}
}

// ---- runtime.Runtime ------------------------------------------------

func (m *mockRuntime) BuildImage(ctx context.Context, spec runtime.BuildSpec, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	return runtime.ImageRef{}, runtime.ErrNotImplemented
}
func (m *mockRuntime) PullImage(ctx context.Context, ref string, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	return runtime.ImageRef{ID: "sha256:" + ref, Tags: []string{ref}}, nil
}

func (m *mockRuntime) RunContainer(ctx context.Context, spec runtime.RunSpec) (*runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runCalls++
	if m.OnRunContainer != nil {
		c, err := m.OnRunContainer(spec)
		if err != nil {
			return nil, err
		}
		if c != nil {
			m.containers[c.ID] = &mockContainer{
				id: c.ID, name: c.Name, image: c.Image,
				labels: copyLabels(spec.Labels),
				state:  runtime.StateCreated,
			}
			return c, nil
		}
	}
	id := fmt.Sprintf("c-%d-%s", len(m.containers)+1, spec.Name)
	m.containers[id] = &mockContainer{
		id: id, name: spec.Name, image: spec.Image,
		labels: copyLabels(spec.Labels),
		state:  runtime.StateCreated,
	}
	return &runtime.Container{ID: id, Name: spec.Name, Image: spec.Image, State: runtime.StateCreated}, nil
}

func (m *mockRuntime) StartContainer(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startCalls++
	c, ok := m.containers[id]
	if !ok {
		return &runtime.ContainerNotFoundError{ID: id}
	}
	c.state = runtime.StateRunning
	return nil
}

func (m *mockRuntime) StopContainer(ctx context.Context, id string, opts runtime.StopOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopCalls++
	c, ok := m.containers[id]
	if !ok {
		return nil
	}
	c.state = runtime.StateExited
	return nil
}

func (m *mockRuntime) RemoveContainer(ctx context.Context, id string, opts runtime.RemoveOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeCalls++
	delete(m.containers, id)
	return nil
}

func (m *mockRuntime) ExecContainer(ctx context.Context, id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
	return runtime.ExecResult{}, nil
}

func (m *mockRuntime) InspectContainer(ctx context.Context, id string) (*runtime.ContainerDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.containers[id]
	if !ok {
		return nil, &runtime.ContainerNotFoundError{ID: id}
	}
	d := &runtime.ContainerDetails{
		Container: runtime.Container{ID: c.id, Name: c.name, Image: c.image, State: c.state},
		Labels:    copyLabels(c.labels),
		ExitCode:  c.exit,
	}
	if m.OnInspect != nil {
		if got := m.OnInspect(id, d); got != nil {
			return got, nil
		}
	}
	return d, nil
}

func (m *mockRuntime) InspectImage(ctx context.Context, ref string) (*runtime.ImageDetails, error) {
	return &runtime.ImageDetails{ID: "sha256:" + ref, Tags: []string{ref}}, nil
}

func (m *mockRuntime) ContainerLogs(ctx context.Context, id string, w io.Writer, follow bool) error {
	return nil
}

func (m *mockRuntime) FindContainerByLabel(ctx context.Context, key, value string) (*runtime.Container, error) {
	return nil, nil
}

func (m *mockRuntime) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createNetworkCalls++
	if existing, ok := m.networks[spec.Name]; ok {
		if labelsSuperset(existing, spec.Labels) {
			return "net-" + spec.Name, nil
		}
	}
	m.networks[spec.Name] = copyLabels(spec.Labels)
	return "net-" + spec.Name, nil
}

func (m *mockRuntime) RemoveNetwork(ctx context.Context, id string) error {
	return nil
}

func (m *mockRuntime) CreateVolume(ctx context.Context, spec runtime.VolumeSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createVolumeCalls++
	m.volumes[spec.Name] = copyLabels(spec.Labels)
	return spec.Name, nil
}

func (m *mockRuntime) RemoveVolume(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.volumes, name)
	return nil
}

func (m *mockRuntime) ListContainers(ctx context.Context, filter runtime.LabelFilter) ([]runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []runtime.Container
	for _, c := range m.containers {
		if labelsSuperset(c.labels, filter.Match) {
			out = append(out, runtime.Container{ID: c.id, Name: c.name, Image: c.image, State: c.state})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *mockRuntime) ListImages(ctx context.Context, filter runtime.LabelFilter) ([]runtime.ImageRef, error) {
	return nil, nil
}

func (m *mockRuntime) RemoveImage(ctx context.Context, ref string) error {
	return nil
}

func (m *mockRuntime) Capabilities() runtime.Capabilities {
	return m.Caps
}

// labelsSuperset is the same predicate as docker's labelsMatch, kept
// local so test mocks don't import runtime/docker.
func labelsSuperset(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ---- tests ----------------------------------------------------------

func newProject(t *testing.T, deps map[string][]string) *composetypes.Project {
	t.Helper()
	svcs := composetypes.Services{}
	for name, ds := range deps {
		svc := composetypes.ServiceConfig{Name: name, Image: "alpine"}
		if len(ds) > 0 {
			svc.DependsOn = composetypes.DependsOnConfig{}
			for _, d := range ds {
				svc.DependsOn[d] = composetypes.ServiceDependency{Condition: "service_started"}
			}
		}
		svcs[name] = svc
	}
	return &composetypes.Project{Services: svcs}
}

func TestUp_SingleService(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt, "docker")
	proj := newProject(t, map[string][]string{"app": nil})

	res, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(res.ContainerIDs) != 1 || res.ContainerIDs["app"] == "" {
		t.Errorf("ContainerIDs = %+v", res.ContainerIDs)
	}
	if rt.createNetworkCalls != 1 {
		t.Errorf("createNetworkCalls = %d, want 1", rt.createNetworkCalls)
	}
	if rt.runCalls != 1 || rt.startCalls != 1 {
		t.Errorf("run=%d start=%d, want 1/1", rt.runCalls, rt.startCalls)
	}
}

func TestUp_DependencyOrder(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt, "docker")
	proj := newProject(t, map[string][]string{
		"db":  nil,
		"api": {"db"},
		"app": {"api"},
	})

	// Record the order RunContainer is called in.
	var order []string
	var mu sync.Mutex
	rt.OnRunContainer = func(spec runtime.RunSpec) (*runtime.Container, error) {
		mu.Lock()
		order = append(order, spec.Labels[LabelComposeService])
		mu.Unlock()
		return nil, nil // fall back to default creation
	}

	if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	want := []string{"db", "api", "app"}
	for i, name := range want {
		if order[i] != name {
			t.Errorf("order[%d] = %q, want %q (full=%v)", i, order[i], name, order)
		}
	}
}

func TestUp_IdempotentReuseOnHashMatch(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt, "docker")
	proj := newProject(t, map[string][]string{"app": nil})
	plan := &Plan{Project: proj, ProjectName: "dc-x"}

	if _, err := orch.Up(context.Background(), plan); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	firstRun := rt.runCalls

	if _, err := orch.Up(context.Background(), plan); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if rt.runCalls != firstRun {
		t.Errorf("second Up triggered %d new RunContainer calls; expected 0 (reuse)", rt.runCalls-firstRun)
	}
}

func TestUp_RecreateOnHashChange(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt, "docker")
	proj := newProject(t, map[string][]string{"app": nil})

	if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	firstRun := rt.runCalls
	firstRemove := rt.removeCalls

	// Mutate the service so its hash changes.
	svc := proj.Services["app"]
	svc.WorkingDir = "/changed"
	proj.Services["app"] = svc

	if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if rt.runCalls != firstRun+1 {
		t.Errorf("runCalls=%d, want %d (recreate expected)", rt.runCalls, firstRun+1)
	}
	if rt.removeCalls != firstRemove+1 {
		t.Errorf("removeCalls=%d, want %d (old container should be removed)", rt.removeCalls, firstRemove+1)
	}
}

func TestUp_PartialFailureSurfacesPartialError(t *testing.T) {
	rt := newMockRuntime()
	rt.OnRunContainer = func(spec runtime.RunSpec) (*runtime.Container, error) {
		if spec.Labels[LabelComposeService] == "api" {
			return nil, errors.New("simulated start failure")
		}
		return nil, nil
	}
	orch := NewOrchestrator(rt, "docker")
	proj := newProject(t, map[string][]string{
		"db":  nil,
		"api": {"db"},
	})

	_, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"})
	var pe *PartialUpError
	if !errors.As(err, &pe) {
		t.Fatalf("want *PartialUpError, got %T: %v", err, err)
	}
	if pe.Failed != "api" {
		t.Errorf("Failed=%q, want api", pe.Failed)
	}
	if len(pe.Started) != 1 || pe.Started[0] != "db" {
		t.Errorf("Started=%v, want [db]", pe.Started)
	}
}

func TestUp_HealthGateTimesOut(t *testing.T) {
	rt := newMockRuntime()
	// Force every InspectContainer to report State=Created — never
	// running, so service_healthy never fires.
	rt.OnInspect = func(id string, base *runtime.ContainerDetails) *runtime.ContainerDetails {
		base.State = runtime.StateCreated
		return base
	}
	orch := NewOrchestrator(rt, "docker")
	orch.HealthTimeout = 100 * time.Millisecond
	orch.PollInterval = 20 * time.Millisecond

	// app depends on db with service_healthy.
	svcs := composetypes.Services{
		"db":  composetypes.ServiceConfig{Name: "db", Image: "alpine"},
		"app": composetypes.ServiceConfig{
			Name: "app", Image: "alpine",
			DependsOn: composetypes.DependsOnConfig{
				"db": composetypes.ServiceDependency{Condition: "service_healthy"},
			},
		},
	}
	proj := &composetypes.Project{Services: svcs}

	_, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"})
	var hte *HealthTimeoutError
	if !errors.As(err, &hte) {
		t.Fatalf("want *HealthTimeoutError, got %T: %v", err, err)
	}
	if hte.Service != "db" {
		t.Errorf("Service=%q, want db", hte.Service)
	}
}

func TestUp_RefusesUnsupportedFields(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt, "docker")
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{Name: "app", Image: "alpine", Deploy: &composetypes.DeployConfig{}},
		},
	}
	_, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"})
	var unsup *UnsupportedFieldError
	if !errors.As(err, &unsup) {
		t.Fatalf("want *UnsupportedFieldError, got %T: %v", err, err)
	}
	if rt.createNetworkCalls != 0 || rt.runCalls != 0 {
		t.Error("validate must run before any side effect")
	}
}

func TestDown_RemovesProjectContainers(t *testing.T) {
	rt := newMockRuntime()
	// Pre-seed two containers from a prior Up.
	rt.containers["c1"] = &mockContainer{
		id: "c1", name: "dc-x-app-1", state: runtime.StateRunning,
		labels: map[string]string{
			LabelComposeProject: "dc-x",
			LabelComposeService: "app",
		},
	}
	rt.containers["c2"] = &mockContainer{
		id: "c2", name: "dc-x-db-1", state: runtime.StateRunning,
		labels: map[string]string{
			LabelComposeProject: "dc-x",
			LabelComposeService: "db",
		},
	}
	// And one from a different project — must not be touched.
	rt.containers["c3"] = &mockContainer{
		id: "c3", name: "other", state: runtime.StateRunning,
		labels: map[string]string{LabelComposeProject: "other"},
	}

	orch := NewOrchestrator(rt, "docker")
	if err := orch.Down(context.Background(), &DownPlan{ProjectName: "dc-x"}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if _, ok := rt.containers["c1"]; ok {
		t.Error("c1 not removed")
	}
	if _, ok := rt.containers["c2"]; ok {
		t.Error("c2 not removed")
	}
	if _, ok := rt.containers["c3"]; !ok {
		t.Error("c3 (other project) was wrongly removed")
	}
}

func TestDown_Idempotent(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt, "docker")
	// Project never up; Down must be a clean no-op.
	if err := orch.Down(context.Background(), &DownPlan{ProjectName: "dc-x"}); err != nil {
		t.Errorf("Down on empty: %v", err)
	}
}
