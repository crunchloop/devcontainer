//go:build !(darwin && arm64)

package main

import (
	"context"
	"fmt"

	"github.com/crunchloop/devcontainer/runtime"
)

func newAppleContainerRuntime(_ context.Context) (runtime.Runtime, func(), error) {
	return nil, nil, fmt.Errorf("applecontainer runtime is only supported on darwin/arm64")
}
