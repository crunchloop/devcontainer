import ContainerAPIClient
import ContainerizationOCI
import ContainerResource
import Foundation

// PR-B default for inspect/list-style sync calls — kept tight because
// these XPC round-trips are local and fast. Bumps in future PRs can
// expose a timeout argument if needed.
private let inspectTimeoutSeconds = 10

// ===== ac_inspect_container ==========================================

@_cdecl("ac_inspect_container")
public func ac_inspect_container(_ idPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let id = readCString(idPtr) else { return dupNullArgErr("id") }
    return runSync(timeoutSeconds: inspectTimeoutSeconds) {
        do {
            let snap = try await ContainerClient().get(id: id)
            return encodeOK(snap)
        } catch {
            return encodeErr(error)
        }
    }
}

// ===== ac_inspect_image ==============================================

// ImageInspectPayload is the projection we return for an image
// inspect — flattened from the OCI Image + ImageConfig so the Go side
// gets a single object to unmarshal. Kept narrow (only the fields the
// Runtime interface needs); add more as later PRs require them.
private struct ImageInspectPayload: Encodable {
    let reference: String
    let digest: String
    let architecture: String?
    let os: String?
    let user: String?
    let env: [String]?
    let labels: [String: String]?
}

@_cdecl("ac_inspect_image")
public func ac_inspect_image(_ refPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let ref = readCString(refPtr) else { return dupNullArgErr("reference") }
    return runSync(timeoutSeconds: inspectTimeoutSeconds) {
        do {
            let img = try await ClientImage.get(reference: ref)
            let ociImage: ContainerizationOCI.Image = try await img.config(for: .current)
            let payload = ImageInspectPayload(
                reference: img.description.reference,
                digest: img.description.digest,
                architecture: ociImage.architecture,
                os: ociImage.os,
                user: ociImage.config?.user,
                env: ociImage.config?.env,
                labels: ociImage.config?.labels
            )
            return encodeOK(payload)
        } catch {
            return encodeErr(error)
        }
    }
}

// ===== ac_find_container_by_label ====================================

@_cdecl("ac_find_container_by_label")
public func ac_find_container_by_label(
    _ keyPtr: UnsafePointer<CChar>?,
    _ valuePtr: UnsafePointer<CChar>?
) -> UnsafePointer<CChar>? {
    guard let key = readCString(keyPtr) else { return dupNullArgErr("key") }
    guard let value = readCString(valuePtr) else { return dupNullArgErr("value") }
    return runSync(timeoutSeconds: inspectTimeoutSeconds) {
        do {
            let all = try await ContainerClient().list()
            let matches = all.filter { $0.configuration.labels[key] == value }
            // Most-recently-started wins, matching the contract on
            // runtime.FindContainerByLabel. startedDate is optional in
            // Apple's snapshot; nil sorts to the bottom.
            let pick = matches.max { lhs, rhs in
                let l = lhs.startedDate ?? Date.distantPast
                let r = rhs.startedDate ?? Date.distantPast
                return l < r
            }
            if let pick {
                return encodeOK(pick)
            }
            return encodeOKNull()
        } catch {
            return encodeErr(error)
        }
    }
}
