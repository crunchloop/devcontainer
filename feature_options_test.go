package devcontainer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crunchloop/devcontainer/config"
	"github.com/crunchloop/devcontainer/runtime"
)

// buildContextRuntime is a fakeRuntime whose BuildImage keeps the
// first feature's feature.env from the build context it is handed,
// then fails as the fake does.
type buildContextRuntime struct {
	*fakeRuntime
	featureEnv []byte
}

func (r *buildContextRuntime) BuildImage(ctx context.Context, spec runtime.BuildSpec, events chan<- runtime.BuildEvent) (runtime.ImageRef, error) {
	env, err := os.ReadFile(filepath.Join(spec.ContextPath, "build-context", "0", "feature.env"))
	if err == nil {
		r.featureEnv = env
	}
	return r.fakeRuntime.BuildImage(ctx, spec, events)
}

// A string option given a JSON array, as in apt-packages' "packages",
// is installed with the comma-joined value the reference CLI writes.
func TestLayerFeatures_ArrayOptionReachesFeatureEnvJoined(t *testing.T) {
	f := newDepFixture(t, `{"ghcr.io/x/apt-packages:1":{"packages":["jq","tree"]}}`, map[string]depEntry{
		"ghcr.io/x/apt-packages": {digestOf('a'), config.FeatureMetadata{
			ID:      "apt-packages",
			Version: "1.0.0",
			Options: map[string]config.FeatureOption{"packages": {Type: "string", Default: ""}},
		}},
	})
	rt := &buildContextRuntime{fakeRuntime: f.rt}
	eng, err := New(EngineOptions{Runtime: rt, FeatureStore: f.store})
	if err != nil {
		t.Fatal(err)
	}
	f.eng = eng

	cfg, err := f.layer(LockfileDisabled)
	if err != nil {
		t.Fatalf("layerFeatures: %v", err)
	}
	if got := string(rt.featureEnv); !strings.Contains(got, "PACKAGES='jq,tree'\n") {
		t.Errorf("feature.env = %q, want PACKAGES='jq,tree'", got)
	}
	found := false
	for _, w := range cfg.Warnings {
		if w.Code == config.WarnFeatureOptionCoerced {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a %s warning, got %v", config.WarnFeatureOptionCoerced, cfg.Warnings)
	}
}
