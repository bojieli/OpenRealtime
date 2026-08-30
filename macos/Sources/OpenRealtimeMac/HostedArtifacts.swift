#if canImport(FoundationNetworking)
@preconcurrency import Foundation
import FoundationNetworking
#else
import Foundation
#endif
#if canImport(AppKit)
import AppKit
#endif
#if canImport(CryptoKit)
import CryptoKit
#endif
import OpenRealtimeClientCore

private final class HostedResourceTransport: NSObject, URLSessionDataDelegate, @unchecked Sendable {
    private final class Pending {
        let url: URL
        let maximumBytes: Int
        let expectedMediaType: String
        let kind: String
        let digest: String
        let version: Int
        let continuation: CheckedContinuation<Data, Error>
        var response: HTTPURLResponse?
        var data = Data()

        init(
            url: URL, maximumBytes: Int, expectedMediaType: String,
            kind: String, digest: String, version: Int,
            continuation: CheckedContinuation<Data, Error>
        ) {
            self.url = url
            self.maximumBytes = maximumBytes
            self.expectedMediaType = expectedMediaType
            self.kind = kind
            self.digest = digest
            self.version = version
            self.continuation = continuation
        }
    }

    private let lock = NSLock()
    private var pending: [Int: Pending] = [:]
    private var disposed = false
    private lazy var session: URLSession = {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.httpCookieStorage = nil
        configuration.urlCredentialStorage = nil
        configuration.httpShouldSetCookies = false
        configuration.requestCachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        return URLSession(configuration: configuration, delegate: self, delegateQueue: nil)
    }()

    func fetch(
        url: URL, maximumBytes: Int, expectedMediaType: String,
        kind: String, digest: String, version: Int
    ) async throws -> Data {
        guard maximumBytes > 0 else { throw NativeProviderError("host resource bound is invalid") }
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { continuation in
                var request = URLRequest(url: url)
                request.httpMethod = "GET"
                request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
                request.timeoutInterval = 30
                request.setValue("no-store", forHTTPHeaderField: "Cache-Control")
                request.setValue("identity", forHTTPHeaderField: "Accept-Encoding")
                let task = session.dataTask(with: request)
                lock.lock()
                if disposed {
                    lock.unlock()
                    continuation.resume(throwing: NativeProviderError("host resource transport is disposed"))
                    return
                }
                pending[task.taskIdentifier] = Pending(
                    url: url, maximumBytes: maximumBytes,
                    expectedMediaType: expectedMediaType, kind: kind,
                    digest: digest, version: version, continuation: continuation
                )
                lock.unlock()
                task.resume()
            }
        } onCancel: {
            self.cancel(url: url)
        }
    }

    func dispose() {
        lock.lock()
        guard !disposed else { lock.unlock(); return }
        disposed = true
        let values = Array(pending.values)
        pending.removeAll()
        lock.unlock()
        session.invalidateAndCancel()
        for value in values {
            value.continuation.resume(throwing: CancellationError())
        }
    }

    func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest,
        completionHandler: @escaping (URLRequest?) -> Void
    ) {
        completionHandler(nil)
    }

    func urlSession(
        _ session: URLSession,
        dataTask: URLSessionDataTask,
        didReceive response: URLResponse,
        completionHandler: @escaping (URLSession.ResponseDisposition) -> Void
    ) {
        lock.lock()
        guard let value = pending[dataTask.taskIdentifier] else {
            lock.unlock()
            completionHandler(.cancel)
            return
        }
        guard let http = response as? HTTPURLResponse,
              http.url == value.url, (200...299).contains(http.statusCode),
              http.expectedContentLength <= Int64(value.maximumBytes),
              Self.mediaType(http.mimeType, matches: value.expectedMediaType),
              http.value(forHTTPHeaderField: "X-OpenRealtime-\(value.kind)-Version") == String(value.version),
              http.value(forHTTPHeaderField: "X-OpenRealtime-\(value.kind)-Digest") == value.digest,
              http.value(forHTTPHeaderField: "ETag") == "\"\(value.digest)\"",
              http.value(forHTTPHeaderField: "Cache-Control")?.lowercased().contains("no-store") == true,
              http.value(forHTTPHeaderField: "X-Content-Type-Options")?.lowercased() == "nosniff",
              [nil, "identity"].contains(http.value(forHTTPHeaderField: "Content-Encoding")?.lowercased()) else {
            pending.removeValue(forKey: dataTask.taskIdentifier)
            lock.unlock()
            completionHandler(.cancel)
            value.continuation.resume(throwing: NativeProviderError("host resource response is invalid"))
            return
        }
        value.response = http
        lock.unlock()
        completionHandler(.allow)
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        lock.lock()
        guard let value = pending[dataTask.taskIdentifier] else { lock.unlock(); return }
        guard data.count <= value.maximumBytes - value.data.count else {
            pending.removeValue(forKey: dataTask.taskIdentifier)
            lock.unlock()
            dataTask.cancel()
            value.continuation.resume(throwing: NativeProviderError("host resource exceeds its byte bound"))
            return
        }
        value.data.append(data)
        lock.unlock()
    }

    func urlSession(
        _ session: URLSession, task: URLSessionTask,
        didCompleteWithError error: Error?
    ) {
        lock.lock()
        guard let value = pending.removeValue(forKey: task.taskIdentifier) else {
            lock.unlock()
            return
        }
        lock.unlock()
        if let error {
            value.continuation.resume(throwing: error)
        } else if value.response == nil || value.data.isEmpty {
            value.continuation.resume(throwing: NativeProviderError("host resource response is empty"))
        } else {
            value.continuation.resume(returning: value.data)
        }
    }

    private func cancel(url: URL) {
        lock.lock()
        let identifiers = pending.compactMap { $0.value.url == url ? $0.key : nil }
        lock.unlock()
        session.getAllTasks { tasks in
            for task in tasks where identifiers.contains(task.taskIdentifier) { task.cancel() }
        }
    }

    private static func mediaType(_ actual: String?, matches expected: String) -> Bool {
        guard let actual else { return false }
        let expectedBase = expected.split(separator: ";", maxSplits: 1)[0]
            .trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        return actual.lowercased() == expectedBase
    }
}

