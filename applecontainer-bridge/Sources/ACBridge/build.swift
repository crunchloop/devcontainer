import ContainerAPIClient
import ContainerBuild
import ContainerImagesServiceClient
import ContainerizationError
import ContainerizationOCI
import Foundation
import Logging
import NIOCore
import NIOPosix

// PR-G2: full BuildKit gRPC integration. Dials Apple's buildkit
// container over vsock, constructs a Builder, runs the build to an
// OCI tarball export, then loads + unpacks + tags it via the images
// service.
//
// Scope cuts intentionally not addressed in PR-G2:
//   - Auto-start of the buildkit container. BuilderStart.start is
//     module-internal to ContainerCommands, so we'd be reimplementing
//     the bring-up logic ourselves. We surface a clean
//     BuilderUnavailableError instead and rely on the user running
//     `container builder start` once per machine.
//   - Progress event streaming. BuildConfig accepts a Terminal? for
//     output; passing nil routes progress to FileHandle.standardError,
//     which the Go process inherits. A future PR can substitute a
//     pipe + line-parsing for typed BuildEvent emission.
//   - Multi-platform builds. We accept a single spec.platform string;
//     anything else is dropped.
//
// Builder vsock port (matches the CLI's BuildCommand.swift default).
private let buildkitVsockPort: UInt32 = 8088
private let buildkitContainerID = "buildkit"

private let buildTimeoutSeconds = 30 * 60 // 30 min for a cold cache

private struct BuildSpecJSON: Decodable {
    var contextPath: String
    // Dockerfile is the in-context relative path, NOT absolute. The
    // engine resolves it relative to contextPath before sending.
    var dockerfile: String?
    var tag: String?
    var args: [String: String]?
    var target: String?
    var cacheFrom: [String]?
    var noCache: Bool?
    var platform: String?
}

private struct BuildResult: Encodable {
    let reference: String
    let digest: String
}

// ac_build_probe is preserved from PR-G — used by the Go side to
// short-circuit with a typed BuilderUnavailableError before paying
// the cost of marshaling a BuildSpec.
@_cdecl("ac_build_probe")
public func ac_build_probe() -> UnsafePointer<CChar>? {
    return runSync(timeoutSeconds: 5) {
        do {
            let snap = try await ContainerClient().get(id: buildkitContainerID)
            if snap.status == .running {
                return "{\"ok\":true}"
            }
            return "{\"ok\":false,\"err\":\"builder container exists but status is \(snap.status)\"}"
        } catch {
            return encodeErr(error)
        }
    }
}

@_cdecl("ac_build")
public func ac_build(_ specPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let specStr = readCString(specPtr) else { return dupNullArgErr("spec") }
    return runSync(timeoutSeconds: buildTimeoutSeconds) {
        do {
            guard let data = specStr.data(using: .utf8) else {
                return "{\"ok\":false,\"err\":\"spec not utf8\"}"
            }
            let spec = try JSONDecoder().decode(BuildSpecJSON.self, from: data)
            let result = try await runBuild(spec: spec)
            return encodeOK(result)
        } catch {
            return encodeErr(error)
        }
    }
}

