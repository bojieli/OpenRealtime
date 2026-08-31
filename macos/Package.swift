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
        // The WebRTC transport needs a real WebRTC stack: ICE, DTLS-SRTP,
        // Opus, and the audio device module. This is Google's WebRTC as
        // LiveKit distributes it, a binary target pinned by SHA-256 rather
        // than by this repository's source digest.
        .package(
            url: "https://github.com/livekit/webrtc-xcframework.git",
            exact: "144.7559.14"
        ),
    ],
    targets: [
        .executableTarget(
            name: "OpenRealtimeMac",
            dependencies: [
                .product(name: "OpenRealtimeClientCore", package: "OpenRealtimeClientCore"),
                .product(name: "LiveKitWebRTC", package: "webrtc-xcframework"),
            ],
            path: "Sources/OpenRealtimeMac",
            resources: [.copy("Resources")]
        ),
    ]
)
