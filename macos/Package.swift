// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "OpenRealtimeMac",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "OpenRealtimeMac", targets: ["OpenRealtimeMac"]),
    ],
    targets: [
        .executableTarget(
            name: "OpenRealtimeMac",
            path: "Sources/OpenRealtimeMac",
            resources: [.copy("Resources")]
        ),
    ]
)
