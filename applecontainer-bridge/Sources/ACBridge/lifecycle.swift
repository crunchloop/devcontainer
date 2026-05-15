import ContainerAPIClient
import ContainerResource
import Containerization
import ContainerizationOCI
import Foundation

// Run-spec JSON wire shape — Go side marshals runtime.RunSpec into
// this. Apple-side fields we don't model yet (publishedPorts,
// resources, dns) get defaulted by ContainerConfiguration's
// initializers. Engine-level concepts we intentionally drop on this
// backend (RunArgs, Privileged, SecurityOpt) are documented in
// design/runtime-applecontainer.md §8.
private struct RunSpecJSON: Decodable {
    var image: String
    var id: String
    var cmd: [String]?
    var entrypoint: [String]?
    var user: String?
    var workingDir: String?
    var env: [String]?
    var labels: [String: String]?
    var mounts: [MountJSON]?
    var initProcess: Bool?
    var capAdd: [String]?
    var overrideCommand: Bool?
}

private struct MountJSON: Decodable {
    // type ∈ {"bind", "tmpfs", "volume"}; anything else returns an error.
    var type: String
    var source: String?
    var target: String
    var readOnly: Bool?
    // ignored fields on this backend (Propagation, etc.) are not modeled.
}

// Run-result wire shape — the Go side decodes this into runtime.Container.
private struct RunResult: Encodable {
    var id: String
}

// Default for sync lifecycle calls. Container creation can take a few
// seconds for kernel/init-image fetches; 60s is generous enough for
// cold first runs without leaving the cgo caller blocked indefinitely.
private let lifecycleTimeoutSeconds = 60

// ===== ac_run ========================================================

@_cdecl("ac_run")
public func ac_run(_ specPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let specStr = readCString(specPtr) else { return dupNullArgErr("spec") }
    return runSync(timeoutSeconds: lifecycleTimeoutSeconds) {
        do {
            guard let specData = specStr.data(using: .utf8) else {
                return "{\"ok\":false,\"err\":\"spec not utf8\"}"
            }
            let spec = try JSONDecoder().decode(RunSpecJSON.self, from: specData)
            try await runContainer(spec: spec)
            return encodeOK(RunResult(id: spec.id))
        } catch {
            return encodeErr(error)
        }
    }
}

private func runContainer(spec: RunSpecJSON) async throws {
    guard !spec.id.isEmpty else {
        throw BridgeError.invalidArgument("RunSpec.id is required")
    }
    guard !spec.image.isEmpty else {
        throw BridgeError.invalidArgument("RunSpec.image is required")
    }

    // The image must already be in the local content store. Pull is
    // PR-F's job; here we assume the caller has done it.
    let img = try await ClientImage.get(reference: spec.image)
    let imageConfig = try await img.config(for: .current).config

    let process = try buildProcessConfiguration(spec: spec, imageConfig: imageConfig)

    var cfg = ContainerConfiguration(id: spec.id, image: img.description, process: process)
    cfg.platform = .current
    cfg.labels = spec.labels ?? [:]
    cfg.mounts = try (spec.mounts ?? []).map(toFilesystem)
    cfg.capAdd = spec.capAdd ?? []
    cfg.useInit = spec.initProcess ?? false

    let kernel = try await ClientKernel.getDefaultKernel(for: .current)
    let options = ContainerCreateOptions(autoRemove: false)
    try await ContainerClient().create(
        configuration: cfg,
        options: options,
        kernel: kernel
    )
}

private func buildProcessConfiguration(
    spec: RunSpecJSON,
    imageConfig: ImageConfig?
) throws -> ProcessConfiguration {
    let executable: String
    let arguments: [String]

    if spec.overrideCommand ?? false {
        // Engine sets OverrideCommand=true to make the container
        // long-lived for exec. Matches the docker runtime's choice
        // (runtime/runtime.go:228-231).
        executable = "/bin/sh"
        arguments = ["-c", "while sleep 1000; do :; done"]
    } else {
        // Merge image defaults with spec-provided cmd/entrypoint.
        // Entrypoint replaces image's Entrypoint; Cmd replaces image's
        // Cmd. If both are missing on spec side, fall back to image
        // config. This matches OCI conventions and docker semantics.
        let entry = spec.entrypoint ?? imageConfig?.entrypoint ?? []
        let cmd = spec.cmd ?? imageConfig?.cmd ?? []
        let combined = entry + cmd
        guard let first = combined.first else {
            throw BridgeError.invalidArgument("no executable: spec.cmd/entrypoint empty and image has none")
        }
        executable = first
        arguments = Array(combined.dropFirst())
    }

    let env: [String]
    if let e = spec.env, !e.isEmpty {
        env = e
    } else {
        env = imageConfig?.env ?? []
    }

    let user = parseUser(spec.user) ?? imageConfigUser(imageConfig)

    return ProcessConfiguration(
        executable: executable,
        arguments: arguments,
        environment: env,
        workingDirectory: spec.workingDir ?? imageConfig?.workingDir ?? "/",
        terminal: false,
        user: user,
        supplementalGroups: [],
        rlimits: []
    )
}

