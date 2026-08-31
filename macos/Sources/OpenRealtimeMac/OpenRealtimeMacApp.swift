import SwiftUI
import Foundation
import AppKit
import OpenRealtimeClientCore

@main
struct OpenRealtimeMacApp: App {
    private static let connectOnLaunchArgument = "--openrealtime-connect-on-launch"
    private static let hostedSmokePrefix = "--openrealtime-hosted-smoke="

    private let model: DeveloperModel?
    private let launchFailure: String

    init() {
        do {
            let environment = ProcessInfo.processInfo.environment
            let automation = try NativeLaunchAutomation.parse(
                arguments: CommandLine.arguments,
                connectArgument: Self.connectOnLaunchArgument,
                hostedSmokePrefix: Self.hostedSmokePrefix
            )
            let distribution = try NativeClientDistribution.parse(
                environment["OPENREALTIME_NATIVE_PROFILE"]
            )
            let endpoints = try nativeEndpointDirectoryData(
                environment["OPENREALTIME_NATIVE_ENDPOINT_DIRECTORY"]
            )
            let developer = try DeveloperModel(
                distribution: distribution, endpointDirectoryData: endpoints
            )
            model = developer
            launchFailure = ""
            if automation.connectOnLaunch || automation.hostedSmokeNonce != nil {
                Task { @MainActor in
                    developer.connect()
                    if let nonce = automation.hostedSmokeNonce {
                        await runHostedSmoke(developer: developer, nonce: nonce)
                    }
                }
            }
        } catch {
            model = nil
            launchFailure = error.localizedDescription
        }
    }

    var body: some Scene {
        WindowGroup("OpenRealtime Developer") {
            if let model {
                ContentView(model: model)
                    .frame(minWidth: 1160, minHeight: 760)
            } else {
                NativeLaunchFailureView(message: launchFailure)
                    .frame(minWidth: 640, minHeight: 360)
            }
        }
        .windowResizability(.contentMinSize)
        .commands {
            CommandGroup(after: .appInfo) {
                Button("Connect") { model?.connect() }
                    .keyboardShortcut("k", modifiers: [.command])
                    .disabled(model == nil || (model?.connectionState != .disconnected && model?.connectionState != .failed))
                Button("Disconnect") { model?.disconnect() }
                    .disabled(model == nil || model?.connectionState == .disconnected)
            }
        }
    }
}

private struct NativeLaunchAutomation {
    let connectOnLaunch: Bool
    let hostedSmokeNonce: String?

    static func parse(
        arguments: [String], connectArgument: String, hostedSmokePrefix: String
    ) throws -> NativeLaunchAutomation {
        let connectCount = arguments.filter { $0 == connectArgument }.count
        let smokeValues = arguments.compactMap { argument -> String? in
            guard argument.hasPrefix(hostedSmokePrefix) else { return nil }
            return String(argument.dropFirst(hostedSmokePrefix.count))
        }
        guard connectCount <= 1, smokeValues.count <= 1 else {
            throw NativeLaunchConfigurationError("native launch automation argument is duplicated")
        }
        if let nonce = smokeValues.first {
            guard nonce.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil else {
                throw NativeLaunchConfigurationError("hosted smoke nonce must be one lowercase SHA-256 value")
            }
        }
        return NativeLaunchAutomation(
            connectOnLaunch: connectCount == 1,
            hostedSmokeNonce: smokeValues.first
        )
    }
}

@MainActor
private func runHostedSmoke(developer: DeveloperModel, nonce: String) async {
    for _ in 0..<1_800 {
        if developer.connectionState == .connected, !developer.sessionID.isEmpty,
           developer.updatedSessionID == developer.sessionID {
            let proof: [String: String] = [
                "schema": "openrealtime/macos/hosted-companion-proof/v1",
                "nonce": nonce,
                "session_id": developer.sessionID,
                "transport": "websocket",
                "distribution": developer.distribution,
                "manifest_fingerprint": developer.manifestFingerprint,
                "endpoint_fingerprint": developer.endpointFingerprint,
                "endpoint": developer.endpoint,
            ]
            if let payload = try? JSONSerialization.data(
                withJSONObject: proof, options: [.sortedKeys, .withoutEscapingSlashes]
            ) {
                var output = Data("OPENREALTIME_HOSTED_COMPANION_PROOF ".utf8)
                output.append(payload)
                output.append(contentsOf: "\n".utf8)
                FileHandle.standardOutput.write(output)
                try? FileHandle.standardOutput.synchronize()
            }
            developer.shutdown()
            try? await Task.sleep(nanoseconds: 500_000_000)
            NSApplication.shared.terminate(nil)
            return
        }
        if developer.connectionState == .failed {
            break
        }
        try? await Task.sleep(nanoseconds: 50_000_000)
    }
    developer.shutdown()
    NSApplication.shared.terminate(nil)
}

private func nativeEndpointDirectoryData(_ path: String?) throws -> Data? {
    guard let path, !path.isEmpty else { return nil }
    guard path == path.trimmingCharacters(in: .whitespacesAndNewlines),
          (path as NSString).isAbsolutePath,
          !path.contains("\0"), !path.contains("\n"), !path.contains("\r") else {
        throw NativeLaunchConfigurationError("native endpoint directory path is not canonical")
    }
    let url = URL(fileURLWithPath: path, isDirectory: false)
    let values = try url.resourceValues(forKeys: [.isRegularFileKey, .fileSizeKey])
    guard values.isRegularFile == true, let size = values.fileSize, size <= 1 << 20 else {
        throw NativeLaunchConfigurationError("native endpoint directory must be a regular file of at most 1 MiB")
    }
    let handle = try FileHandle(forReadingFrom: url)
    defer { try? handle.close() }
    let data = try handle.read(upToCount: (1 << 20) + 1) ?? Data()
    guard data.count <= 1 << 20 else {
        throw NativeLaunchConfigurationError("native endpoint directory exceeds 1 MiB")
    }
    return data
}

private struct NativeLaunchConfigurationError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}

private struct NativeLaunchFailureView: View {
    let message: String

    var body: some View {
        ContentUnavailableView(
            "Native composition refused",
            systemImage: "exclamationmark.shield",
            description: Text(message)
        )
        .padding(32)
    }
}
