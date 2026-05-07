package feature

import (
	"errors"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
)

func feat(ref string, installsAfter ...string) config.ResolvedFeature {
	return config.ResolvedFeature{
		Ref: ref,
		Metadata: config.FeatureMetadata{
			ID:            normalizeID(ref),
			InstallsAfter: installsAfter,
		},
	}
}

func featDeps(ref string, depsOn ...string) config.ResolvedFeature {
	deps := make(map[string]map[string]any, len(depsOn))
	for _, d := range depsOn {
		deps[d] = nil
	}
	return config.ResolvedFeature{
		Ref: ref,
		Metadata: config.FeatureMetadata{
			ID:        normalizeID(ref),
			DependsOn: deps,
		},
	}
}

func refsOf(features []config.ResolvedFeature) []string {
	out := make([]string, len(features))
	for i, f := range features {
		out[i] = f.Ref
	}
	return out
}

func TestOrder_NoDependencies_AlphabeticalAndDeterministic(t *testing.T) {
	in := []config.ResolvedFeature{feat("zzz"), feat("aaa"), feat("mmm")}
	got, _, err := Order(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aaa", "mmm", "zzz"}
	if !equalRefs(refsOf(got), want) {
		t.Errorf("got %v, want %v", refsOf(got), want)
	}
}

func TestOrder_InstallsAfter(t *testing.T) {
	// node installsAfter [common-utils, git]
	// → common-utils and git come before node, alphabetical between
	//   themselves.
	in := []config.ResolvedFeature{
		feat("node", "common-utils", "git"),
		feat("git"),
		feat("common-utils"),
	}
	got, _, err := Order(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"common-utils", "git", "node"}
	if !equalRefs(refsOf(got), want) {
		t.Errorf("got %v, want %v", refsOf(got), want)
	}
}

func TestOrder_DependsOn(t *testing.T) {
	in := []config.ResolvedFeature{
		featDeps("python", "common-utils"),
		feat("common-utils"),
	}
	got, _, err := Order(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"common-utils", "python"}
	if !equalRefs(refsOf(got), want) {
		t.Errorf("got %v, want %v", refsOf(got), want)
	}
}

func TestOrder_Cycle(t *testing.T) {
	in := []config.ResolvedFeature{
		feat("a", "b"),
		feat("b", "a"),
	}
	_, _, err := Order(in, nil)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	var ce *FeatureCycleError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *FeatureCycleError, got %T: %v", err, err)
	}
	if !strings.Contains(strings.Join(ce.Path, " "), "a") || !strings.Contains(strings.Join(ce.Path, " "), "b") {
		t.Errorf("cycle path should mention both nodes: %v", ce.Path)
	}
}

func TestOrder_OverrideFeatureInstallOrder(t *testing.T) {
	// Override puts git first, then alphabetical.
	in := []config.ResolvedFeature{
		feat("zzz"),
		feat("git"),
		feat("aaa"),
	}
	got, _, err := Order(in, []string{"git"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"git", "aaa", "zzz"}
	if !equalRefs(refsOf(got), want) {
		t.Errorf("got %v, want %v", refsOf(got), want)
	}
}

func TestOrder_OverrideMatchesByID_NotTag(t *testing.T) {
	// User-supplied features have tags; overrideOrder may not.
	in := []config.ResolvedFeature{
		feat("ghcr.io/devcontainers/features/node:1"),
		feat("ghcr.io/devcontainers/features/git:1"),
	}
	got, _, err := Order(in, []string{"ghcr.io/devcontainers/features/git"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Ref != "ghcr.io/devcontainers/features/git:1" {
		t.Errorf("got %v, want git first", refsOf(got))
	}
}

func TestOrder_DepthWarn_AtSoftLimit(t *testing.T) {
	// Build a chain of length 17 (depth 16). Should warn but succeed.
	const N = 17
	in := make([]config.ResolvedFeature, N)
	for i := 0; i < N; i++ {
		ref := indexRef(i)
		var after []string
		if i > 0 {
			after = []string{indexRef(i - 1)}
		}
		in[i] = feat(ref, after...)
	}
	_, warns, err := Order(in, nil)
	if err != nil {
		t.Fatalf("Order: %v", err)
	}
	found := false
	for _, w := range warns {
		if w.Code == config.WarnDeepFeatureChain {
			found = true
		}
	}
	if !found {
		t.Errorf("expected WarnDeepFeatureChain at chain length 17, got %v", warns)
	}
}

func TestOrder_DepthError_AtHardLimit(t *testing.T) {
	// Build a chain of length 65 (depth 64). Should error.
	const N = 65
	in := make([]config.ResolvedFeature, N)
	for i := 0; i < N; i++ {
		var after []string
		if i > 0 {
			after = []string{indexRef(i - 1)}
		}
		in[i] = feat(indexRef(i), after...)
	}
	_, _, err := Order(in, nil)
	if err == nil {
		t.Fatal("expected FeatureDAGTooDeepError")
	}
	var dErr *FeatureDAGTooDeepError
	if !errors.As(err, &dErr) {
		t.Fatalf("expected *FeatureDAGTooDeepError, got %T: %v", err, err)
	}
}

func TestOrder_PartialMetadataDegrades(t *testing.T) {
	// Features with empty Metadata (pre-fetch) should still order
	// alphabetically without errors.
	in := []config.ResolvedFeature{
		{Ref: "ghcr.io/x/zzz:1"},
		{Ref: "ghcr.io/x/aaa:1"},
	}
	got, _, err := Order(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Ref != "ghcr.io/x/aaa:1" {
		t.Errorf("got %v, want aaa first", refsOf(got))
	}
}

func indexRef(i int) string {
	// 3-letter zero-padded ref so alphabetical sort matches numerical.
	return "f" + pad3(i)
}

func pad3(n int) string {
	s := ""
	for i := 0; i < 3; i++ {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func equalRefs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
