// Package docker is a Docker Engine API implementation of runtime.Runtime.
//
// It uses github.com/moby/moby/client (the canonical post-split SDK
// path). We chose moby over github.com/docker/docker/client to favor
// the upstream module that the broader Go container ecosystem is
// converging on, avoiding two near-duplicate transitive trees when
// downstream consumers also depend on moby/moby/api.
//
// All methods accept context.Context; streaming endpoints (PullImage,
// BuildImage, follow logs) are wrapped with runtime.CancellableCopy so
// ctx cancellation always returns within milliseconds.
package docker
