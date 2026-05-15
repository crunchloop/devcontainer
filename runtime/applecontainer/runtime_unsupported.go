//go:build !(darwin && arm64)

package applecontainer

import (
	"context"
	"errors"
)

// Runtime is the apple-container backend handle. On non-darwin/arm64
// platforms it is unconstructable — New always returns an error.
type Runtime struct{}

// Options configure New.
type Options struct {
	// PingTimeoutSeconds bounds the daemon-health probe in New. Zero
	// uses the bridge default (5s).
	PingTimeoutSeconds int
}

// PingResult is the parsed result of a daemon health-check probe.
type PingResult struct {
	APIServerVersion string `json:"apiServerVersion"`
	APIServerBuild   string `json:"apiServerBuild"`
	APIServerCommit  string `json:"apiServerCommit"`
	AppRoot          string `json:"appRoot"`
	InstallRoot      string `json:"installRoot"`
}

// New always returns an unsupported-platform error off darwin/arm64.
func New(_ context.Context, _ Options) (*Runtime, error) {
	return nil, errors.New("applecontainer: only supported on darwin/arm64")
}