// runBuild is the actual build flow. Split out so the @_cdecl wrapper
// stays small and the resource cleanup (event loop, file handle,
// temp directory) is structured as a single function with defers.
private func runBuild(spec: BuildSpecJSON) async throws -> BuildResult {
    let client = ContainerClient()

    // Resolve dockerfile contents. Defaults to "Dockerfile" in the
    // context if not specified — matches Docker / OCI BuildKit
    // conventions.
    let dockerfileRel = (spec.dockerfile ?? "Dockerfile")
    let dockerfileURL = URL(fileURLWithPath: spec.contextPath).appendingPathComponent(dockerfileRel)
    let dockerfileData = try Data(contentsOf: dockerfileURL)

    // Dockerignore is optional; absent file is fine.
    let dockerignoreURL = URL(fileURLWithPath: spec.contextPath).appendingPathComponent(".dockerignore")
    let dockerignoreData = try? Data(contentsOf: dockerignoreURL)

    // Dial buildkit. If the container isn't running, surface a clean
    // typed error path via the message (Go side maps to
    // BuilderUnavailableError).
    let socketHandle: FileHandle
    do {
        socketHandle = try await client.dial(id: buildkitContainerID, port: buildkitVsockPort)
    } catch {
        throw ContainerizationError(
            .notFound,
            message: "builder not running (run `container builder start`): \(error)"
        )
    }

    let eventLoopGroup = MultiThreadedEventLoopGroup(numberOfThreads: System.coreCount)
    // Shutdown is handled explicitly before each return path below
    // (after the build completes). Even with synchronous-on-completion
    // shutdown, the gRPC client's internal graceful-shutdown has
    // straggler work that occasionally produces "Cannot schedule
    // tasks on an EventLoop that has already shut down" warnings.
    // These are upstream SwiftNIO deprecation warnings rather than
    // functional failures; a polish PR can pin a sequencing fix once
    // grpc-swift exposes the right hook.

    var logger = Logger(label: "applecontainer-bridge.build")
    logger.logLevel = .warning

    let builder = try Builder(socket: socketHandle, group: eventLoopGroup, logger: logger)

    // Verify the builder responds. The CLI does this same probe right
    // after construction.
    _ = try await builder.info()

    // Export destination MUST live under the apiserver's appRoot at
    // <appRoot>/builder/<buildID>/, which the apiserver mounts into
    // the buildkit VM as /var/lib/container-builder-shim/exports/<buildID>/.
    // Anywhere else fails with "no such file or directory" inside
    // the VM. Source of truth: Application.BuilderCommand.builderResourceDir
    // in apple/container's ContainerCommands/Builder/Builder.swift.
    let buildID = UUID().uuidString
    let systemHealth = try await ClientHealthCheck.ping(timeout: .seconds(10))
    let exportDir = systemHealth.appRoot
        .appendingPathComponent("builder")
        .appendingPathComponent(buildID)
    try FileManager.default.createDirectory(at: exportDir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: exportDir) }

    let tarURL = exportDir.appendingPathComponent("out.tar")
    let exports: [Builder.BuildExport] = [
        Builder.BuildExport(
            type: "oci",
            destination: tarURL,
            additionalFields: [:],
            rawValue: "type=oci,dest=\(tarURL.path)"
        )
    ]

    // Parse the single-platform string (e.g. "linux/arm64") if given,
    // else default to .current. BuildKit hangs indefinitely if
    // platforms is empty — the CLI guards against this by always
    // resolving a platform from CLI/env/host defaults.
    let platforms = try parsePlatformsWithDefault(spec.platform)

    // Build args: BuildKit expects ["KEY=value", ...] form.
    let buildArgs: [String] = (spec.args ?? [:]).map { "\($0.key)=\($0.value)" }

    let tag = spec.tag ?? ""
    let tags: [String] = tag.isEmpty ? [] : [tag]

    // Matches the CLI's `progress=plain` path: terminal nil, quiet
    // false. `quiet=true` stalls the Solve request before it leaves
    // the client (observed empirically); plain progress + nil
    // terminal makes BuildKit fall back to its internal logging
    // (which goes to the builder VM's stderr — invisible to us).
    let config = Builder.BuildConfig(
        buildID: buildID,
        contentStore: RemoteContentStoreClient(),
        buildArgs: buildArgs,
        secrets: [:],
        contextDir: spec.contextPath,
        dockerfile: dockerfileData,
        dockerignore: dockerignoreData,
        labels: [],
        noCache: spec.noCache ?? false,
        platforms: platforms,
        terminal: nil,
        tags: tags,
        target: spec.target ?? "",
        quiet: false,
        exports: exports,
        cacheIn: spec.cacheFrom ?? [],
        cacheOut: [],
        pull: false
    )

    do {
        try await builder.build(config)
    } catch {
        try? await eventLoopGroup.shutdownGracefully()
        throw error
    }
    // Shut down the gRPC event loop now that the build session is
    // complete. Synchronous-on-completion (not deferred) so the
    // gRPC client's graceful shutdown sees the loop alive long
    // enough to finish without scheduling errors.
    try? await eventLoopGroup.shutdownGracefully()

    // Build done — load the OCI tarball into the local content store,
    // unpack, and tag. Mirrors the CLI's post-build "Unpacking built
    // image" pass.
    let loadResult = try await ClientImage.load(from: tarURL.path, force: false)
    if !loadResult.rejectedMembers.isEmpty {
        throw ContainerizationError(
            .internalError,
            message: "build archive contained rejected members: \(loadResult.rejectedMembers)"
        )
    }
    guard let firstImage = loadResult.images.first else {
        throw ContainerizationError(.internalError, message: "build produced no images")
    }
    try await firstImage.unpack(platform: nil, progressUpdate: nil)
    var tagged: ClientImage = firstImage
    for tagName in tags {
        tagged = try await firstImage.tag(new: tagName)
    }

    return BuildResult(
        reference: tags.first ?? tagged.description.reference,
        digest: tagged.description.digest
    )
}

private func parsePlatformsWithDefault(_ s: String?) throws -> [ContainerizationOCI.Platform] {
    if let s, !s.isEmpty {
        return [try ContainerizationOCI.Platform(from: s)]
    }
    return [.current]
}
