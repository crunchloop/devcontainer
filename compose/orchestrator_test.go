package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
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
type mockRuntime struct {
	mu sync.Mutex

	// Resources
	networks   map[string]map[string]string // name -> labels
	volumes    map[string]map[string]string // name -> labels
	containers map[string]*mockContainer    // id -> container

	// Call log for assertions
	// Caps is what Capabilities() reports. newMockRuntime defaults to
	// the docker baseline; tests exercising the fallback paths set it.
	Caps runtime.Capabilities

	createNetworkCalls int
	createVolumeCalls  int
	runCalls           int
	startCalls         int
	stopCalls          int
	removeCalls        int
	removeNetworkCalls []string

	// Hooks (set to override default behavior).
	OnRunContainer func(spec runtime.RunSpec) (*runtime.Container, error)
	OnInspect      func(id string, base *runtime.ContainerDetails) *runtime.ContainerDetails
	OnExec         func(id string, opts runtime.ExecOptions) (runtime.ExecResult, error)

	// imageDigest, when non-empty, is the digest InspectImage
	// returns for every reference. Tests that exercise digest-drift
	// recreate set this between Up calls.
	imageDigest string
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
		Caps:       runtime.Capabilities{ExitCodes: true, ServiceNameDNS: true},
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
	if m.OnExec != nil {
		return m.OnExec(id, opts)
	}
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

func (m *mockRuntime) Capabilities() runtime.Capabilities {
	return m.Caps
}

func (m *mockRuntime) InspectImage(ctx context.Context, ref string) (*runtime.ImageDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.imageDigest
	if id == "" {
		id = "sha256:" + ref
	}
	return &runtime.ImageDetails{ID: id, Tags: []string{ref}}, nil
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeNetworkCalls = append(m.removeNetworkCalls, id)
	delete(m.networks, id)
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
	// Mirror the real Runtime contract: empty filters are rejected.
	// Lets orchestrator regressions that drop the filter surface
	// here instead of silently returning every container.
	if len(filter.Match) == 0 {
		return nil, errors.New("mockRuntime: ListContainers requires a non-empty filter")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []runtime.Container
	for _, c := range m.containers {
		if labelsSuperset(c.labels, filter.Match) {
			out = append(out, runtime.Container{ID: c.id, Name: c.name, Image: c.image, State: c.state, Labels: copyLabels(c.labels)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *mockRuntime) ListImages(ctx context.Context, filter runtime.LabelFilter) ([]runtime.ImageRef, error) {
	if len(filter.Match) == 0 {
		return nil, errors.New("mockRuntime: ListImages requires a non-empty filter")
	}
	return nil, nil
}

func (m *mockRuntime) RemoveImage(ctx context.Context, ref string) error {
	return nil
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
	orch := NewOrchestrator(rt)
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

func TestServiceToRunSpec_CarriesSecurityFields(t *testing.T) {
	initT := true
	svc := composetypes.ServiceConfig{
		Name:        "app",
		Image:       "alpine",
		Init:        &initT,
		Privileged:  true,
		CapAdd:      []string{"SYS_ADMIN"},
		SecurityOpt: []string{"seccomp=unconfined"},
	}
	spec, err := serviceToRunSpec(&Plan{ProjectName: "dc-x"}, svc, nil, "hash", "", func(string) string { return "" })
	if err != nil {
		t.Fatalf("serviceToRunSpec: %v", err)
	}

	if !spec.Privileged {
		t.Error("Privileged not carried into RunSpec")
	}
	if !spec.Init {
		t.Error("Init not carried into RunSpec")
	}
	if len(spec.CapAdd) != 1 || spec.CapAdd[0] != "SYS_ADMIN" {
		t.Errorf("CapAdd = %v, want [SYS_ADMIN]", spec.CapAdd)
	}
	if len(spec.SecurityOpt) != 1 || spec.SecurityOpt[0] != "seccomp=unconfined" {
		t.Errorf("SecurityOpt = %v, want [seccomp=unconfined]", spec.SecurityOpt)
	}
}

func TestUp_DependencyOrder(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
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
	if len(order) != len(want) {
		t.Fatalf("order len=%d, want %d (full=%v)", len(order), len(want), order)
	}
	for i, name := range want {
		if order[i] != name {
			t.Errorf("order[%d] = %q, want %q (full=%v)", i, order[i], name, order)
		}
	}
}

func TestUp_IdempotentReuseOnHashMatch(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
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
	orch := NewOrchestrator(rt)
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

// TestUp_RecreateOnImageDigestChange exercises the digest-driven
// recreate path: the compose service config is byte-identical
// across two Ups, but the registry has moved the tag to a new
// digest. Our InspectImage mock returns a different digest on the
// second call, and the orchestrator must recreate the container —
// otherwise users would silently keep running the old image after
// a `docker pull`.
func TestUp_RecreateOnImageDigestChange(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{"app": nil})
	plan := &Plan{Project: proj, ProjectName: "dc-x"}

	if _, err := orch.Up(context.Background(), plan); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	firstRun := rt.runCalls
	firstRemove := rt.removeCalls

	// Flip the InspectImage to return a different digest so the
	// orchestrator observes a tag-to-different-digest drift.
	rt.imageDigest = "sha256:moved"

	if _, err := orch.Up(context.Background(), plan); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if rt.runCalls != firstRun+1 {
		t.Errorf("runCalls=%d, want %d (digest-change recreate expected)", rt.runCalls, firstRun+1)
	}
	if rt.removeCalls != firstRemove+1 {
		t.Errorf("removeCalls=%d, want %d", rt.removeCalls, firstRemove+1)
	}
}

// TestUp_StartsStoppedContainerOnConfigMatch covers the daemon-restart
// path: a previous Up created and started a container; the docker
// daemon then went away (e.g. host pod destroyed in a k8s deployment
// where /var/lib/docker lives on a PVC) and came back, leaving the
// container Exited. A subsequent Up against an unchanged project
// must reuse the existing container by Starting it, not by destroying
// and recreating it — recreating would lose the writable layer
// (issue #71).
func TestUp_StartsStoppedContainerOnConfigMatch(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{"app": nil})
	plan := &Plan{Project: proj, ProjectName: "dc-x"}

	res, err := orch.Up(context.Background(), plan)
	if err != nil {
		t.Fatalf("first Up: %v", err)
	}
	firstID := res.ContainerIDs["app"]
	if firstID == "" {
		t.Fatalf("first Up did not produce a container id")
	}
	firstRun := rt.runCalls
	firstRemove := rt.removeCalls

	// Simulate the daemon-restart effect: container is on disk but
	// not running. Use the public StopContainer path so the mock
	// records the state transition the same way a real Exited
	// container would surface to ListContainers.
	if err := rt.StopContainer(context.Background(), firstID, runtime.StopOptions{}); err != nil {
		t.Fatalf("stop container: %v", err)
	}
	startCallsBeforeReuse := rt.startCalls

	res2, err := orch.Up(context.Background(), plan)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}

	if got := res2.ContainerIDs["app"]; got != firstID {
		t.Errorf("container id changed across Up: first=%s second=%s — reuse expected", firstID, got)
	}
	if rt.runCalls != firstRun {
		t.Errorf("runCalls=%d, want %d (no RunContainer expected on reuse)", rt.runCalls, firstRun)
	}
	if rt.removeCalls != firstRemove {
		t.Errorf("removeCalls=%d, want %d (no RemoveContainer expected on reuse)", rt.removeCalls, firstRemove)
	}
	if rt.startCalls != startCallsBeforeReuse+1 {
		t.Errorf("startCalls=%d, want %d (one StartContainer expected to revive the stopped container)", rt.startCalls, startCallsBeforeReuse+1)
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
	orch := NewOrchestrator(rt)
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
	orch := NewOrchestrator(rt)
	orch.HealthTimeout = 100 * time.Millisecond
	orch.PollInterval = 20 * time.Millisecond

	// app depends on db with service_healthy.
	svcs := composetypes.Services{
		"db": composetypes.ServiceConfig{Name: "db", Image: "alpine"},
		"app": composetypes.ServiceConfig{
			Name: "app", Image: "alpine",
			DependsOn: composetypes.DependsOnConfig{
				// Required: true mirrors what compose-go's Load
				// produces after normalization. Without it, the
				// dependency is treated as optional and the gate
				// timeout would be swallowed.
				"db": composetypes.ServiceDependency{Condition: "service_healthy", Required: true},
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

// TestUp_OptionalDependencySkipsOnTimeout pins the compose-spec
// optional-dependency contract: when depends_on.<dep>.required=false
// AND the dep's health gate doesn't satisfy in time, the dependent
// still starts (the gate failure is swallowed). Strict dependencies
// (required=true) keep their existing fatal-on-timeout behavior.
func TestUp_OptionalDependencySkipsOnTimeout(t *testing.T) {
	rt := newMockRuntime()
	// db never reports running -> service_healthy never satisfied.
	rt.OnInspect = func(id string, base *runtime.ContainerDetails) *runtime.ContainerDetails {
		base.State = runtime.StateCreated
		return base
	}
	orch := NewOrchestrator(rt)
	orch.HealthTimeout = 50 * time.Millisecond
	orch.PollInterval = 10 * time.Millisecond

	svcs := composetypes.Services{
		"db": composetypes.ServiceConfig{Name: "db", Image: "alpine"},
		"app": composetypes.ServiceConfig{
			Name: "app", Image: "alpine",
			DependsOn: composetypes.DependsOnConfig{
				"db": composetypes.ServiceDependency{
					Condition: "service_healthy",
					Required:  false,
				},
			},
		},
	}
	proj := &composetypes.Project{Services: svcs}

	res, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"})
	if err != nil {
		t.Fatalf("Up: %v (optional dep should not fail Up)", err)
	}
	if res.ContainerIDs["app"] == "" {
		t.Error("app not started after optional-dep health timeout")
	}
}

func TestUp_RefusesUnsupportedFields(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := &composetypes.Project{
		Services: composetypes.Services{
			"app": composetypes.ServiceConfig{Name: "app", Image: "alpine", Deploy: &composetypes.DeployConfig{Mode: "global"}},
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

	orch := NewOrchestrator(rt)
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

// TestDown_ReverseTopoOrder pins the contract that Down processes
// dependent services BEFORE their dependencies. The orchestrator
// reads the compose-service label off each container (set on Up)
// and looks it up against the project's topo levels.
func TestDown_ReverseTopoOrder(t *testing.T) {
	rt := newMockRuntime()
	// db (level 0) ← api (level 1) ← app (level 2).
	rt.containers["c-db"] = &mockContainer{
		id: "c-db", name: "dc-x-db-1", state: runtime.StateRunning,
		labels: map[string]string{
			LabelComposeProject: "dc-x",
			LabelComposeService: "db",
		},
	}
	rt.containers["c-api"] = &mockContainer{
		id: "c-api", name: "dc-x-api-1", state: runtime.StateRunning,
		labels: map[string]string{
			LabelComposeProject: "dc-x",
			LabelComposeService: "api",
		},
	}
	rt.containers["c-app"] = &mockContainer{
		id: "c-app", name: "dc-x-app-1", state: runtime.StateRunning,
		labels: map[string]string{
			LabelComposeProject: "dc-x",
			LabelComposeService: "app",
		},
	}

	// Record stop order for assertion.
	var stopOrder []string
	var mu sync.Mutex
	wrapStop := rt.stopCalls
	_ = wrapStop
	origStop := func(ctx context.Context, id string, opts runtime.StopOptions) error {
		mu.Lock()
		stopOrder = append(stopOrder, id)
		mu.Unlock()
		return nil
	}

	// Hook stop tracking via a small wrapper. The mock's StopContainer
	// just sets state and counts; we tap into RemoveContainer too
	// since that's what the orchestrator does after Stop.
	wrapped := &stopTracker{
		mockRuntime: rt,
		stopFunc:    origStop,
	}

	orch := NewOrchestrator(wrapped)
	proj := newProject(t, map[string][]string{
		"db":  nil,
		"api": {"db"},
		"app": {"api"},
	})
	if err := orch.Down(context.Background(), &DownPlan{ProjectName: "dc-x", Project: proj}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	want := []string{"c-app", "c-api", "c-db"}
	if len(stopOrder) != 3 {
		t.Fatalf("stopOrder=%v, want 3 entries", stopOrder)
	}
	for i, exp := range want {
		if stopOrder[i] != exp {
			t.Errorf("stopOrder[%d]=%q, want %q (full=%v)", i, stopOrder[i], exp, stopOrder)
		}
	}
}

// stopTracker wraps mockRuntime to capture stop order.
type stopTracker struct {
	*mockRuntime
	stopFunc func(context.Context, string, runtime.StopOptions) error
}

func (s *stopTracker) StopContainer(ctx context.Context, id string, opts runtime.StopOptions) error {
	if s.stopFunc != nil {
		_ = s.stopFunc(ctx, id, opts)
	}
	return s.mockRuntime.StopContainer(ctx, id, opts)
}

// TestUp_AnonymousVolumesFlowThrough confirms a service-level
// `volumes: [target_only_path]` (no source) makes it through to the
// RunSpec the runtime sees, with empty Source — docker's convention
// for anonymous volumes. The orchestrator must NOT call CreateVolume
// for the anonymous entry (only named ones).
func TestUp_AnonymousVolumesFlowThrough(t *testing.T) {
	rt := newMockRuntime()
	var seenSpec runtime.RunSpec
	rt.OnRunContainer = func(spec runtime.RunSpec) (*runtime.Container, error) {
		if spec.Labels[LabelComposeService] == "app" {
			seenSpec = spec
		}
		return nil, nil
	}
	orch := NewOrchestrator(rt)
	svc := composetypes.ServiceConfig{
		Name:  "app",
		Image: "alpine",
		Volumes: []composetypes.ServiceVolumeConfig{
			// Anonymous: no Source.
			{Type: composetypes.VolumeTypeVolume, Target: "/data"},
		},
	}
	proj := &composetypes.Project{
		Services: composetypes.Services{"app": svc},
	}
	if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if rt.createVolumeCalls != 0 {
		t.Errorf("CreateVolume call count = %d, want 0 (anonymous volumes must not be pre-created)", rt.createVolumeCalls)
	}
	if len(seenSpec.Mounts) != 1 {
		t.Fatalf("Mounts = %v, want 1 entry", seenSpec.Mounts)
	}
	got := seenSpec.Mounts[0]
	if got.Type != runtime.MountVolume || got.Source != "" || got.Target != "/data" {
		t.Errorf("anonymous mount = %+v, want {Type:volume Source:\"\" Target:/data}", got)
	}
}

// TestUp_ResourceLimitsTranslate pins the compose-to-RunSpec mapping
// for memory and CPU limits, including the deploy.resources.limits >
// legacy mem_limit/cpus precedence. Backends translate from RunSpec;
// this test pins the orchestrator side.
func TestUp_ResourceLimitsTranslate(t *testing.T) {
	cases := []struct {
		name     string
		mut      func(*composetypes.ServiceConfig)
		wantMem  int64
		wantNano int64
	}{
		{
			name: "deploy_limits",
			mut: func(s *composetypes.ServiceConfig) {
				s.Deploy = &composetypes.DeployConfig{
					Resources: composetypes.Resources{
						Limits: &composetypes.Resource{
							MemoryBytes: composetypes.UnitBytes(2 * 1024 * 1024 * 1024),
							NanoCPUs:    composetypes.NanoCPUs(2.5),
						},
					},
				}
			},
			wantMem:  2 * 1024 * 1024 * 1024,
			wantNano: 2_500_000_000,
		},
		{
			name: "legacy_only",
			mut: func(s *composetypes.ServiceConfig) {
				s.MemLimit = composetypes.UnitBytes(512 * 1024 * 1024)
				s.CPUS = 1.5
			},
			wantMem:  512 * 1024 * 1024,
			wantNano: 1_500_000_000,
		},
		{
			name: "deploy_overrides_legacy",
			mut: func(s *composetypes.ServiceConfig) {
				s.MemLimit = composetypes.UnitBytes(128 * 1024 * 1024)
				s.CPUS = 1.0
				s.Deploy = &composetypes.DeployConfig{
					Resources: composetypes.Resources{
						Limits: &composetypes.Resource{
							MemoryBytes: composetypes.UnitBytes(4 * 1024 * 1024 * 1024),
							NanoCPUs:    composetypes.NanoCPUs(4),
						},
					},
				}
			},
			wantMem:  4 * 1024 * 1024 * 1024,
			wantNano: 4_000_000_000,
		},
		{
			name:     "unset",
			mut:      func(*composetypes.ServiceConfig) {},
			wantMem:  0,
			wantNano: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newMockRuntime()
			var seen runtime.RunSpec
			rt.OnRunContainer = func(spec runtime.RunSpec) (*runtime.Container, error) {
				seen = spec
				return nil, nil
			}
			orch := NewOrchestrator(rt)
			svc := composetypes.ServiceConfig{Name: "app", Image: "alpine"}
			tc.mut(&svc)
			proj := &composetypes.Project{Services: composetypes.Services{"app": svc}}
			if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
				t.Fatalf("Up: %v", err)
			}
			if seen.MemoryBytes != tc.wantMem {
				t.Errorf("MemoryBytes = %d, want %d", seen.MemoryBytes, tc.wantMem)
			}
			if seen.NanoCPUs != tc.wantNano {
				t.Errorf("NanoCPUs = %d, want %d", seen.NanoCPUs, tc.wantNano)
			}
		})
	}
}

// TestDown_RemovesProjectNetwork pins the network-cleanup contract.
// Up creates <project>_default; Down must call RemoveNetwork on it
// after containers are gone. Without this, every devcontainer
// teardown would leak the project network.
func TestDown_RemovesProjectNetwork(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{"app": nil})
	plan := &Plan{Project: proj, ProjectName: "dc-x"}
	if _, err := orch.Up(context.Background(), plan); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := orch.Down(context.Background(), &DownPlan{ProjectName: "dc-x"}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	wantNet := "dc-x_default"
	found := false
	for _, id := range rt.removeNetworkCalls {
		if id == wantNet {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("RemoveNetwork(%q) not called; got %v", wantNet, rt.removeNetworkCalls)
	}
}

func TestDown_Idempotent(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	// Project never up; Down must be a clean no-op.
	if err := orch.Down(context.Background(), &DownPlan{ProjectName: "dc-x"}); err != nil {
		t.Errorf("Down on empty: %v", err)
	}
}

// A container for the (project, service) pair that this orchestrator
// did not create — the shellout backend or a plain `docker compose up`
// — has the compose labels but none of the dev.containers ones. It
// must be adopted, not recreated: removing it destroys the writable
// layer the shellout path's NoRecreate contract preserved. Recreate-
// mode Ups tear the project down before the orchestrator runs, so
// adoption only ever applies to reattach/resume flows.
func TestUp_AdoptsForeignContainerWithoutOurLabels(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{"app": nil})

	rt.containers["legacy-1"] = &mockContainer{
		id: "legacy-1", name: "dc-x-app-1", image: "alpine",
		labels: map[string]string{
			LabelComposeProject: "dc-x",
			LabelComposeService: "app",
		},
		state: runtime.StateExited,
	}

	res, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if res.ContainerIDs["app"] != "legacy-1" {
		t.Errorf("ContainerIDs[app] = %q, want the adopted legacy-1", res.ContainerIDs["app"])
	}
	if rt.removeCalls != 0 {
		t.Errorf("removeCalls = %d; adopting must not remove the foreign container", rt.removeCalls)
	}
	if rt.runCalls != 0 {
		t.Errorf("runCalls = %d; adopting must not create a replacement", rt.runCalls)
	}
	// The exited container must be started, mirroring the reuse path.
	if rt.containers["legacy-1"].state != runtime.StateRunning {
		t.Errorf("adopted container state = %v, want running", rt.containers["legacy-1"].state)
	}
}

// Same adoption when the foreign container is already running: fully
// hands-off.
func TestUp_AdoptsRunningForeignContainerWithoutStarting(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{"app": nil})

	rt.containers["legacy-2"] = &mockContainer{
		id: "legacy-2", name: "dc-x-app-1", image: "alpine",
		labels: map[string]string{
			LabelComposeProject: "dc-x",
			LabelComposeService: "app",
		},
		state: runtime.StateRunning,
	}

	res, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if res.ContainerIDs["app"] != "legacy-2" {
		t.Errorf("ContainerIDs[app] = %q, want legacy-2", res.ContainerIDs["app"])
	}
	if rt.removeCalls != 0 || rt.runCalls != 0 || rt.startCalls != 0 {
		t.Errorf("adoption of a running container must be hands-off (remove=%d run=%d start=%d)",
			rt.removeCalls, rt.runCalls, rt.startCalls)
	}
}

// Restricting a plan to a service subset must keep `docker compose up
// <names...>` semantics: dependencies (including `service:<x>`
// namespace targets) come up too.
func TestUp_RestrictedServicesStartDependencyClosure(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{
		"db":       nil,
		"app":      {"db"},
		"unwanted": nil,
	})

	res, err := orch.Up(context.Background(), &Plan{
		Project: proj, ProjectName: "dc-x", Services: []string{"app"},
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if res.ContainerIDs["app"] == "" || res.ContainerIDs["db"] == "" {
		t.Errorf("ContainerIDs = %+v, want app AND its dependency db", res.ContainerIDs)
	}
	if _, started := res.ContainerIDs["unwanted"]; started {
		t.Error("service outside the closure was started")
	}
}

func TestServiceClosure_FollowsNamespaceEdges(t *testing.T) {
	proj := newProject(t, map[string][]string{"proxy": nil, "app": nil, "other": nil})
	app := proj.Services["app"]
	app.NetworkMode = "service:proxy"
	proj.Services["app"] = app

	got := ServiceClosure(proj, []string{"app"})
	want := []string{"app", "proxy"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ServiceClosure = %v, want %v", got, want)
	}
}

// network_mode must reach the backend, not be silently replaced by
// the project network — `none` in particular is an isolation request.
func TestUp_NetworkModeCarriedAndProjectNetworkSkipped(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{"app": nil, "sandboxed": nil})
	sandboxed := proj.Services["sandboxed"]
	sandboxed.NetworkMode = "none"
	sandboxed.Pid = "host"
	proj.Services["sandboxed"] = sandboxed

	var mu sync.Mutex
	specs := map[string]runtime.RunSpec{}
	rt.OnRunContainer = func(spec runtime.RunSpec) (*runtime.Container, error) {
		mu.Lock()
		specs[spec.Labels[LabelComposeService]] = spec
		mu.Unlock()
		return nil, nil
	}

	if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := specs["sandboxed"].NetworkMode; got != "none" {
		t.Errorf("sandboxed NetworkMode = %q, want none", got)
	}
	if got := specs["sandboxed"].PidMode; got != "host" {
		t.Errorf("sandboxed PidMode = %q, want host", got)
	}
	if nets := specs["sandboxed"].Networks; len(nets) != 0 {
		t.Errorf("sandboxed Networks = %v; a namespace mode excludes the project network", nets)
	}
	if nets := specs["app"].Networks; len(nets) != 1 {
		t.Errorf("app Networks = %v, want the project network", nets)
	}
}

// `service:<x>` resolves to the dependency's container ID, which the
// topo order guarantees exists by the time the dependent is created.
func TestUp_ServiceNetworkModeResolvesToContainer(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{"proxy": nil, "app": nil})
	app := proj.Services["app"]
	app.NetworkMode = "service:proxy"
	proj.Services["app"] = app

	var mu sync.Mutex
	specs := map[string]runtime.RunSpec{}
	rt.OnRunContainer = func(spec runtime.RunSpec) (*runtime.Container, error) {
		mu.Lock()
		specs[spec.Labels[LabelComposeService]] = spec
		mu.Unlock()
		return nil, nil
	}

	res, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	want := "container:" + res.ContainerIDs["proxy"]
	if got := specs["app"].NetworkMode; got != want {
		t.Errorf("app NetworkMode = %q, want %q", got, want)
	}
}

// AdoptExisting (resume) reuses an existing container even when its stored
// config-hash no longer matches — a restored workspace's primary image is
// feature-layered fresh every boot, so the hash always drifts; adoption must
// keep the container (and its upperdir + anon volumes) anyway.
func TestUp_AdoptExistingReusesDespiteHashDrift(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{"app": nil})

	// A prior orchestrator-made container whose stored hash is stale.
	rt.containers["resumed-1"] = &mockContainer{
		id: "resumed-1", name: "dc-x-app-1", image: "alpine",
		labels: map[string]string{
			LabelComposeProject: "dc-x",
			LabelComposeService: "app",
			LabelConfigHash:     "STALE-hash-from-a-previous-boot",
			LabelImageDigest:    "sha256:stale",
		},
		state: runtime.StateExited,
	}

	res, err := orch.Up(context.Background(), &Plan{
		Project: proj, ProjectName: "dc-x", AdoptExisting: true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if res.ContainerIDs["app"] != "resumed-1" {
		t.Errorf("ContainerIDs[app] = %q, want the adopted resumed-1", res.ContainerIDs["app"])
	}
	if rt.removeCalls != 0 {
		t.Errorf("removeCalls = %d; adoption must not recreate despite hash drift", rt.removeCalls)
	}
	if rt.runCalls != 0 {
		t.Errorf("runCalls = %d; adoption must not create a replacement", rt.runCalls)
	}
	if rt.containers["resumed-1"].state != runtime.StateRunning {
		t.Errorf("adopted container not started: state=%v", rt.containers["resumed-1"].state)
	}
}

// Without AdoptExisting, hash drift still recreates (guards the default path).
func TestUp_NoAdoptRecreatesOnHashDrift(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	proj := newProject(t, map[string][]string{"app": nil})
	rt.containers["old-1"] = &mockContainer{
		id: "old-1", name: "dc-x-app-1", image: "alpine",
		labels: map[string]string{
			LabelComposeProject: "dc-x", LabelComposeService: "app",
			LabelConfigHash: "STALE", LabelImageDigest: "sha256:stale",
		},
		state: runtime.StateExited,
	}
	if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if rt.removeCalls == 0 {
		t.Error("expected recreate (remove) on hash drift without AdoptExisting")
	}
}

// TestUp_HealthGateRefusesUnreportedHealth pins the contract for a
// backend that does not surface health status. runtime.HealthStatus
// documents HealthNone as ambiguous — either the image declared no
// HEALTHCHECK, or the backend reports no health at all — and the gate
// treats it as satisfied so healthcheck-less projects come up. When
// the service declares a healthcheck of its own, that reading is
// unavailable: passing the gate would start dependents before the
// check ever succeeded. Until #128 removed the capability gating,
// Plan.Validate refused these plans up front via
// Capabilities.Healthchecks; this asserts the guarantee survives at
// the gate that needs it, for any Runtime implementation.
func TestUp_HealthGateRefusesUnreportedHealth(t *testing.T) {
	// mockRuntime's inspect reports State=Running with the zero
	// HealthStatus, which IS runtime.HealthNone.
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	orch.HealthTimeout = 100 * time.Millisecond
	orch.PollInterval = 20 * time.Millisecond

	proj := &composetypes.Project{Services: composetypes.Services{
		"db": composetypes.ServiceConfig{
			Name: "db", Image: "alpine",
			HealthCheck: &composetypes.HealthCheckConfig{Test: []string{"CMD", "true"}},
		},
		"app": composetypes.ServiceConfig{
			Name: "app", Image: "alpine",
			DependsOn: composetypes.DependsOnConfig{
				"db": composetypes.ServiceDependency{Condition: "service_healthy", Required: true},
			},
		},
	}}

	_, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"})
	if err == nil {
		t.Fatal("want an error: the backend never reported health for a service that declares one")
	}
	if !strings.Contains(err.Error(), "never reported a health status") {
		t.Errorf("error = %v, want it to name the unreported health status", err)
	}
}

// TestUp_HealthGatePassesWithoutDeclaredHealthcheck locks the other
// side of that boundary: with no healthcheck declared, HealthNone
// keeps meaning "no healthcheck" and State=Running satisfies the
// gate. This is compose v2's behavior for healthcheck-less services
// and the reason HealthNone is permissive in the first place.
func TestUp_HealthGatePassesWithoutDeclaredHealthcheck(t *testing.T) {
	rt := newMockRuntime()
	orch := NewOrchestrator(rt)
	orch.HealthTimeout = 100 * time.Millisecond
	orch.PollInterval = 20 * time.Millisecond

	proj := &composetypes.Project{Services: composetypes.Services{
		"db": composetypes.ServiceConfig{Name: "db", Image: "alpine"},
		"app": composetypes.ServiceConfig{
			Name: "app", Image: "alpine",
			DependsOn: composetypes.DependsOnConfig{
				"db": composetypes.ServiceDependency{Condition: "service_healthy", Required: true},
			},
		},
	}}

	if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
		t.Fatalf("Up: %v", err)
	}
}

// TestUp_HealthGateAcceptsDisabledHealthchecks covers the ways a
// compose service can have a HealthCheckConfig that is nonetheless not
// an active healthcheck. Each must keep the permissive HealthNone +
// State=Running fallback, or a valid project would block on its own
// gate until the health timeout:
//
//   - test: ["NONE"] — compose's inline disable. compose-go accepts it
//     verbatim (loader/validate.go allows CMD, CMD-SHELL and NONE) and
//     does not fold it into Disable, and runtime/docker forwards it to
//     docker's NONE sentinel, so docker reports no health for it.
//   - disable: true — the explicit form.
//   - no test at all — the image's HEALTHCHECK applies, and the
//     compose file cannot tell us whether the image declares one.
func TestUp_HealthGateAcceptsDisabledHealthchecks(t *testing.T) {
	cases := map[string]*composetypes.HealthCheckConfig{
		"test_none":  {Test: []string{"NONE"}},
		"disable":    {Test: []string{"CMD", "true"}, Disable: true},
		"no_test":    {},
		"nil_config": nil,
	}
	for name, hc := range cases {
		t.Run(name, func(t *testing.T) {
			rt := newMockRuntime()
			orch := NewOrchestrator(rt)
			orch.HealthTimeout = 100 * time.Millisecond
			orch.PollInterval = 20 * time.Millisecond

			proj := &composetypes.Project{Services: composetypes.Services{
				"db": composetypes.ServiceConfig{Name: "db", Image: "alpine", HealthCheck: hc},
				"app": composetypes.ServiceConfig{
					Name: "app", Image: "alpine",
					DependsOn: composetypes.DependsOnConfig{
						"db": composetypes.ServiceDependency{Condition: "service_healthy", Required: true},
					},
				},
			}}

			if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
				t.Fatalf("Up: %v", err)
			}
		})
	}
}

// TestUp_PatchesHostsWithoutServiceNameDNS pins the service-name DNS
// fallback: on a backend that does not resolve peers by service name,
// Up patches /etc/hosts inside every started container. Nothing
// observable at Up time distinguishes working DNS from broken DNS —
// the failure shows up inside the container later — which is why this
// stays a declared capability rather than a check at the gate.
func TestUp_PatchesHostsWithoutServiceNameDNS(t *testing.T) {
	rt := newMockRuntime()
	rt.Caps = runtime.Capabilities{ExitCodes: true, ServiceNameDNS: false}

	// Report an IP for every container so the patch has a map to write.
	rt.OnInspect = func(id string, base *runtime.ContainerDetails) *runtime.ContainerDetails {
		base.Labels["dev.containers.network-ip"] = "192.168.66.2"
		return base
	}
	var patched []string
	rt.OnExec = func(id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
		if len(opts.Cmd) > 0 && strings.Contains(strings.Join(opts.Cmd, " "), "/etc/hosts") {
			patched = append(patched, id)
		}
		return runtime.ExecResult{}, nil
	}

	orch := NewOrchestrator(rt)
	proj := &composetypes.Project{Services: composetypes.Services{
		"db":  composetypes.ServiceConfig{Name: "db", Image: "alpine"},
		"app": composetypes.ServiceConfig{Name: "app", Image: "alpine"},
	}}

	if _, err := orch.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-x"}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(patched) == 0 {
		t.Error("want /etc/hosts patched on a backend without service-name DNS")
	}

	// And the inverse: the docker baseline never touches /etc/hosts.
	rt2 := newMockRuntime()
	var patched2 []string
	rt2.OnExec = func(id string, opts runtime.ExecOptions) (runtime.ExecResult, error) {
		if len(opts.Cmd) > 0 && strings.Contains(strings.Join(opts.Cmd, " "), "/etc/hosts") {
			patched2 = append(patched2, id)
		}
		return runtime.ExecResult{}, nil
	}
	orch2 := NewOrchestrator(rt2)
	if _, err := orch2.Up(context.Background(), &Plan{Project: proj, ProjectName: "dc-y"}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(patched2) != 0 {
		t.Errorf("docker baseline must not patch /etc/hosts, got %v", patched2)
	}
}
