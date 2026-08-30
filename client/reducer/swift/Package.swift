// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "OpenRealtimeClientCore",
    products: [
        .library(name: "OpenRealtimeClientCore", targets: ["OpenRealtimeClientCore"]),
    ],
    targets: [
        .target(
            name: "OpenRealtimeClientCore",
            path: ".",
            exclude: ["Package.swift", "main.swift", "Tests"],
            sources: ["StrictJSON.swift", "ClientReducer.swift", "NativeClientCore.swift"]
        ),
        .testTarget(
            name: "OpenRealtimeClientCoreTests",
            dependencies: ["OpenRealtimeClientCore"],
            path: "Tests"
        ),
    ]
)
