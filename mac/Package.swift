// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "Beacon",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(name: "Beacon"),
        .testTarget(name: "BeaconTests", dependencies: ["Beacon"]),
    ]
)
