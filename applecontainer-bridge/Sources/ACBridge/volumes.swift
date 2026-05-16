import ContainerAPIClient
import ContainerizationError
import Foundation

// Compose orchestrator primitives — apple/container 0.12 volume
// surface. Apple's volumes are ext4-on-disk-image and exclusively
// mounted: probe 4 in design/compose-native.md confirmed
// multi-attach fails at the VM layer. The orchestrator's Plan
// validator refuses shared volumes on apple (via the SharedVolumes
// capability flag); these primitives only handle the simple
// single-mount case.

private let volumeTimeoutSeconds = 15

private struct VolumeSpecJSON: Decodable {
    var name: String
    var labels: [String: String]?
    var driver: String?
    var options: [String: String]?
}

private struct VolumeResultData: Encodable {
    let name: String
}

@_cdecl("ac_volume_create")
public func ac_volume_create(_ specPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let specStr = readCString(specPtr) else { return dupNullArgErr("spec") }
    return runSync(timeoutSeconds: volumeTimeoutSeconds) {
        do {
            guard let data = specStr.data(using: .utf8) else {
                return "{\"ok\":false,\"err\":\"spec not utf8\"}"
            }
            let spec = try JSONDecoder().decode(VolumeSpecJSON.self, from: data)
            guard !spec.name.isEmpty else {
                return "{\"ok\":false,\"err\":\"VolumeSpec.Name is required\"}"
            }

            // Idempotency on (name, label superset).
            if let existing = try? await ClientVolume.inspect(spec.name) {
                if labelsSupersetVol(existing.labels, want: spec.labels ?? [:]) {
                    return encodeOK(VolumeResultData(name: existing.name))
                }
            }

            let driver = (spec.driver?.isEmpty == false) ? spec.driver! : "local"
            let created = try await ClientVolume.create(
                name: spec.name,
                driver: driver,
                driverOpts: spec.options ?? [:],
                labels: spec.labels ?? [:]
            )
            return encodeOK(VolumeResultData(name: created.name))
        } catch {
            return encodeErr(error)
        }
    }
}

@_cdecl("ac_volume_remove")
public func ac_volume_remove(_ namePtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let name = readCString(namePtr) else { return dupNullArgErr("name") }
    return runSync(timeoutSeconds: volumeTimeoutSeconds) {
        do {
            try await ClientVolume.delete(name: name)
            return "{\"ok\":true}"
        } catch let e as ContainerizationError where e.code == .notFound {
            return "{\"ok\":true}"
        } catch {
            return encodeErr(error)
        }
    }
}

private func labelsSupersetVol(_ have: [String: String], want: [String: String]) -> Bool {
    for (k, v) in want {
        if have[k] != v { return false }
    }
    return true
}
