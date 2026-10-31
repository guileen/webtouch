// swift-tools-version: 5.8
import PackageDescription

let package = Package(
    name: "UWSViewerPrototype",
    platforms: [.macOS(.v13)],
    products: [
        .library(name: "ViewerCore", targets: ["ViewerCore"]),
        .executable(name: "uws-viewer-prototype", targets: ["ViewerApp"]),
    ],
    targets: [
        .target(name: "ViewerCore"),
        .executableTarget(name: "ViewerApp", dependencies: ["ViewerCore"]),
        .testTarget(name: "ViewerCoreTests", dependencies: ["ViewerCore"]),
    ]
)
