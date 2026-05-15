import ContainerAPIClient
import Foundation

// PR-G scope: builder-liveness probe only. The full BuildKit gRPC
// integration (dial buildkit container → Builder(socket:) → BuildConfig
// construction → progress event translation) is a separate
// follow-up. This stub at least gives callers an actionable error:
// either "builder not running, start with `container builder start`"
// or "build not implemented yet on this backend."
//
// Why a stub rather than a working implementation now: the build
// surface is large (Builder.BuildConfig has ~17 fields), the
// progress-streaming path requires implementing a custom Terminal
// protocol or scraping the buildkit gRPC events, and SwiftNIO event-
// loop management leaks into the bridge. Doing it right is a PR
// unto itself.

// Apple's buildkit container's vsock port — copied from the CLI's
// constants (it uses a -p flag but defaults to this).
private let buildkitVsockPort: UInt32 = 8088
private let buildkitContainerID = "buildkit"

@_cdecl("ac_build_probe")
public func ac_build_probe() -> UnsafePointer<CChar>? {
    return runSync(timeoutSeconds: 5) {
        do {
            let client = ContainerClient()
            // Just verify the buildkit container exists and is
            // running. We don't actually dial the vsock here — that
            // would require SwiftNIO and is the territory of the
            // real build implementation. ContainerClient.get returns
            // the snapshot, and we check status.
            let snap = try await client.get(id: buildkitContainerID)
            if snap.status == .running {
                return "{\"ok\":true}"
            }
            return "{\"ok\":false,\"err\":\"builder container exists but status is \\(\"\(snap.status)\\\"\"}"
        } catch {
            // Includes the case where the buildkit container doesn't
            // exist at all (never started).
            return encodeErr(error)
        }
    }
}