// parseUser turns the textual user spec from RunSpec.User into the
// Codable form Apple wants. "<uid>:<gid>" → .id; anything else
// (e.g. "vscode" or "vscode:dev") → .raw, which the apiserver
// resolves inside the container.
private func parseUser(_ s: String?) -> ProcessConfiguration.User? {
    guard let s, !s.isEmpty else { return nil }
    let parts = s.split(separator: ":")
    if parts.count == 2,
       let uid = UInt32(parts[0]),
       let gid = UInt32(parts[1])
    {
        return .id(uid: uid, gid: gid)
    }
    return .raw(userString: s)
}

private func imageConfigUser(_ cfg: ImageConfig?) -> ProcessConfiguration.User {
    if let s = cfg?.user, !s.isEmpty {
        return parseUser(s) ?? .raw(userString: s)
    }
    return .id(uid: 0, gid: 0)
}

private func toFilesystem(_ m: MountJSON) throws -> Filesystem {
    var options: MountOptions = []
    if m.readOnly ?? false {
        options.append("ro")
    }
    switch m.type {
    case "bind":
        guard let src = m.source, !src.isEmpty else {
            throw BridgeError.invalidArgument("bind mount requires source")
        }
        return .virtiofs(source: src, destination: m.target, options: options)
    case "tmpfs":
        return .tmpfs(destination: m.target, options: options)
    case "volume":
        // PR-C scope decision: treat named volumes as virtiofs binds
        // against the supplied source. Real named-volume support (with
        // the apiserver pre-creating the volume) is a later PR.
        guard let src = m.source, !src.isEmpty else {
            throw BridgeError.invalidArgument("volume mount on this backend currently requires source (named-volume support is deferred)")
        }
        return .virtiofs(source: src, destination: m.target, options: options)
    default:
        throw BridgeError.invalidArgument("unknown mount type \"\(m.type)\"")
    }
}

private enum BridgeError: LocalizedError {
    case invalidArgument(String)

    var errorDescription: String? {
        switch self {
        case .invalidArgument(let m): return "invalid argument: \(m)"
        }
    }
}

// ===== ac_start ======================================================

@_cdecl("ac_start")
public func ac_start(_ idPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let id = readCString(idPtr) else { return dupNullArgErr("id") }
    return runSync(timeoutSeconds: lifecycleTimeoutSeconds) {
        do {
            let client = ContainerClient()
            let snap = try await client.get(id: id)
            // Idempotency: if the container is already running, return
            // success. Matches the CLI's behavior in
            // ContainerStart.swift L60-72 and Docker's "start" on a
            // running container (no-op).
            if snap.status == .running {
                return "{\"ok\":true}"
            }
            // Detached start: no stdio attachment. ProcessIO with
            // detach=true returns [nil,nil,nil] for stdio; we replicate
            // that directly without instantiating ProcessIO.
            let process = try await client.bootstrap(
                id: id,
                stdio: [nil, nil, nil],
                dynamicEnv: [:]
            )
            try await process.start()
            return "{\"ok\":true}"
        } catch {
            return encodeErr(error)
        }
    }
}

// ===== ac_stop =======================================================

@_cdecl("ac_stop")
public func ac_stop(_ idPtr: UnsafePointer<CChar>?, _ timeoutSeconds: Int32) -> UnsafePointer<CChar>? {
    guard let id = readCString(idPtr) else { return dupNullArgErr("id") }
    return runSync(timeoutSeconds: lifecycleTimeoutSeconds) {
        do {
            // Apple's ContainerStopOptions has its own grace-period
            // knob. timeoutSeconds <= 0 uses the API default (5s
            // SIGTERM, then SIGKILL).
            let opts: ContainerStopOptions = timeoutSeconds > 0
                ? .init(timeoutInSeconds: timeoutSeconds, signal: SIGTERM)
                : .default
            try await ContainerClient().stop(id: id, opts: opts)
            return "{\"ok\":true}"
        } catch {
            return encodeErr(error)
        }
    }
}

// ===== ac_delete =====================================================

@_cdecl("ac_delete")
public func ac_delete(_ idPtr: UnsafePointer<CChar>?, _ force: Int32) -> UnsafePointer<CChar>? {
    guard let id = readCString(idPtr) else { return dupNullArgErr("id") }
    return runSync(timeoutSeconds: lifecycleTimeoutSeconds) {
        do {
            try await ContainerClient().delete(id: id, force: force != 0)
            return "{\"ok\":true}"
        } catch {
            return encodeErr(error)
        }
    }
}
