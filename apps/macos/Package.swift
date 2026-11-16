// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "Tabtender",
    platforms: [.macOS(.v13)],
    products: [
        .library(name: "TabtenderKit", targets: ["TabtenderKit"]),
        .executable(name: "Tabtender", targets: ["Tabtender"]),
    ],
    targets: [
        .target(name: "TabtenderKit"),
        .executableTarget(name: "Tabtender", dependencies: ["TabtenderKit"]),
        .testTarget(name: "TabtenderKitTests", dependencies: ["TabtenderKit"]),
    ]
)
