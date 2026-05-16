import ContainerAPIClient
import ContainerResource
import ContainerizationError
import Foundation

// Compose orchestrator primitives — apple/container 0.12 network
// surface. Each export marshals a runtime-neutral request struct
// over XPC via NetworkClient and returns the canonical JSON
// envelope.
//
// Apple's network model uses NetworkConfiguration with .nat mode
// + the vmnet plugin as the default. Compose orchestrator-created
// project networks always use the .nat mode (the host-only mode
// is not what compose semantics expect). Subnet auto-allocation
// is delegated to the apiserver by passing nil for ipv4Subnet.

// Apiserver round-trips are local + fast; 10s is generous.
private let networkTimeoutSeconds = 10

// NetworkSpecJSON mirrors the Go-side runtime.NetworkSpec wire shape.
// driver / options are accepted for parity with the runtime
// interface but ignored on this backend (apple's network plugin
// selection is not user-facing in 0.12.x).
private struct NetworkSpecJSON: Decodable {
    var name: String
    var labels: [String: String]?
    var driver: String?
    var options: [String: String]?
}

// NetworkResultData reports the apiserver-assigned id back to Go.
// On apple the network id IS the name (NetworkConfiguration.id is
// the unique identifier), so we return it both as id and the same
// name the caller passed in.
private struct NetworkResultData: Encodable {
    let id: String
}

@_cdecl("ac_network_create")
public func ac_network_create(_ specPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let specStr = readCString(specPtr) else { return dupNullArgErr("spec") }
    return runSync(timeoutSeconds: networkTimeoutSeconds) {
        do {
            guard let data = specStr.data(using: .utf8) else {
                return "{\"ok\":false,\"err\":\"spec not utf8\"}"
            }
            let spec = try JSONDecoder().decode(NetworkSpecJSON.self, from: data)
            guard !spec.name.isEmpty else {
                return "{\"ok\":false,\"err\":\"NetworkSpec.Name is required\"}"
            }

            let client = NetworkClient()

            // Idempotency on (name, label superset): if a network with
            // this id already exists and its labels are a superset of
            // ours, reuse it. Matches docker.CreateNetwork's behavior.
            if let existing = try? await client.get(id: spec.name) {
                if labelsSuperset(networkLabels(existing), want: spec.labels ?? [:]) {
                    return encodeOK(NetworkResultData(id: existing.id))
                }
            }

            let labels = try ResourceLabels(spec.labels ?? [:])
            let config = try NetworkConfiguration(
                id: spec.name,
                mode: .nat,
                ipv4Subnet: nil,
                ipv6Subnet: nil,
                labels: labels,
                pluginInfo: NetworkPluginInfo(plugin: "container-network-vmnet", variant: nil)
            )
            let state = try await client.create(configuration: config)
            return encodeOK(NetworkResultData(id: state.id))
        } catch {
            return encodeErr(error)
        }
    }
}

@_cdecl("ac_network_remove")
public func ac_network_remove(_ idPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let id = readCString(idPtr) else { return dupNullArgErr("id") }
    return runSync(timeoutSeconds: networkTimeoutSeconds) {
        do {
            // delete throws notFound when the network is missing —
            // swallow that case so RemoveNetwork is idempotent at the
            // Go interface boundary.
            try await NetworkClient().delete(id: id)
            return "{\"ok\":true}"
        } catch let e as ContainerizationError where e.code == .notFound {
            return "{\"ok\":true}"
        } catch {
            return encodeErr(error)
        }
    }
}

// networkLabels extracts the resource-labels dictionary from a
// NetworkState. The Swift enum carries the configuration as an
// associated value; pattern-match to reach the labels.
private func networkLabels(_ state: NetworkState) -> [String: String] {
    switch state {
    case .created(let cfg), .running(let cfg, _):
        return cfg.labels.dictionary
    }
}

// labelsSuperset is the apple-bridge analogue of runtime/docker's
// labelsMatch: every (k,v) in want must appear in have. Used by
// the network create idempotency check.
private func labelsSuperset(_ have: [String: String], want: [String: String]) -> Bool {
    for (k, v) in want {
        if have[k] != v { return false }
    }
    return true
}
