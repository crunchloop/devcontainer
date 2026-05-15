import ContainerAPIClient
import ContainerizationOCI
import Foundation

// PR-F scope: synchronous pull. Apple's ClientImage.pull returns when
// the entire image is fetched + unpacked. Progress streaming is left
// for a future PR — the Runtime interface accepts a BuildEvent
// channel, but the engine treats coarse "started / completed" as
// acceptable for v1. See design/runtime-applecontainer.md §8.
//
// Cancellation: not yet wired. Apple's pull API doesn't expose a
// cancellation token; aborting a pull cleanly would require deleting
// the partial image, which is risky if other pulls share the same
// content store. Documented limitation; revisit when DAP needs it.

private struct PullResult: Encodable {
    let reference: String
    let digest: String
}

// 30 minutes — covers a cold pull of a multi-GB base image on
// reasonable networks. The bridge will trip its own timeout before
// this fires on most realistic links.
private let pullTimeoutSeconds = 1800

@_cdecl("ac_pull_image")
public func ac_pull_image(_ refPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let ref = readCString(refPtr) else { return dupNullArgErr("reference") }
    return runSync(timeoutSeconds: pullTimeoutSeconds) {
        do {
            let img = try await ClientImage.pull(reference: ref, platform: .current)
            return encodeOK(PullResult(
                reference: img.description.reference,
                digest: img.description.digest
            ))
        } catch {
            return encodeErr(error)
        }
    }
}
