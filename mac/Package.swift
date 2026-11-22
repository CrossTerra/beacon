// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "Beacon",
    platforms: [.macOS(.v14)],
    targets: [
        .target(name: "TetherSupport"),
        .executableTarget(name: "tether", dependencies: ["TetherSupport"]),
        .executableTarget(name: "Beacon", dependencies: ["TetherSupport"]),
        .testTarget(name: "BeaconTests", dependencies: ["Beacon"]),
    ]
)
