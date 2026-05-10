package config

import (
	"reflect"
	"testing"
)

func ptrBool(b bool) *bool { return &b }

func TestMergeMetadata_RemoteUser_UserWins(t *testing.T) {
	cfg := &ResolvedConfig{RemoteUser: "alice"}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "base", RemoteUser: "vscode"},
		{ID: "feat", RemoteUser: "node"},
	})
	if cfg.RemoteUser != "alice" {
		t.Errorf("user-supplied remoteUser must win, got %q", cfg.RemoteUser)
	}
}

func TestMergeMetadata_RemoteUser_FillsFromBase(t *testing.T) {
	cfg := &ResolvedConfig{}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "base", RemoteUser: "vscode"},
	})
	if cfg.RemoteUser != "vscode" {
		t.Errorf("RemoteUser should fill from base layer when user unset, got %q", cfg.RemoteUser)
	}
}

func TestMergeMetadata_RemoteUser_LastLayerWins(t *testing.T) {
	cfg := &ResolvedConfig{}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "base", RemoteUser: "root"},
		{ID: "common-utils", RemoteUser: "vscode"},
	})
	if cfg.RemoteUser != "vscode" {
		t.Errorf("expected last non-empty layer to win, got %q", cfg.RemoteUser)
	}
}

func TestMergeMetadata_BoolFields(t *testing.T) {
	cfg := &ResolvedConfig{Init: ptrBool(true)}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "a", Init: ptrBool(false), Privileged: ptrBool(true)},
	})
	if !*cfg.Init {
		t.Errorf("user Init=true must win, got %v", *cfg.Init)
	}
	if cfg.Privileged == nil || !*cfg.Privileged {
		t.Errorf("Privileged should fill from layer, got %v", cfg.Privileged)
	}
}

func TestMergeMetadata_EnvUnion_UserWinsOnCollision(t *testing.T) {
	cfg := &ResolvedConfig{ContainerEnv: map[string]string{"PATH": "/user", "USER_VAR": "u"}}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "a", ContainerEnv: map[string]string{"PATH": "/feat", "FEAT_VAR": "v"}},
	})
	want := map[string]string{"PATH": "/user", "USER_VAR": "u", "FEAT_VAR": "v"}
	if !reflect.DeepEqual(cfg.ContainerEnv, want) {
		t.Errorf("got %v, want %v", cfg.ContainerEnv, want)
	}
}

func TestMergeMetadata_CapAdd_UnionDedup(t *testing.T) {
	cfg := &ResolvedConfig{CapAdd: []string{"SYS_PTRACE"}}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "a", CapAdd: []string{"NET_ADMIN", "SYS_PTRACE"}},
		{ID: "b", CapAdd: []string{"NET_ADMIN", "SYS_ADMIN"}},
	})
	want := []string{"NET_ADMIN", "SYS_PTRACE", "SYS_ADMIN"}
	if !reflect.DeepEqual(cfg.CapAdd, want) {
		t.Errorf("got %v, want %v", cfg.CapAdd, want)
	}
}

func TestMergeMetadata_LifecycleHooks_OrderedAndUserLast(t *testing.T) {
	cfg := &ResolvedConfig{
		Lifecycle: LifecycleCommands{
			PostCreate: []LifecycleCommand{{Single: &Command{Shell: "user"}}},
		},
	}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "base", PostCreateCommand: LifecycleCommand{Single: &Command{Shell: "base"}}},
		{ID: "feat", PostCreateCommand: LifecycleCommand{Single: &Command{Shell: "feat"}}},
	})
	if len(cfg.Lifecycle.PostCreate) != 3 {
		t.Fatalf("want 3 hooks, got %+v", cfg.Lifecycle.PostCreate)
	}
	got := []string{
		cfg.Lifecycle.PostCreate[0].Single.Shell,
		cfg.Lifecycle.PostCreate[1].Single.Shell,
		cfg.Lifecycle.PostCreate[2].Single.Shell,
	}
	want := []string{"base", "feat", "user"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeMetadata_LifecycleHooks_SkipEmpty(t *testing.T) {
	cfg := &ResolvedConfig{}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "a", PostCreateCommand: LifecycleCommand{}},
		{ID: "b", PostCreateCommand: LifecycleCommand{Single: &Command{Shell: "b"}}},
	})
	if len(cfg.Lifecycle.PostCreate) != 1 {
		t.Errorf("want 1 hook (empty skipped), got %+v", cfg.Lifecycle.PostCreate)
	}
}

func TestMergeMetadata_Mounts_TargetDedup_UserWins(t *testing.T) {
	cfg := &ResolvedConfig{
		Mounts: []Mount{{Type: MountBind, Source: "/host/user", Target: "/data"}},
	}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "a", Mounts: []Mount{
			{Type: MountVolume, Source: "feat-vol", Target: "/data"},
			{Type: MountBind, Source: "/host/feat", Target: "/feat-only"},
		}},
	})
	// /data: user mount wins (last). /feat-only: feature mount survives.
	if len(cfg.Mounts) != 2 {
		t.Fatalf("want 2 mounts, got %+v", cfg.Mounts)
	}
	var dataM Mount
	for _, m := range cfg.Mounts {
		if m.Target == "/data" {
			dataM = m
		}
	}
	if dataM.Source != "/host/user" {
		t.Errorf("user mount on /data should win, got source %q", dataM.Source)
	}
}

