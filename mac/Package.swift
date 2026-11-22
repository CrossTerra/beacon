// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "Beacon",
    platforms: [.macOS(.v14)],
    targets: [
        .target(name: "TetherSupport"),
        .target(
            name: "BeaconSecurity",
            cSettings: [.unsafeFlags(["-Wno-deprecated-declarations"])],
            linkerSettings: [.linkedFramework("Security"), .linkedFramework("CoreFoundation"), .linkedLibrary("bsm")]
        ),
        .target(name: "BeaconCore", dependencies: ["BeaconSecurity", "TetherSupport"], linkerSettings: [.linkedLibrary("sqlite3")]),
        .executableTarget(name: "tether", dependencies: ["TetherSupport"]),
        .executableTarget(name: "Beacon", dependencies: ["TetherSupport", "BeaconCore"]),
        .testTarget(name: "BeaconTests", dependencies: ["Beacon"]),
        .testTarget(name: "BeaconCoreTests", dependencies: ["BeaconCore", "TetherSupport"], resources: [.copy("Fixtures")]),
    ]
)
