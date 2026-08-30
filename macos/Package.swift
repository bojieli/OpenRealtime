// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "OpenRealtimeMac",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "OpenRealtimeMac", targets: ["OpenRealtimeMac"]),
    ],
    dependencies: [
        .package(name: "OpenRealtimeClientCore", path: "../client/reducer/swift"),
    ],
    targets: [
        .executableTarget(
            name: "OpenRealtimeMac",
            dependencies: [
                .product(name: "OpenRealtimeClientCore", package: "OpenRealtimeClientCore"),
            ],
            path: "Sources/OpenRealtimeMac",
            exclude: ["DesktopComputer.swift", "ToolHost.swift"],
            resources: [.copy("Resources")]
        ),
    ]
)
