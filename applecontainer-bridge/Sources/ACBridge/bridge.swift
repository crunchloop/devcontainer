import ContainerAPIClient
import Foundation

private let bridgeVersion = "0.1.0"
private let applePinnedVersion = "0.12.3"

@_cdecl("ac_version")
public func ac_version() -> UnsafePointer<CChar>? {
    let s = "ACBridge/\(bridgeVersion) apple-container/\(applePinnedVersion)"
    return UnsafePointer(strdup(s))
}

@_cdecl("ac_ping")
public func ac_ping(_ timeoutSeconds: Int32) -> UnsafePointer<CChar>? {
    let seconds = Int(timeoutSeconds <= 0 ? 5 : timeoutSeconds)
    let timeout: Duration = .seconds(seconds)
    let sem = DispatchSemaphore(value: 0)
    nonisolated(unsafe) var json = "{\"ok\":false,\"err\":\"unset\"}"

    Task {
        defer { sem.signal() }
        do {
            let h = try await ClientHealthCheck.ping(timeout: timeout)
            json = encodePingOK(h)
        } catch {
            json = encodePingErr(error)
        }
    }
    // ClientHealthCheck.ping already enforces its own Duration timeout,
    // but guard the semaphore wait as belt-and-suspenders: if the Task
    // never signals (cancellation race, runtime hang), we return a
    // deterministic error instead of blocking the cgo caller forever.
    let waitResult = sem.wait(timeout: .now() + .seconds(seconds + 2))
    if waitResult == .timedOut {
        return UnsafePointer(strdup("{\"ok\":false,\"err\":\"bridge-timeout\"}"))
    }
    return UnsafePointer(strdup(json))
}

@_cdecl("ac_free")
public func ac_free(_ p: UnsafeMutableRawPointer?) {
    free(p)
}

private func encodePingOK(_ h: SystemHealth) -> String {
    let payload: [String: Any] = [
        "ok": true,
        "apiServerVersion": h.apiServerVersion,
        "apiServerBuild": h.apiServerBuild,
        "apiServerCommit": h.apiServerCommit,
        "appRoot": h.appRoot.path,
        "installRoot": h.installRoot.path,
    ]
    return jsonString(payload, fallback: "{\"ok\":true}")
}

private func encodePingErr(_ error: Error) -> String {
    let payload: [String: Any] = [
        "ok": false,
        "err": String(describing: error),
    ]
    return jsonString(payload, fallback: "{\"ok\":false,\"err\":\"encode-failed\"}")
}

private func jsonString(_ payload: [String: Any], fallback: String) -> String {
    guard let data = try? JSONSerialization.data(withJSONObject: payload),
          let s = String(data: data, encoding: .utf8)
    else {
        return fallback
    }
    return s
}
