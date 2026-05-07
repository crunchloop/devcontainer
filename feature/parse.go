package feature

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crunchloop/devcontainer/config"
)

// metadataFile is the file inside a feature directory containing the
// feature's typed metadata.
const metadataFile = "devcontainer-feature.json"

// installScript is the script every feature must provide and we invoke
// to install it. Verified at parse time so we fail with a clear message
// instead of opaque mid-build errors.
const installScript = "install.sh"

// parseMetadata reads <dir>/devcontainer-feature.json into a
// config.FeatureMetadata. Missing fields are tolerated (zero value);
// JSON parse errors are returned with the file path included.
func parseMetadata(dir string) (config.FeatureMetadata, error) {
	path := filepath.Join(dir, metadataFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return config.FeatureMetadata{}, fmt.Errorf("read %s: %w", path, err)
	}
	var raw rawFeatureMetadata
	if err := json.Unmarshal(data, &raw); err != nil {
		return config.FeatureMetadata{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return raw.toResolved(), nil
}

// rawFeatureMetadata mirrors devcontainer-feature.json on the wire. We
// keep this internal because the public type lives in config; the raw
// form here only needs to round-trip the on-disk JSON.
type rawFeatureMetadata struct {
	ID               string                            `json:"id"`
	Version          string                            `json:"version,omitempty"`
	Name             string                            `json:"name,omitempty"`
	Description      string                            `json:"description,omitempty"`
	DocumentationURL string                            `json:"documentationURL,omitempty"`
	LicenseURL       string                            `json:"licenseURL,omitempty"`
	Options          map[string]rawFeatureOption       `json:"options,omitempty"`
	ContainerEnv     map[string]string                 `json:"containerEnv,omitempty"`
	Init             *bool                             `json:"init,omitempty"`
	Privileged       *bool                             `json:"privileged,omitempty"`
	CapAdd           []string                          `json:"capAdd,omitempty"`
	SecurityOpt      []string                          `json:"securityOpt,omitempty"`
	Entrypoint       string                            `json:"entrypoint,omitempty"`
	InstallsAfter    []string                          `json:"installsAfter,omitempty"`
	DependsOn        map[string]map[string]any         `json:"dependsOn,omitempty"`
	Customizations   map[string]json.RawMessage        `json:"customizations,omitempty"`
}

type rawFeatureOption struct {
	Type        string `json:"type,omitempty"`
	Default     any    `json:"default,omitempty"`
	Enum        []any  `json:"enum,omitempty"`
	Proposals   []any  `json:"proposals,omitempty"`
	Description string `json:"description,omitempty"`
}

func (r rawFeatureMetadata) toResolved() config.FeatureMetadata {
	out := config.FeatureMetadata{
		ID:               r.ID,
		Version:          r.Version,
		Name:             r.Name,
		Description:      r.Description,
		DocumentationURL: r.DocumentationURL,
		LicenseURL:       r.LicenseURL,
		ContainerEnv:     r.ContainerEnv,
		Init:             r.Init,
		Privileged:       r.Privileged,
		CapAdd:           r.CapAdd,
		SecurityOpt:      r.SecurityOpt,
		Entrypoint:       r.Entrypoint,
		InstallsAfter:    r.InstallsAfter,
		DependsOn:        r.DependsOn,
		Customizations:   r.Customizations,
	}
	if len(r.Options) > 0 {
		out.Options = make(map[string]config.FeatureOption, len(r.Options))
		for k, v := range r.Options {
			out.Options[k] = config.FeatureOption{
				Type:        v.Type,
				Default:     v.Default,
				Enum:        v.Enum,
				Proposals:   v.Proposals,
				Description: v.Description,
			}
		}
	}
	return out
}
