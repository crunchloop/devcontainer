package config

import (
	"encoding/json"

	"github.com/tidwall/jsonc"
)

// parseRaw decodes a devcontainer.json document. Comments (line and block)
// and trailing commas are tolerated via the JSONC pre-processor.
//
// path is used only for error messages and is otherwise opaque to the
// parser.
func parseRaw(src []byte, path string) (*rawConfig, error) {
	cleaned := jsonc.ToJSON(src)
	var raw rawConfig
	if err := json.Unmarshal(cleaned, &raw); err != nil {
		return nil, &ConfigParseError{Path: path, Err: err}
	}
	return &raw, nil
}
