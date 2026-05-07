package config

import "fmt"

// ConfigParseError indicates that a devcontainer.json file could not be
// parsed (JSONC syntax invalid, schema mismatch, etc.).
type ConfigParseError struct {
	Path string
	Err  error
}

func (e *ConfigParseError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("parse %s: %v", e.Path, e.Err)
	}
	return fmt.Sprintf("parse devcontainer.json: %v", e.Err)
}

func (e *ConfigParseError) Unwrap() error { return e.Err }

// ConfigInvalidError indicates that a parsed config violates a structural
// rule that prevents producing a ResolvedConfig (e.g. neither image, build,
// nor dockerComposeFile is set).
type ConfigInvalidError struct {
	Path    string
	Message string
}

func (e *ConfigInvalidError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("invalid devcontainer config %s: %s", e.Path, e.Message)
	}
	return "invalid devcontainer config: " + e.Message
}