@MainActor
final class NativeArtifactsBoundary {
    private static let maximumCachedDownloads = 16
    let service: ClientArtifactsService
    private let publisher: ClientArtifactsPublisher
    private var unsubscribeEffects: (() -> Void)?
    private let artifactBase: URLComponents
    private let downloadBase: URLComponents
    private var configured = false
    private var desiredConfiguration = false
    private var transport: HostedResourceTransport?
    private var cachedFiles: [URL] = []
    private var mounted = false

    init(artifactEndpoint: String, downloadEndpoint: String) throws {
        artifactBase = try Self.resourceBase(
            artifactEndpoint, label: "artifact", schemes: ["http", "https"]
        )
        downloadBase = try Self.resourceBase(
            downloadEndpoint, label: "download", schemes: ["http", "https"]
        )
        let channel = ClientArtifactsService.makeChannel()
        service = channel.service
        publisher = channel.publisher
    }

    func mount(effects: NativeEffectsBoundary) throws {
        guard !mounted else { throw NativeProviderError("native artifacts provider mounted twice") }
        mounted = true
        configured = desiredConfiguration
        if configured { transport = HostedResourceTransport() }
        unsubscribeEffects = try effects.subscribe { [weak self] _, event in
            self?.consume(event)
        }
    }

    func unmount() {
        guard mounted else { return }
        mounted = false
        unsubscribeEffects?()
        unsubscribeEffects = nil
        configured = false
        transport?.dispose()
        transport = nil
        clearCachedFiles()
        service.suspend()
    }

    func dispose() {
        unmount()
        desiredConfiguration = false
        service.dispose()
    }

    func configure() throws {
        guard mounted else { throw NativeProviderError("native artifacts provider is unavailable") }
        desiredConfiguration = true
        configured = true
        transport?.dispose()
        transport = HostedResourceTransport()
    }

    func deactivate() {
        desiredConfiguration = false
        configured = false
        transport?.dispose()
        transport = nil
        clearCachedFiles()
    }

