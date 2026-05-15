import ContainerAPIClient
import Foundation

// ===== ac_version =====================================================

@_cdecl("ac_version")
public func ac_version() -> UnsafePointer<CChar>? {
    let s = "ACBridge/\(bridgeBuildVersion) apple-container/\(applePinnedVersion)"
    return UnsafePointer(strdup(s))
}

// ===== ac_ping ========================================================

@_cdecl("ac_ping")
public func ac_ping(_ timeoutSeconds: Int32) -> UnsafePointer<CChar>? {
    let seconds = Int(timeoutSeconds <= 0 ? 5 : timeoutSeconds)
    return runSync(timeoutSeconds: seconds) {
        do {
            let h = try await ClientHealthCheck.ping(timeout: .seconds(seconds))
            return encodePingOK(h)
        } catch {
            return encodeErr(error)
        }
    }
}

private func encodePingOK(_ h: SystemHealth) -> String {
    // ac_ping predates the style guide's canonical {ok, data} envelope
    // and ships SystemHealth's fields at the top level. Kept as-is for
    // PR-A stability; new exports use encodeOK(...) instead.
    let payload: [String: Any] = [
        "ok": true,
        "apiServerVersion": h.apiServerVersion,
        "apiServerBuild": h.apiServerBuild,
        "apiServerCommit": h.apiServerCommit,
        "appRoot": h.appRoot.path,
        "installRoot": h.installRoot.path,
    ]
    guard let data = try? JSONSerialization.data(withJSONObject: payload),
          let s = String(data: data, encoding: .utf8)
    else {
        return "{\"ok\":true}"
    }
    return s
}

// ===== ac_free ========================================================

@_cdecl("ac_free")
public func ac_free(_ p: UnsafeMutableRawPointer?) {
    free(p)
}
