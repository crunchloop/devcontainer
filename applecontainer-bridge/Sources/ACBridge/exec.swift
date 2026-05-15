import ContainerAPIClient
import ContainerResource
import Containerization
import Darwin
import Foundation

// Handle table for in-flight exec processes. Each handle owns:
//   - the ClientProcess for signaling/waiting
//   - the FileHandles wrapping the caller-supplied stdio fds, kept
//     alive so they aren't released while the apiserver-side dup is
//     still active.
//
// Thread safety: NSLock around a dictionary. The synchronous @_cdecl
// entry points all touch the table once, so contention is negligible.
private final class ExecRegistry: @unchecked Sendable {
    struct Entry {
        let process: any ClientProcess
        let stdio: [FileHandle?]
    }

    private let lock = NSLock()
    private var entries: [UInt64: Entry] = [:]
    private var next: UInt64 = 1

    func register(_ entry: Entry) -> UInt64 {
        lock.lock()
        defer { lock.unlock() }
        let h = next
        next &+= 1
        if next == 0 { next = 1 } // skip the sentinel value
        entries[h] = entry
        return h
    }

    func get(_ h: UInt64) -> Entry? {
        lock.lock()
        defer { lock.unlock() }
        return entries[h]
    }

    func remove(_ h: UInt64) {
        lock.lock()
        defer { lock.unlock() }
        entries.removeValue(forKey: h)
    }
}

private let execRegistry = ExecRegistry()

// ExecOptsJSON is the wire shape from runtime.ExecOptions.
//
// stdio fds are passed as separate int arguments rather than nested
// in this JSON so the C ABI for ac_exec_start stays straightforward.
// A fd value of -1 means "no pipe" (e.g. nil stdin, or stderr in TTY
// mode where Apple merges into stdout).
private struct ExecOptsJSON: Decodable {
    var cmd: [String]
    var env: [String]?
    var user: String?
    var workingDir: String?
    var tty: Bool?
}

private struct ExecStartResult: Encodable {
    let handle: UInt64
}

private struct ExecWaitResult: Encodable {
    let exitCode: Int32
}

// Sync timeouts. exec_start should be fast (XPC round-trip).
// exec_wait blocks for the process duration, which is unbounded by
// design — the caller chooses the timeout per call.
private let execStartTimeoutSeconds = 30

// ===== ac_exec_start =================================================

@_cdecl("ac_exec_start")
public func ac_exec_start(
    _ idPtr: UnsafePointer<CChar>?,
    _ optsPtr: UnsafePointer<CChar>?,
    _ stdinReadFd: Int32,
    _ stdoutWriteFd: Int32,
    _ stderrWriteFd: Int32
) -> UnsafePointer<CChar>? {
    guard let containerId = readCString(idPtr) else { return dupNullArgErr("id") }
    guard let optsStr = readCString(optsPtr) else { return dupNullArgErr("opts") }

    return runSync(timeoutSeconds: execStartTimeoutSeconds) {
        do {
            guard let optsData = optsStr.data(using: .utf8) else {
                return "{\"ok\":false,\"err\":\"opts not utf8\"}"
            }
            let opts = try JSONDecoder().decode(ExecOptsJSON.self, from: optsData)
            guard !opts.cmd.isEmpty else {
                return "{\"ok\":false,\"err\":\"empty cmd\"}"
            }

            let client = ContainerClient()
            let snap = try await client.get(id: containerId)

            // Start from the init process config (inherits PATH, etc.)
            // and override what the caller specified, matching the
            // CLI's exec command (ContainerExec.swift).
            var cfg = snap.configuration.initProcess
            cfg.executable = opts.cmd[0]
            cfg.arguments = Array(opts.cmd.dropFirst())
            cfg.terminal = opts.tty ?? false
            if let e = opts.env, !e.isEmpty {
                cfg.environment = e
            }
            if let cwd = opts.workingDir, !cwd.isEmpty {
                cfg.workingDirectory = cwd
            }
            if let user = opts.user, !user.isEmpty {
                if let parsed = parseUserForExec(user) {
                    cfg.user = parsed
                }
            }

            // Wrap caller-supplied fds in FileHandles. closeOnDealloc:
            // false keeps fd lifetime in the caller's hands; XPC
            // dup's the fd when serializing so the apiserver gets its
            // own. After createProcess returns, the caller is free to
            // close its end.
            let stdio: [FileHandle?] = [
                fileHandleOrNil(stdinReadFd),
                fileHandleOrNil(stdoutWriteFd),
                fileHandleOrNil(stderrWriteFd),
            ]

            let processId = "exec-" + UUID().uuidString.prefix(8)
            let process = try await client.createProcess(
                containerId: containerId,
                processId: String(processId),
                configuration: cfg,
                stdio: stdio
            )
            try await process.start()

            let handle = execRegistry.register(.init(process: process, stdio: stdio))
            return encodeOK(ExecStartResult(handle: handle))
        } catch {
            return encodeErr(error)
        }
    }
}

// ===== ac_exec_wait ==================================================

@_cdecl("ac_exec_wait")
public func ac_exec_wait(_ handle: UInt64, _ timeoutSeconds: Int32) -> UnsafePointer<CChar>? {
    guard let entry = execRegistry.get(handle) else {
        return UnsafePointer(strdup("{\"ok\":false,\"err\":\"unknown exec handle\"}"))
    }
    // timeoutSeconds <= 0 → effectively unbounded (Int32.max). The
    // bridge's runSync still bounds the underlying wait via its hard
    // cap, but for exec we want it large so legitimate long-running
    // processes don't trip a bridge-side timeout.
    let timeout = timeoutSeconds > 0 ? Int(timeoutSeconds) : Int(Int32.max / 1000)
    return runSync(timeoutSeconds: timeout) {
        do {
            let code = try await entry.process.wait()
            return encodeOK(ExecWaitResult(exitCode: code))
        } catch {
            return encodeErr(error)
        }
    }
}

// ===== ac_exec_signal ================================================

@_cdecl("ac_exec_signal")
public func ac_exec_signal(_ handle: UInt64, _ signal: Int32) -> UnsafePointer<CChar>? {
    guard let entry = execRegistry.get(handle) else {
        return UnsafePointer(strdup("{\"ok\":false,\"err\":\"unknown exec handle\"}"))
    }
    return runSync(timeoutSeconds: 5) {
        do {
            try await entry.process.kill(signal)
            return "{\"ok\":true}"
        } catch {
            return encodeErr(error)
        }
    }
}

// ===== ac_exec_release ===============================================

@_cdecl("ac_exec_release")
public func ac_exec_release(_ handle: UInt64) {
    execRegistry.remove(handle)
}

// ---- helpers --------------------------------------------------------

private func fileHandleOrNil(_ fd: Int32) -> FileHandle? {
    if fd < 0 { return nil }
    return FileHandle(fileDescriptor: fd, closeOnDealloc: false)
}

private func parseUserForExec(_ s: String) -> ProcessConfiguration.User? {
    let parts = s.split(separator: ":")
    if parts.count == 2,
       let uid = UInt32(parts[0]),
       let gid = UInt32(parts[1])
    {
        return .id(uid: uid, gid: gid)
    }
    return .raw(userString: s)
}
