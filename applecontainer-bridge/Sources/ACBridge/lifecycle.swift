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
    // Network IDs the container should be attached to. Empty / nil
    // means "no explicit attachment" — apple's apiserver auto-joins
    // the built-in default network when the field is unset. The
    // compose orchestrator passes <project>_default here so its
    // services land on the project network it created via
    // NetworkClient.create.
    var networks: [String]?
    var initProcess: Bool?
    var capAdd: [String]?
    var overrideCommand: Bool?
    // Hard memory limit for the per-container VM, in bytes. Zero or
    // absent leaves apple's default (1 GiB on 0.12.x) in place.
    var memoryBytes: Int64?
    // CPU limit in nano-units (1_000_000_000 = 1 CPU). Apple's
    // apiserver takes an integer CPU count, so the bridge rounds up
    // to the next whole CPU. Zero or absent leaves apple's default (4)
    // in place.
    var nanoCPUs: Int64?
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
    // Resolve the platform on the cached image first, then re-fetch
    // for that platform so the daemon has the correct snapshot
    // staged. apple/container's containerConfigFromFlags uses
    // ClientImage.fetch (not get) for exactly this reason: get
    // returns the index entry but doesn't ensure a per-platform
    // snapshot is present, and ContainerClient().create rejects
    // missing snapshots as "does not support required platforms".
    // Resolve the image's platform. For multi-arch images, prefer
    // the host's `.current`; for single-arch images (commonly
    // amd64-only when the publisher only builds on x86 CI), fall
    // back to whatever the image actually carries. Then stage the
    // per-platform snapshot the apiserver requires before create —
    // mirrors apple/container CLI's containerConfigFromFlags path.
    let img = try await ClientImage.get(reference: spec.image)
    let platform = try await resolvePlatform(for: img)
    try await img.getCreateSnapshot(platform: platform, progressUpdate: nil)
    let imageConfig = try await img.config(for: platform).config

    let process = try buildProcessConfiguration(spec: spec, imageConfig: imageConfig)

    var cfg = ContainerConfiguration(id: spec.id, image: img.description, process: process)
    cfg.platform = platform
    cfg.labels = spec.labels ?? [:]
    cfg.mounts = try (spec.mounts ?? []).map(toFilesystem)
    cfg.capAdd = spec.capAdd ?? []
    cfg.useInit = spec.initProcess ?? false
    // Resource limits. Apply only when caller specified a value;
    // leave apple's Resources defaults (4 cpus / 1 GiB) untouched
    // otherwise. Negative inputs are clamped out at the bridge
    // boundary; the Go side rejects them earlier too.
    if let mem = spec.memoryBytes, mem > 0 {
        cfg.resources.memoryInBytes = UInt64(mem)
    }
    if let nano = spec.nanoCPUs, nano > 0 {
        // Round up to the next whole CPU. NanoCPUs of 1_500_000_000
        // (1.5 cpus) → cpus = 2. Apple's apiserver doesn't model
        // fractional CPU shares; callers expressing a fractional
        // limit get the next whole CPU rather than a silent floor.
        let cpus = Int((nano + 999_999_999) / 1_000_000_000)
        if cpus > 0 {
            cfg.resources.cpus = cpus
        }
    }
    // Enable Rosetta when running an amd64 container on an arm64
    // host. Without this flag the apiserver rejects amd64 containers
    // with "unsupported: platform linux/amd64". Mirrors
    // apple/container CLI's containerConfigFromFlags auto-enabling
    // of rosetta for the same case. Subject to host's Rosetta-for-
    // Linux being installed and Virtualization.framework allowing
    // its use — neither is universally available, and an
    // unsupported host surfaces as VZErrorDomain Code=1 at bootstrap.
    let host = ContainerizationOCI.Platform.current
    if host.architecture == "arm64" && platform.architecture == "amd64" {
        cfg.rosetta = true
    }
    // Attach explicitly to any networks the caller requested. The
    // hostname per attachment defaults to the container id, matching
    // apple/container CLI's behavior. Empty Networks => no override:
    // the apiserver attaches to the built-in default automatically.
    if let nets = spec.networks, !nets.isEmpty {
        cfg.networks = nets.map {
            AttachmentConfiguration(network: $0, options: AttachmentOptions(hostname: spec.id))
        }
    }

    // Kernel selection: always use the host platform. For amd64
    // containers on arm64 hosts, the VM still runs an arm64 kernel
    // and Apple's Rosetta translates amd64 userland binaries
    // (cfg.rosetta=true, set below). Mirrors apple/container's CLI:
    // the kernel is host-arch; the container's platform only
    // influences Rosetta enablement and image manifest selection.
    let hostSysPlatform: SystemPlatform = .linuxArm
    let kernel = try await ClientKernel.getDefaultKernel(for: hostSysPlatform)
    // Stage the init image for the host platform (.current).
    // The init binary runs in the VM's pid 1 slot — apple's
    // apiserver wires up a translation when the container's
    // platform differs (Rosetta on Apple silicon). Mirrors the
    // CLI's containerConfigFromFlags: it always fetches init for
    // .current regardless of the container's platform.
    let initImageRef = ClientImage.initImageRef
    let initImg = try await ClientImage.fetch(
        reference: initImageRef,
        platform: .current,
        scheme: .auto,
        progressUpdate: nil
    )
    try await initImg.getCreateSnapshot(platform: .current, progressUpdate: nil)

    let options = ContainerCreateOptions(autoRemove: false)
    try await ContainerClient().create(
        configuration: cfg,
        options: options,
        kernel: kernel,
        initImage: initImageRef
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

// resolvePlatform picks a platform descriptor the image actually
// supports. Default preference is the host's `.current`; if the
// image's index doesn't carry that variant (common case: amd64-only
// images on Apple silicon), fall back to the first variant the
// image's index declares. Falls back to .current if the image
// store can't surface an index (legacy single-manifest images).
private func resolvePlatform(for img: ClientImage) async throws -> ContainerizationOCI.Platform {
    let current = ContainerizationOCI.Platform.current
    do {
        let index = try await img.index()
        for desc in index.manifests {
            if let p = desc.platform, p.architecture == current.architecture && p.os == current.os {
                return current
            }
        }
        for desc in index.manifests {
            if let p = desc.platform {
                return p
            }
        }
    } catch {
        // Single-manifest image or other index lookup error;
        // .current is the right default to try.
    }
    return current
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
