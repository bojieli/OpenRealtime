import SwiftUI
import Foundation
import AppKit
import CryptoKit
import Darwin
import OpenRealtimeClientCore

@main
struct OpenRealtimeMacApp: App {
    private static let connectOnLaunchArgument = "--openrealtime-connect-on-launch"
    private static let hostedSmokePrefix = "--openrealtime-hosted-smoke="

    @ObservedObject private var host: NativeClientHost

    init() {
        let hostedSmokeRequested = CommandLine.arguments.contains {
            $0.hasPrefix(Self.hostedSmokePrefix)
        }
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
            let assembled = NativeClientHost(
                distribution: distribution, pinnedDirectoryData: endpoints
            )
            host = assembled
            if automation.connectOnLaunch || automation.hostedSmokeNonce != nil,
               let developer = assembled.model {
                Task { @MainActor in
                    developer.connect()
                    if let nonce = automation.hostedSmokeNonce {
                        await runHostedSmoke(
                            developer: developer,
                            nonce: nonce,
                            snapshotPath: environment["OPENREALTIME_HOSTED_MANAGEMENT_SNAPSHOT"]
                        )
                    }
                }
            }
        } catch {
            host = NativeClientHost(refusal: error.localizedDescription)
            if hostedSmokeRequested {
                hostedSmokeRecord(
                    "FAILURE", "native client initialization failed: \(error.localizedDescription)"
                )
                Darwin.exit(EXIT_FAILURE)
            }
        }
    }

    var body: some Scene {
        WindowGroup("OpenRealtime Developer") {
            NativeRootView(host: host)
        }
        .windowResizability(.contentMinSize)
        .commands {
            CommandGroup(after: .appInfo) {
                Button("Connect") { host.model?.connect() }
                    .keyboardShortcut("k", modifiers: [.command])
                    .disabled(host.model == nil ||
                              (host.model?.connectionState != .disconnected &&
                               host.model?.connectionState != .failed))
                Button("Disconnect") { host.model?.disconnect() }
                    .disabled(host.model == nil || host.model?.connectionState == .disconnected)
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
private func runHostedSmoke(
    developer: DeveloperModel, nonce: String, snapshotPath: String?
) async {
    hostedSmokeRecord("PROGRESS", "launch accepted")
    var lastFailure = "hosted management snapshot was not ready"
    var lastReadiness = ""
    let deadline = ProcessInfo.processInfo.systemUptime + 60
    while ProcessInfo.processInfo.systemUptime < deadline {
        let hasSession = !developer.sessionID.isEmpty
        let hasUpdatedSession = !developer.updatedSessionID.isEmpty
        let sessionMatches = hasSession && hasUpdatedSession &&
            developer.updatedSessionID == developer.sessionID
        let readiness = "phase=\(developer.connectionState.rawValue) session=\(hasSession) updated=\(hasUpdatedSession) match=\(sessionMatches)"
        if readiness != lastReadiness {
            lastReadiness = readiness
            lastFailure = "native client not proof-ready: \(readiness)"
            hostedSmokeRecord("PROGRESS", readiness)
        }
        if developer.connectionState == .connected, sessionMatches {
            let management: (evidence: [String: Any], payload: Data)
            do {
                let document = try await developer.hostedManagementSnapshot(
                    expectedSessionID: developer.sessionID
                )
                management = try hostedManagementEvidence(
                    document, expectedSessionID: developer.sessionID
                )
                try publishHostedManagementSnapshot(
                    management.payload, path: snapshotPath
                )
            } catch {
                let failure = hostedSmokeMessage(error.localizedDescription)
                if failure != lastFailure {
                    lastFailure = failure
                    hostedSmokeRecord("PROGRESS", failure)
                }
                try? await Task.sleep(nanoseconds: 50_000_000)
                continue
            }
            let proof: [String: Any] = [
                "schema": "openrealtime/macos/hosted-companion-proof/v3",
                "nonce": nonce,
                "session_id": developer.sessionID,
                "transport": "websocket",
                "distribution": developer.distribution,
                "manifest_fingerprint": developer.manifestFingerprint,
                "endpoint_fingerprint": developer.endpointFingerprint,
                "endpoint": developer.endpoint,
                "management": management.evidence,
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
            lastFailure = "native realtime connection failed"
            break
        }
        try? await Task.sleep(nanoseconds: 50_000_000)
    }
    hostedSmokeRecord("FAILURE", lastFailure)
    developer.shutdown()
    NSApplication.shared.terminate(nil)
}

private func hostedSmokeMessage(_ value: String) -> String {
    String(
        value.replacingOccurrences(of: "\r", with: " ")
            .replacingOccurrences(of: "\n", with: " ")
            .prefix(512)
    )
}

private func hostedSmokeRecord(_ kind: String, _ message: String) {
    let output = Data(
        "OPENREALTIME_HOSTED_COMPANION_\(kind) \(hostedSmokeMessage(message))\n".utf8
    )
    FileHandle.standardError.write(output)
    try? FileHandle.standardError.synchronize()
}

@MainActor
private func hostedManagementEvidence(
    _ document: SessionInspectionDocument, expectedSessionID: String
) throws -> (evidence: [String: Any], payload: Data) {
    let data = document.encoded()
    guard
          document.resource == .live, document.sessionID == expectedSessionID,
          !document.responseURL.isEmpty, !data.isEmpty,
          let live = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
          let formatVersion = live["format_version"] as? NSNumber,
          let graphID = live["graph_id"] as? String,
          let graphRevision = live["graph_revision"] as? NSNumber,
          let fingerprint = live["fingerprint"] as? String,
          let configuration = live["configuration"] as? [String: Any],
          let configurationDigest = configuration["digest"] as? String,
          let sequence = live["sequence"] as? NSNumber,
          let state = live["state"] as? String,
          formatVersion.uint64Value == 1, !graphID.isEmpty,
          graphRevision.uint64Value > 0, fingerprint.hasPrefix("sha256:"),
          configurationDigest.hasPrefix("sha256:"), sequence.uint64Value > 0,
          state == "running" else {
        throw NativeLaunchConfigurationError(
            "hosted management document is not canonical running live evidence"
        )
    }
    return (evidence: [
        "session_id": document.sessionID,
        "resource": "live",
        "response_url": document.responseURL,
        "payload_bytes": data.count,
        "payload_digest": "sha256:" + SHA256.hash(data: data).map {
            String(format: "%02x", $0)
        }.joined(),
    ], payload: data)
}

private func publishHostedManagementSnapshot(_ data: Data, path: String?) throws {
    guard let path, !path.isEmpty,
          path == path.trimmingCharacters(in: .whitespacesAndNewlines),
          (path as NSString).isAbsolutePath,
          !path.contains("\0"), !path.contains("\r"), !path.contains("\n"),
          !data.isEmpty, data.count <= SessionInspectionClient.maximumResponseBytes else {
        throw NativeLaunchConfigurationError("hosted management snapshot path or payload is invalid")
    }
    let canonical = (path as NSString).standardizingPath
    let parent = (path as NSString).deletingLastPathComponent
    let basename = (path as NSString).lastPathComponent
    guard canonical == path, !parent.isEmpty, !basename.isEmpty,
          basename != ".", basename != ".." else {
        throw NativeLaunchConfigurationError("hosted management snapshot path is not canonical")
    }
    var parentInfo = stat()
    guard lstat(parent, &parentInfo) == 0,
          (parentInfo.st_mode & S_IFMT) == S_IFDIR,
          (parentInfo.st_mode & 0o777) == 0o700,
          parentInfo.st_uid == geteuid() else {
        throw NativeLaunchConfigurationError("hosted management snapshot parent is not private")
    }
    var absent = stat()
    guard lstat(path, &absent) != 0, errno == ENOENT else {
        throw NativeLaunchConfigurationError("hosted management snapshot already exists")
    }
    var descriptor = path.withCString {
        Darwin.open(
            $0, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW,
            mode_t(S_IRUSR | S_IWUSR)
        )
    }
    guard descriptor >= 0 else {
        throw NativeLaunchConfigurationError("hosted management snapshot create failed")
    }
    var keep = false
    defer {
        if descriptor >= 0 { _ = Darwin.close(descriptor) }
        if !keep { _ = path.withCString { Darwin.unlink($0) } }
    }
    try data.withUnsafeBytes { raw in
        guard let base = raw.baseAddress else {
            throw NativeLaunchConfigurationError("hosted management snapshot payload is empty")
        }
        var offset = 0
        while offset < raw.count {
            let written = Darwin.write(
                descriptor, base.advanced(by: offset), raw.count - offset
            )
            if written < 0, errno == EINTR { continue }
            guard written > 0 else {
                throw NativeLaunchConfigurationError("hosted management snapshot write failed")
            }
            offset += written
        }
    }
    guard Darwin.fsync(descriptor) == 0 else {
        throw NativeLaunchConfigurationError("hosted management snapshot sync failed")
    }
    var fileInfo = stat()
    guard Darwin.fstat(descriptor, &fileInfo) == 0,
          (fileInfo.st_mode & S_IFMT) == S_IFREG,
          (fileInfo.st_mode & 0o777) == 0o600,
          fileInfo.st_uid == geteuid(), fileInfo.st_nlink == 1,
          fileInfo.st_size == off_t(data.count) else {
        throw NativeLaunchConfigurationError("hosted management snapshot is not one exact private file")
    }
    let closeResult = Darwin.close(descriptor)
    descriptor = -1
    guard closeResult == 0 else {
        throw NativeLaunchConfigurationError("hosted management snapshot close failed")
    }
    keep = true
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

// AppKit derives the window's frame autosave name from the type of the scene's
// content. A conditional built inline from a file-private view mangles to
// "(unknown context at $<address>)", so the name changed on every launch: the
// window never restored its size or position, and each launch left another
// orphan key pair in the preferences. One named, non-private root view keeps
// that identity stable across launches.
struct NativeRootView: View {
    @ObservedObject var host: NativeClientHost

    var body: some View {
        if let model = host.model {
            ContentView(model: model, host: host)
                .frame(minWidth: 1160, minHeight: 760)
        } else {
            NativeLaunchFailureView(message: host.launchFailure)
                .frame(minWidth: 640, minHeight: 360)
        }
    }
}

struct NativeLaunchFailureView: View {
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