    func snapshot() throws -> ClientArtifactsSnapshot { try service.snapshot() }

    @discardableResult
    func subscribe(_ listener: @escaping (ClientArtifactsSnapshot) -> Void) throws -> () -> Void {
        try service.subscribe(listener)
    }

    func artifactHTML(_ reference: ClientArtifactReference) async throws -> String {
        guard let current = try service.artifact(id: reference.id), current == reference else {
            throw NativeProviderError("artifact reference is stale")
        }
        let bytes = try await fetch(
            path: reference.path, digest: reference.digest, version: reference.version,
            expectedBytes: reference.bytes,
            maximumBytes: ClientArtifactsService.maximumArtifactBytes,
            mediaType: "text/html"
        )
        guard let result = String(data: bytes, encoding: .utf8) else {
            throw NativeProviderError("artifact is not valid UTF-8 HTML")
        }
        return result
    }

    func export(_ reference: ClientDownloadReference, to destination: URL) async throws {
        guard destination.isFileURL else {
            throw NativeProviderError("download export destination must be a local file")
        }
        let bytes = try await downloadBytes(reference)
        try bytes.write(to: destination, options: .atomic)
    }

    func open(_ reference: ClientDownloadReference) async throws {
        let url = try await cachedDownload(reference)
#if canImport(AppKit)
        guard NSWorkspace.shared.open(url) else {
            throw NativeProviderError("macOS could not open the verified host file")
        }
#else
        _ = url
        throw NativeProviderError("opening a verified host file requires macOS")
#endif
    }

    func reveal(_ reference: ClientDownloadReference) async throws {
        let url = try await cachedDownload(reference)
#if canImport(AppKit)
        NSWorkspace.shared.activateFileViewerSelecting([url])
#else
        _ = url
        throw NativeProviderError("revealing a verified host file requires macOS")
#endif
    }

    private func consume(_ event: [String: Any]?) {
        guard mounted, let event, event["type"] as? String == "result",
              !event.keys.contains("error") else { return }
        do {
            if event.keys.contains("artifact") {
                guard let artifact = event["artifact"] as? [String: Any] else {
                    throw NativeProviderError("artifact reference must be an object")
                }
                try Self.onlyKeys(
                    artifact, ["id", "title", "path", "digest", "bytes", "version", "updated_at"]
                )
                try publisher.publishArtifact(
                    id: try Self.string(artifact["id"]),
                    title: try Self.string(artifact["title"]),
                    path: try Self.string(artifact["path"]),
                    digest: try Self.string(artifact["digest"]),
                    bytes: try Self.integer(artifact["bytes"]),
                    version: try Self.integer(artifact["version"]),
                    updatedAt: try Self.string(artifact["updated_at"])
                )
            }
            if event.keys.contains("download") {
                guard let download = event["download"] as? [String: Any] else {
                    throw NativeProviderError("download reference must be an object")
                }
                try Self.onlyKeys(
                    download, ["id", "filename", "media_type", "path", "digest", "bytes", "version", "updated_at"]
                )
                try publisher.publishDownload(
                    id: try Self.string(download["id"]),
                    filename: try Self.string(download["filename"]),
                    mediaType: try Self.string(download["media_type"]),
                    path: try Self.string(download["path"]),
                    digest: try Self.string(download["digest"]),
                    bytes: try Self.integer(download["bytes"]),
                    version: try Self.integer(download["version"]),
                    updatedAt: try Self.string(download["updated_at"])
                )
            }
        } catch {
            publisher.report(error.localizedDescription)
        }
    }

    private func downloadBytes(_ reference: ClientDownloadReference) async throws -> Data {
        guard let current = try service.download(id: reference.id), current == reference else {
            throw NativeProviderError("download reference is stale")
        }
        return try await fetch(
            path: reference.path, digest: reference.digest, version: reference.version,
            expectedBytes: reference.bytes,
            maximumBytes: ClientArtifactsService.maximumDownloadBytes,
            mediaType: reference.mediaType
        )
    }