func TestMergeMetadata_HostRequirements_FillsIfNil(t *testing.T) {
	cfg := &ResolvedConfig{}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "a", HostRequirements: &HostRequirements{CPUs: 4}},
	})
	if cfg.HostRequirements == nil || cfg.HostRequirements.CPUs != 4 {
		t.Errorf("HostRequirements should fill, got %+v", cfg.HostRequirements)
	}
}

func TestMergeMetadata_HostRequirements_UserWins(t *testing.T) {
	cfg := &ResolvedConfig{HostRequirements: &HostRequirements{CPUs: 8}}
	MergeMetadata(cfg, []FeatureMetadata{
		{ID: "a", HostRequirements: &HostRequirements{CPUs: 4}},
	})
	if cfg.HostRequirements.CPUs != 8 {
		t.Errorf("user HostRequirements should win, got %+v", cfg.HostRequirements)
	}
}

func TestMergeMetadata_NilCfgOrEmptyLayers_NoOp(t *testing.T) {
	MergeMetadata(nil, []FeatureMetadata{{RemoteUser: "x"}})
	cfg := &ResolvedConfig{RemoteUser: "u"}
	MergeMetadata(cfg, nil)
	if cfg.RemoteUser != "u" {
		t.Errorf("empty layers should not mutate cfg")
	}
}

// MergeMetadata is idempotent for scalar / dedup'd fields. Slice-append
// fields (lifecycle hooks) are *not* idempotent — they reflect "ran twice
// = appended twice" by design, since the merge has no way to distinguish
// "user already merged this layer" from "user wrote the same hook
// themselves." The contract is one merge per cfg lifetime; this test
// verifies the dedup'd / scalar fields behave idempotently regardless.
func TestMergeMetadata_DedupFieldsIdempotent(t *testing.T) {
	layers := []FeatureMetadata{
		{ID: "base", RemoteUser: "vscode", CapAdd: []string{"SYS_PTRACE"}, ContainerEnv: map[string]string{"K": "v"}},
	}
	cfg1 := &ResolvedConfig{}
	MergeMetadata(cfg1, layers)
	cfg2 := &ResolvedConfig{}
	MergeMetadata(cfg2, layers)
	MergeMetadata(cfg2, layers) // run twice
	if cfg1.RemoteUser != cfg2.RemoteUser {
		t.Errorf("RemoteUser drift: %q vs %q", cfg1.RemoteUser, cfg2.RemoteUser)
	}
	if !reflect.DeepEqual(cfg1.CapAdd, cfg2.CapAdd) {
		t.Errorf("CapAdd drift: %v vs %v", cfg1.CapAdd, cfg2.CapAdd)
	}
	if !reflect.DeepEqual(cfg1.ContainerEnv, cfg2.ContainerEnv) {
		t.Errorf("ContainerEnv drift: %v vs %v", cfg1.ContainerEnv, cfg2.ContainerEnv)
	}
}

func TestFinalize_Defaults(t *testing.T) {
	cfg := &ResolvedConfig{}
	cfg.Finalize()
	if !BoolOr(cfg.OverrideCommand, false) {
		t.Error("OverrideCommand should default to true")
	}
	if cfg.UserEnvProbe != UserEnvProbeLoginInteractive {
		t.Errorf("UserEnvProbe = %q", cfg.UserEnvProbe)
	}
	if cfg.WaitFor != LifecyclePostCreate {
		t.Errorf("WaitFor = %q", cfg.WaitFor)
	}
}

func TestFinalize_WaitForUpdateContent(t *testing.T) {
	cfg := &ResolvedConfig{
		Lifecycle: LifecycleCommands{
			UpdateContent: []LifecycleCommand{{Single: &Command{Shell: "x"}}},
		},
	}
	cfg.Finalize()
	if cfg.WaitFor != LifecycleUpdateContent {
		t.Errorf("WaitFor = %q, want updateContent", cfg.WaitFor)
	}
}

func TestFinalize_RespectsExplicitValues(t *testing.T) {
	cfg := &ResolvedConfig{
		OverrideCommand: ptrBool(false),
		UserEnvProbe:    UserEnvProbeNone,
		WaitFor:         LifecyclePostStart,
	}
	cfg.Finalize()
	if BoolOr(cfg.OverrideCommand, true) {
		t.Error("explicit OverrideCommand=false must survive Finalize")
	}
	if cfg.UserEnvProbe != UserEnvProbeNone {
		t.Errorf("UserEnvProbe = %q", cfg.UserEnvProbe)
	}
	if cfg.WaitFor != LifecyclePostStart {
		t.Errorf("WaitFor = %q", cfg.WaitFor)
	}
}
