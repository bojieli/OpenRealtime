import SwiftUI
import Foundation

@main
struct OpenRealtimeMacApp: App {
    private let model: DeveloperModel?
    private let launchFailure: String

    init() {
        do {
            let environment = ProcessInfo.processInfo.environment
            let distribution = try NativeClientDistribution.parse(
                environment["OPENREALTIME_NATIVE_PROFILE"]
            )
            let endpoints = try nativeEndpointDirectoryData(
                environment["OPENREALTIME_NATIVE_ENDPOINT_DIRECTORY"]
            )
            model = try DeveloperModel(
                distribution: distribution, endpointDirectoryData: endpoints
            )
            launchFailure = ""
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
