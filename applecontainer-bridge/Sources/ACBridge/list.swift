import ContainerAPIClient
import ContainerizationError
import ContainerizationOCI
import Foundation

// Compose orchestrator primitives — listing containers / images and
// removing images. Apple's surface lacks server-side label
// filtering (probe R1b confirmed `container list` has no --filter
// in 0.12.3), so these exports enumerate full result sets and
// expect the Go layer to filter. The result-set size for a single
// project is small enough that the overhead is negligible.

private let listTimeoutSeconds = 15

// ContainerListItem is the projection per container the Go side
// consumes from ListContainers. Mirrors the runtime.Container shape
// the docker backend returns from its own ContainerList.
private struct ContainerListItem: Encodable {
    let id: String
    let name: String
    let image: String
    let state: String
    let labels: [String: String]
}

private struct ContainerListData: Encodable {
    let containers: [ContainerListItem]
}

@_cdecl("ac_list_containers")
public func ac_list_containers() -> UnsafePointer<CChar>? {
    return runSync(timeoutSeconds: listTimeoutSeconds) {
        do {
            // ContainerListFilters.all enumerates every container
            // regardless of state. The Go side applies label-based
            // filtering after this returns.
            let snaps = try await ContainerClient().list(filters: .all)
            let items = snaps.map { snap -> ContainerListItem in
                let cfg = snap.configuration
                return ContainerListItem(
                    id: cfg.id,
                    name: cfg.id,
                    image: cfg.image.reference,
                    state: snap.status.rawValue,
                    labels: cfg.labels
                )
            }
            return encodeOK(ContainerListData(containers: items))
        } catch {
            return encodeErr(error)
        }
    }
}

// ImageListItem mirrors runtime.ImageRef. Tags slice carries the
// image's user-facing reference; ID is the manifest digest.
private struct ImageListItem: Encodable {
    let id: String
    let tags: [String]
}

private struct ImageListData: Encodable {
    let images: [ImageListItem]
}

@_cdecl("ac_list_images")
public func ac_list_images() -> UnsafePointer<CChar>? {
    return runSync(timeoutSeconds: listTimeoutSeconds) {
        do {
            let imgs = try await ClientImage.list()
            let items = imgs.map { img -> ImageListItem in
                ImageListItem(
                    id: img.description.digest,
                    tags: [img.description.reference]
                )
            }
            return encodeOK(ImageListData(images: items))
        } catch {
            return encodeErr(error)
        }
    }
}

@_cdecl("ac_remove_image")
public func ac_remove_image(_ refPtr: UnsafePointer<CChar>?) -> UnsafePointer<CChar>? {
    guard let ref = readCString(refPtr) else { return dupNullArgErr("reference") }
    return runSync(timeoutSeconds: listTimeoutSeconds) {
        do {
            try await ClientImage.delete(reference: ref, garbageCollect: true)
            return "{\"ok\":true}"
        } catch let e as ContainerizationError where e.code == .notFound {
            return "{\"ok\":true}"
        } catch {
            return encodeErr(error)
        }
    }
}
