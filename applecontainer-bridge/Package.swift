// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "ACBridge",
    platforms: [.macOS("15.0")],
    products: [
        .library(name: "ACBridge", type: .dynamic, targets: ["ACBridge"]),
    ],
    dependencies: [
        .package(url: "https://github.com/apple/container.git", exact: "0.12.3"),
    ],
    targets: [
        .target(
            name: "ACBridge",
            dependencies: [
                .product(name: "ContainerAPIClient", package: "container"),
                // PR-G2: BuildKit gRPC build flow.
                .product(name: "ContainerBuild", package: "container"),
                // ContainerImagesService exposes RemoteContentStoreClient
                // (BuildKit's ContentStore implementation backed by the
                // local images service). Required by Builder.BuildConfig.
                .product(name: "ContainerImagesService", package: "container"),
            ],
            path: "Sources/ACBridge"
        ),
    ]
)
