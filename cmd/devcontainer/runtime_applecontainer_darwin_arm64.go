//go:build darwin && arm64

package main

import (
	"context"
	"fmt"

	"github.com/crunchloop/devcontainer/runtime"
	"github.com/crunchloop/devcontainer/runtime/applecontainer"
)

func newAppleContainerRuntime(ctx context.Context) (runtime.Runtime, func(), error) {
	rt, err := applecontainer.New(ctx, applecontainer.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("applecontainer runtime: %w", err)
	}
	return rt, func() {}, nil
}
