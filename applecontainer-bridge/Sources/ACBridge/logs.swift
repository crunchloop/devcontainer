import ContainerAPIClient
import Darwin
import Foundation

// Logs design notes:
// Apple's client.logs(id:) returns an array of FileHandles — index 0
// is the container's stdio log, index 1 is the boot log. We always
// return stdio. Boot logs aren't part of the runtime.Runtime contract.
//
// The returned fd is dup(2)'d from Apple's FileHandle so that
// FileHandle deinit can close its end without affecting the Go-side
// reader. Go owns the dup'd fd from this point.
//
// Follow vs non-follow is implemented Go-side: the log is a regular
// file on disk; non-follow reads to EOF, follow polls after EOF until
// ctx cancellation closes the fd. Pushing the polling logic to Go
// keeps ctx cancellation simple (close fd → Read returns) and avoids
// another handle table.

private struct LogsOpenResult: Encodable {
    let fd: Int32
}

private let logsTimeoutSeconds = 10

@_cdecl("ac_logs_open")
public func ac_logs_open(_ idPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let id = readCString(idPtr) else { return dupNullArgErr("id") }
    return runSync(timeoutSeconds: logsTimeoutSeconds) {
        do {
            let handles = try await ContainerClient().logs(id: id)
            guard let stdioHandle = handles.first else {
                return "{\"ok\":false,\"err\":\"apiserver returned no log handles\"}"
            }
            // dup so Apple's FileHandle deinit doesn't kill the fd
            // out from under the Go reader.
            let dupFd = Darwin.dup(stdioHandle.fileDescriptor)
            if dupFd < 0 {
                let err = String(cString: strerror(errno))
                return "{\"ok\":false,\"err\":\"dup logs fd: \(err)\"}"
            }
            return encodeOK(LogsOpenResult(fd: dupFd))
        } catch {
            return encodeErr(error)
        }
    }
}
