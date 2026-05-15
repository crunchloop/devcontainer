// Package applecontainer is an Apple `container` implementation of
// runtime.Runtime targeting macOS 15+ on arm64.
//
// The runtime is a thin cgo wrapper around libACBridge.dylib, a Swift
// dynamic library that imports apple/container's ContainerAPIClient and
// speaks XPC to the system container-apiserver daemon. The daemon is
// installed via `brew install container` and started with
// `container system start`. New returns a *runtime.DaemonUnavailableError
// if the daemon is not reachable.
//
// Only darwin/arm64 builds compile against the bridge. Other platforms
// link a stub that returns "platform unsupported" from every constructor;
// the package itself is importable from any build so callers can keep
// platform-agnostic wiring.
//
// PR-A scope: New + Ping only. Every other Runtime method returns
// runtime.ErrNotImplemented and is filled in by PR-B onward (see
// design/status.md M6).
//
// See design/runtime-applecontainer.md for the full architecture.
package applecontainer