    private func fetch(
        path: String, digest: String, version: Int, expectedBytes: Int,
        maximumBytes: Int, mediaType: String
    ) async throws -> Data {
        guard mounted, configured, let transport else {
            throw NativeProviderError("host resource provider is not configured")
        }
        let isArtifact = Self.belongs(path, to: artifactBase)
        let isDownload = Self.belongs(path, to: downloadBase)
        guard isArtifact != isDownload else {
            throw NativeProviderError("host resource reference does not match a declared endpoint")
        }
        var components = isArtifact ? artifactBase : downloadBase
        components.percentEncodedPath = path
        components.queryItems = [
            URLQueryItem(name: "version", value: String(version)),
            URLQueryItem(name: "digest", value: digest),
        ]
        guard let url = components.url else {
            throw NativeProviderError("host resource URL is invalid")
        }
        let data = try await transport.fetch(
            url: url, maximumBytes: maximumBytes, expectedMediaType: mediaType,
            kind: isArtifact ? "Artifact" : "Download",
            digest: digest, version: version
        )
        guard mounted, self.transport === transport,
              data.count == expectedBytes,
              try Self.digest(data) == digest else {
            throw NativeProviderError("host resource size or digest does not match its immutable reference")
        }
        return data
    }

    private func cachedDownload(_ reference: ClientDownloadReference) async throws -> URL {
        let bytes = try await downloadBytes(reference)
        let root = try FileManager.default.url(
            for: .applicationSupportDirectory, in: .userDomainMask,
            appropriateFor: nil, create: true
        ).appendingPathComponent("OpenRealtime/HostResources", isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        let url = root.appendingPathComponent(
            "\(reference.id)-v\(reference.version)-\(reference.filename)", isDirectory: false
        )
        try bytes.write(to: url, options: .atomic)
        cachedFiles.removeAll { $0 == url }
        cachedFiles.append(url)
        while cachedFiles.count > Self.maximumCachedDownloads {
            let expired = cachedFiles.removeFirst()
            try? FileManager.default.removeItem(at: expired)
        }
        return url
    }

    private func clearCachedFiles() {
        for url in cachedFiles { try? FileManager.default.removeItem(at: url) }
        cachedFiles.removeAll()
    }

    private static func resourceBase(
        _ endpoint: String, label: String, schemes: Set<String>
    ) throws -> URLComponents {
        guard let components = URLComponents(string: endpoint),
              let scheme = components.scheme, schemes.contains(scheme),
              let host = components.host, !host.isEmpty,
              components.user == nil, components.password == nil,
              components.query == nil, components.fragment == nil,
              !components.percentEncodedPath.isEmpty,
              !components.percentEncodedPath.hasSuffix("/"),
              components.url?.absoluteString == endpoint else {
            throw NativeProviderError("the declared host \(label) endpoint is invalid")
        }
        return components
    }

    private static func belongs(_ path: String, to base: URLComponents) -> Bool {
        path.hasPrefix(base.percentEncodedPath + "/") &&
            !path.dropFirst(base.percentEncodedPath.count + 1).isEmpty &&
            !path.contains("..") && !path.contains("?") && !path.contains("#")
    }

    private static func onlyKeys(_ value: [String: Any], _ allowed: Set<String>) throws {
        guard value.keys.allSatisfy(allowed.contains) else {
            throw NativeProviderError("host resource reference contains an unknown field")
        }
    }

    private static func string(_ value: Any?) throws -> String {
        guard let value = value as? String else {
            throw NativeProviderError("host resource reference string is invalid")
        }
        return value
    }

    private static func integer(_ value: Any?) throws -> Int {
        guard !(value is Bool), let number = value as? NSNumber,
              number.doubleValue.isFinite,
              number.doubleValue.rounded(.towardZero) == number.doubleValue else {
            throw NativeProviderError("host resource reference integer is invalid")
        }
        return number.intValue
    }

    private static func digest(_ data: Data) throws -> String {
#if canImport(CryptoKit)
        return "sha256:" + SHA256.hash(data: data).map {
            String(format: "%02x", $0)
        }.joined()
#else
        _ = data
        throw NativeProviderError("SHA-256 verification requires the native CryptoKit adapter")
#endif
    }
}
