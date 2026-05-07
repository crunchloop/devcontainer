// Package docker is a Docker Engine API implementation of runtime.Runtime.
//
// It uses github.com/moby/moby/client (the canonical post-split SDK path).
// We chose moby over github.com/docker/docker/client to align with DAP's
// existing dependency on github.com/moby/moby/api, avoiding two
// near-duplicate transitive trees in the consumer's go.mod.
//
// All methods accept context.Context; streaming endpoints (PullImage,
// BuildImage, follow logs) are wrapped with runtime.CancellableCopy so
// ctx cancellation always returns within milliseconds.
package docker
