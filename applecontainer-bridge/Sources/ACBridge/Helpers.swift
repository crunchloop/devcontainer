import Foundation

let bridgeBuildVersion = "0.1.0"
let applePinnedVersion = "0.12.3"

// runSync runs an async closure on a Task and blocks the calling cgo
// thread on a DispatchSemaphore until it completes, with a hard wait
// timeout to prevent a misbehaving Task from hanging the caller
// forever. Every export that wraps an apple/container async call goes
// through this so the cgo-thread-blocking shape stays consistent.
//
// The closure must catch its own errors and encode them into the
// returned JSON envelope; runSync only handles the wait-timeout case
// itself.
//
// timeoutSeconds is the upper bound on the inner async work. The
// semaphore is given a `timeoutSeconds + 2` slack so the inner op's
// own timeout fires first and produces a typed error rather than the
// generic `bridge-timeout` fallback.
func runSync(timeoutSeconds: Int, _ op: @Sendable @escaping () async -> String) -> UnsafePointer<CChar>? {
    let sem = DispatchSemaphore(value: 0)
    nonisolated(unsafe) var json = "{\"ok\":false,\"err\":\"unset\"}"
    Task {
        defer { sem.signal() }
        json = await op()
    }
    let result = sem.wait(timeout: .now() + .seconds(timeoutSeconds + 2))
    if result == .timedOut {
        return UnsafePointer(strdup("{\"ok\":false,\"err\":\"bridge-timeout\"}"))
    }
    return UnsafePointer(strdup(json))
}

// bridgeEncoder configures every payload going to Go with ISO8601
// dates so Go's encoding/json time.Time decoder accepts them. Apple's
// default (secondsSince2001Jan1) ships dates as numbers, which would
// force the Go side to special-case every Date field.
private let bridgeEncoder: JSONEncoder = {
    let enc = JSONEncoder()
    enc.dateEncodingStrategy = .iso8601
    return enc
}()

// encodeOK wraps an Encodable payload in the canonical envelope:
//   { "ok": true, "data": <payload> }
func encodeOK<T: Encodable>(_ value: T) -> String {
    do {
        let data = try bridgeEncoder.encode(value)
        guard let inner = String(data: data, encoding: .utf8) else {
            return "{\"ok\":false,\"err\":\"utf8 encoding failed\"}"
        }
        return "{\"ok\":true,\"data\":\(inner)}"
    } catch {
        return encodeErr(error)
    }
}

// encodeOKNull is the success-with-no-payload form: callers like
// "find by label" use it to signal "looked, found nothing" distinct
// from an actual error.
func encodeOKNull() -> String {
    return "{\"ok\":true,\"data\":null}"
}

// encodeErr serializes any Error into the failure envelope, escaping
// quotes and newlines so the result is always valid JSON.
func encodeErr(_ error: Error) -> String {
    let msg = String(describing: error)
        .replacingOccurrences(of: "\\", with: "\\\\")
        .replacingOccurrences(of: "\"", with: "\\\"")
        .replacingOccurrences(of: "\n", with: "\\n")
    return "{\"ok\":false,\"err\":\"\(msg)\"}"
}

// readCString safely converts a possibly-null C string pointer into a
// Swift String, returning nil for null pointers so each export can
// short-circuit with a deterministic error envelope.
func readCString(_ p: UnsafePointer<CChar>?) -> String? {
    guard let p else { return nil }
    return String(cString: p)
}

// dupNullArgErr is a one-liner for the "caller passed null where a
// non-null C string was required" case.
func dupNullArgErr(_ argName: String) -> UnsafePointer<CChar>? {
    UnsafePointer(strdup("{\"ok\":false,\"err\":\"null \(argName)\"}"))
}
