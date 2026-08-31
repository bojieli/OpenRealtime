import Foundation
import OpenRealtimeClientCore

@MainActor
final class NativeClientAssembly {
    let manifest: NativeClientManifest
    let endpointDirectory: NativeEndpointDirectory
    let composition: NativeClientComposition
    let view: NativeViewBoundary

    init(
        distribution: NativeClientDistribution = .observerDeveloper,
        manifestData: Data? = nil,
        endpointDirectoryData: Data? = nil,
        registry: NativeClientProviderRegistry? = nil
    ) throws {
        let source: Data
        if let manifestData {
            source = manifestData
        } else {
            guard let url = Bundle.module.url(
                forResource: distribution.manifestResourceName,
                withExtension: "json", subdirectory: "Resources"
            ) ?? Bundle.module.url(
                forResource: distribution.manifestResourceName, withExtension: "json"
            ) else {
                throw NativeAssemblyError(
                    "the bundled \(distribution.rawValue) native client manifest is unavailable"
                )
            }
            source = try Data(contentsOf: url, options: [.mappedIfSafe])
        }
        let decodedManifest = try NativeClientManifest.decodeStrict(source)
        let endpointSource: Data
        if let endpointDirectoryData {
            endpointSource = endpointDirectoryData
        } else {
            guard let url = Bundle.module.url(
                forResource: distribution.endpointDirectoryResourceName,
                withExtension: "json", subdirectory: "Resources"
            ) ?? Bundle.module.url(
                forResource: distribution.endpointDirectoryResourceName, withExtension: "json"
            ) else {
                throw NativeAssemblyError(
                    "the bundled \(distribution.rawValue) native endpoint directory is unavailable"
                )
            }
            endpointSource = try Data(contentsOf: url, options: [.mappedIfSafe])
        }
        let decodedEndpoints = try NativeEndpointDirectory.decodeStrict(endpointSource)
        try decodedEndpoints.validate(selectedBy: decodedManifest)
        let installed = try registry ?? NativeMacProviderRegistry.makeStandard(
            endpointDirectory: decodedEndpoints
        )
        let assembledComposition = try NativeClientComposition(
            manifest: decodedManifest, registry: installed
        )
        try assembledComposition.start()
        guard let assembledView = try? assembledComposition.service(.view) as? NativeViewBoundary else {
            try? assembledComposition.stop()
            throw NativeAssemblyError("native composition did not publish its declared view provider")
        }
        manifest = decodedManifest
        endpointDirectory = decodedEndpoints
        composition = assembledComposition
        view = assembledView
    }
}

private extension NativeClientDistribution {
    var manifestResourceName: String {
        switch self {
        case .effectsDeveloper: return "native-client-manifest"
        case .observerDeveloper: return "native-observer-client-manifest"
        }
    }

    var endpointDirectoryResourceName: String {
        switch self {
        case .effectsDeveloper: return "native-endpoints"
        case .observerDeveloper: return "native-observer-endpoints"
        }
    }
}

@MainActor
private enum NativeMacProviderRegistry {
    static func makeStandard(
        endpointDirectory: NativeEndpointDirectory
    ) throws -> NativeClientProviderRegistry {
        let registry = NativeClientProviderRegistry()
        let state = NativeMacFactoryState()

        try register(registry, selection(
            "slots", .slots, "macos.swiftui-slots.v1"
        )) { context in
            adapter(context, instance: NativeBoundary())
        }
        try register(registry, selection(
            "strict-json", .strictJSON, "portable.swift-strict-json.v1"
        )) { context in
            let codec = StrictJSONService()
            return adapter(context, instance: codec, onDispose: { codec.dispose() })
        }
        try register(registry, selection(
            "transport", .connection, "macos.urlsession-websocket.v1",
            requires: [.strictJSON], permissions: nativePermissionCeiling(.connection)
        )) { context in
            let codec = try context.service(.strictJSON, as: StrictJSONService.self)
            let endpoint = try endpointDirectory.endpoint(
                named: .realtimeWebSocket,
                protocol: NativeEndpoint.realtimeWebSocketProtocol
            )
            let transport = try RealtimeClient(strictJSON: codec, endpoint: endpoint.url)
            return adapter(
                context, instance: transport,
                onStop: { transport.disconnect(reason: "native transport provider stopped") }
            )
        }
        try register(registry, selection(
            "transport-diagnostics", .transportDiagnostics,
            "macos.websocket-diagnostics.v1", requires: [.connection]
        )) { context in
            let transport = try context.service(.connection, as: RealtimeClient.self)
            let channel = TransportDiagnosticsService.makeChannel()
            return adapter(
                context, instance: channel.service,
                onStart: { try transport.bindDiagnostics(channel.publisher) },
                onStop: {
                    transport.unbindDiagnostics(channel.publisher)
                    channel.service.suspend()
                },
                onDispose: { channel.service.dispose() }
            )
        }
        try register(registry, selection(
            "reducer", .reducer, "portable.swift-reducer.v1",
            requires: [.strictJSON, .connection]
        )) { context in
            _ = try context.service(.strictJSON, as: StrictJSONService.self)
            let transport = try context.service(.connection, as: RealtimeClient.self)
            let reducer = NativeReducerController(transport: transport)
            return adapter(
                context, instance: reducer,
                onStop: { reducer.suspend(reason: "native reducer provider stopped") }
            )
        }
        try register(registry, selection(
            "protocol-events", .protocolEvents, "portable.swift-protocol-events.v1",
            requires: [.reducer]
        )) { context in
            let reducer = try context.service(.reducer, as: NativeReducerController.self)
            let channel = ValidatedProtocolEventService.makeChannel()
            return adapter(
                context, instance: channel.service,
                onStart: { try reducer.bindProtocolEvents(channel.publisher) },
                onStop: {
                    reducer.unbindProtocolEvents(channel.publisher)
                    channel.service.suspend()
                },
                onDispose: { channel.service.dispose() }
            )
        }
        try register(registry, selection(
            "inspection-access", .inspectionAccess,
            "portable.swift-inspection-access.v1", requires: [.reducer]
        )) { context in
            let reducer = try context.service(.reducer, as: NativeReducerController.self)
            let access = SessionInspectionAccessService()
            let client = SessionInspectionClient(accessSource: access)
            let endpoint = try endpointDirectory.endpoint(
                named: .management, protocol: NativeEndpoint.managementProtocol
            )
            try client.configure(managementEndpoint: endpoint.url)
            state.inspectionClients[ObjectIdentifier(access)] = client
            return adapter(
                context, instance: access,
                onStart: { try reducer.bindInspection(access: access, client: client) },
                onStop: {
                    reducer.unbindInspection(access: access, client: client)
                    client.deactivate()
                },
                onDispose: {
                    state.inspectionClients.removeValue(forKey: ObjectIdentifier(access))
                    client.dispose()
                    access.dispose()
                }
            )
        }
        try register(registry, selection(
            "session-configuration", .sessionConfiguration,
            "portable.swift-session-configuration.v1", requires: [.reducer]
        )) { context in
            let reducer = try context.service(.reducer, as: NativeReducerController.self)
            let service = SessionConfigurationService { [weak reducer] value in
                guard let reducer else {
                    throw NativeAssemblyError("native session state provider is unavailable")
                }
                try reducer.sessionUpdateThrowing(value)
            }
            return adapter(
                context, instance: service,
                onStart: { try reducer.bindSessionConfiguration(service) },
                onStop: {
                    reducer.unbindSessionConfiguration(service)
                    service.suspend()
                },
                onDispose: { service.dispose() }
            )
        }
        try register(registry, selection(
            "media", .media, "macos.av-media.v2",
            requires: [.connection, .reducer, .protocolEvents],
            permissions: nativePermissionCeiling(.media)
        )) { context in
            let media = NativeMediaBoundary(
                transport: try context.service(.connection, as: RealtimeClient.self),
                reducer: try context.service(.reducer, as: NativeReducerController.self),
                protocolEvents: try context.service(
                    .protocolEvents, as: ValidatedProtocolEventService.self
                )
            )
            return adapter(
                context, instance: media,
                onStart: { try media.mount() }, onStop: { media.unmount() }
            )
        }
        try register(registry, selection(
            "video", .video, "macos.video-protocol.v2",
            requires: [.connection, .reducer, .sessionConfiguration],
            permissions: nativePermissionCeiling(.video)
        )) { context in
            let video = NativeVideoBoundary(
                transport: try context.service(.connection, as: RealtimeClient.self),
                reducer: try context.service(.reducer, as: NativeReducerController.self),
                configuration: try context.service(
                    .sessionConfiguration, as: SessionConfigurationService.self
                )
            )
            return adapter(
                context, instance: video,
                onStart: { try video.mount() }, onStop: { video.unmount() }
            )
        }
        try register(registry, selection(
            "effects", .effects, "macos.host-effects.v1",
            requires: [.strictJSON, .reducer, .protocolEvents, .sessionConfiguration],
            permissions: nativePermissionCeiling(.effects)
        )) { context in
            let effects = try NativeEffectsBoundary(
                reducer: try context.service(.reducer, as: NativeReducerController.self),
                strictJSON: try context.service(.strictJSON, as: StrictJSONService.self),
                protocolEvents: try context.service(
                    .protocolEvents, as: ValidatedProtocolEventService.self
                ),
                sessionConfiguration: try context.service(
                    .sessionConfiguration, as: SessionConfigurationService.self
                ),
                declaration: try context.endpoint(named: "effects.local"),
                endpoint: try endpointDirectory.endpoint(
                    named: .effects, protocol: NativeEndpoint.effectsProtocol
                ).url
            )
            return adapter(
                context, instance: effects,
                onStart: { try effects.mount() }, onStop: { effects.unmount() },
                onDispose: { effects.dispose() }
            )
        }
        try register(registry, selection(
            "artifacts", .artifacts, "macos.host-resource-references.v1",
            requires: [.effects], permissions: nativePermissionCeiling(.artifacts)
        )) { context in
            let effects = try context.service(.effects, as: NativeEffectsBoundary.self)
            let artifacts = try NativeArtifactsBoundary(
                artifactEndpoint: endpointDirectory.endpoint(
                    named: .artifacts, protocol: NativeEndpoint.artifactsProtocol
                ).url,
                downloadEndpoint: endpointDirectory.endpoint(
                    named: .downloads, protocol: NativeEndpoint.downloadsProtocol
                ).url
            )
            return adapter(
                context, instance: artifacts,
                onStart: { try artifacts.mount(effects: effects) },
                onStop: { artifacts.unmount() }, onDispose: { artifacts.dispose() }
            )
        }
        try register(registry, selection(
            "inspection", .inspection, "macos.inspection.v1",
            requires: [.inspectionAccess], permissions: nativePermissionCeiling(.inspection)
        )) { context in
            let access = try context.service(
                .inspectionAccess, as: SessionInspectionAccessService.self
            )
            guard let client = state.inspectionClients[ObjectIdentifier(access)] else {
                throw NativeAssemblyError("native inspection client owner is unavailable")
            }
            let inspection = NativeInspectionBoundary(client: client)
            return adapter(
                context, instance: inspection,
                onStop: { inspection.suspend() }, onDispose: { inspection.dispose() }
            )
        }
        try registerView(
            registry, implementation: "macos.swiftui-view.v1", effectsEnabled: true
        )
        try registerView(
            registry, implementation: "macos.swiftui-observer-view.v1", effectsEnabled: false
        )
        return registry
    }

    private static func registerView(
        _ registry: NativeClientProviderRegistry,
        implementation: String,
        effectsEnabled: Bool
    ) throws {
        var requires: [NativeClientService] = [
            .slots, .transportDiagnostics, .reducer, .media, .video,
        ]
        if effectsEnabled { requires += [.effects, .artifacts] }
        requires.append(.inspection)
        let row = selection("view", .view, implementation, requires: requires)
        try register(registry, row) { context in
            _ = try context.service(.slots)
            let services = NativeViewServices(
                transportDiagnostics: try context.service(
                    .transportDiagnostics, as: TransportDiagnosticsService.self
                ),
                reducer: try context.service(.reducer, as: NativeReducerController.self),
                media: try context.service(.media, as: NativeMediaBoundary.self),
                video: try context.service(.video, as: NativeVideoBoundary.self),
                effects: effectsEnabled ? try context.service(
                    .effects, as: NativeEffectsBoundary.self
                ) : nil,
                artifacts: effectsEnabled ? try context.service(
                    .artifacts, as: NativeArtifactsBoundary.self
                ) : nil,
                inspection: try context.service(
                    .inspection, as: NativeInspectionBoundary.self
                )
            )
            let view = NativeViewBoundary(services: services)
            return adapter(
                context, instance: view,
                onStart: { try view.mount() }, onStop: { view.unmount() }
            )
        }
    }

    private static func register(
        _ registry: NativeClientProviderRegistry,
        _ selection: NativeProviderSelection,
        factory: @escaping NativeClientProviderFactoryRegistration.Factory
    ) throws {
        try registry.register(NativeClientProviderFactoryRegistration(
            selection: selection, factory: factory
        ))
    }

    private static func selection(
        _ id: String, _ service: NativeClientService, _ implementation: String,
        requires: [NativeClientService] = [],
        permissions: [NativeClientPermission] = []
    ) -> NativeProviderSelection {
        NativeProviderSelection(
            id: id, service: service, implementation: implementation,
            requires: requires, permissions: permissions
        )
    }

    private static func adapter(
        _ context: NativeProviderFactoryContext,
        instance: AnyObject,
        onStart: @escaping @MainActor () throws -> Void = {},
        onStop: @escaping @MainActor () -> Void = {},
        onDispose: @escaping @MainActor () -> Void = {}
    ) -> NativeAdapterProvider {
        NativeAdapterProvider(
            selection: context.selection, permissions: context.selection.permissions,
            instance: instance, onStart: onStart, onStop: onStop,
            onDispose: onDispose
        )
    }
}

@MainActor
private final class NativeMacFactoryState {
    var inspectionClients: [ObjectIdentifier: SessionInspectionClient] = [:]
}

final class NativeInspectionBoundary {
    let client: SessionInspectionClient

    init(client: SessionInspectionClient) { self.client = client }

    var available: Bool { client.available() }
    var access: SessionInspectionAccessProjection? { client.access() }
    @discardableResult
    func subscribe(
        _ listener: @escaping (SessionInspectionAccessProjection?) -> Void
    ) -> () -> Void {
        client.subscribe(listener)
    }
    func live() async throws -> SessionInspectionDocument { try await client.live() }
    func deltas(after: Int = 0, limit: Int = 256) async throws -> SessionInspectionDocument {
        try await client.deltas(after: after, limit: limit)
    }
    func trace() async throws -> SessionInspectionDocument { try await client.trace() }
    func protocolPayload(_ event: [String: Any]) -> String { safeProtocolPayload(event) }
    func suspend() { client.deactivate() }
    func dispose() { client.dispose() }
}

private final class NativeBoundary: NSObject {}

@MainActor
struct NativeViewServices {
    let transportDiagnostics: TransportDiagnosticsService
    let reducer: NativeReducerController
    let media: NativeMediaBoundary
    let video: NativeVideoBoundary
    let effects: NativeEffectsBoundary?
    let artifacts: NativeArtifactsBoundary?
    let inspection: NativeInspectionBoundary
}

@MainActor
final class NativeViewBoundary {
    let services: NativeViewServices
    private var active = false
    private var onMount: (() -> Void)?
    private var onUnmount: (() -> Void)?

    init(services: NativeViewServices) { self.services = services }

    func mount() throws {
        guard !active else { throw NativeAssemblyError("native view provider mounted twice") }
        active = true
        onMount?()
    }

    func unmount() {
        guard active else { return }
        onUnmount?()
        active = false
    }

    func attach(onMount: @escaping () -> Void, onUnmount: @escaping () -> Void) {
        self.onMount = onMount
        self.onUnmount = onUnmount
        if active { onMount() }
    }

    func detach() {
        if active { onUnmount?() }
        onMount = nil
        onUnmount = nil
    }
}

@MainActor
private final class NativeAdapterProvider: NativeClientProvider {
    let service: NativeClientService
    let implementation: String
    let permissions: [NativeClientPermission]
    let instance: AnyObject
    private let requires: [NativeClientService]
    private let onStop: @MainActor () -> Void
    private let onStart: @MainActor () throws -> Void
    private let onDispose: @MainActor () -> Void
    private var active = false
    private var disposed = false

    init(
        selection: NativeProviderSelection,
        permissions: [NativeClientPermission],
        instance: AnyObject,
        onStart: @escaping @MainActor () throws -> Void,
        onStop: @escaping @MainActor () -> Void,
        onDispose: @escaping @MainActor () -> Void
    ) {
        service = selection.service
        implementation = selection.implementation
        requires = selection.requires
        self.permissions = permissions
        self.instance = instance
        self.onStart = onStart
        self.onStop = onStop
        self.onDispose = onDispose
    }

    func start(dependencies: NativeProviderDependencies) throws {
        guard !disposed, !active else { throw NativeAssemblyError("native provider cannot be started") }
        for dependency in requires { _ = try dependencies.service(dependency) }
        for permission in permissions {
            for operation in permission.operations where !dependencies.allows(
                kind: permission.kind, resource: permission.resource, operation: operation
            ) {
                throw NativeAssemblyError("native provider permission was not granted by its manifest")
            }
        }
        do {
            try onStart()
            active = true
        } catch {
            onStop()
            throw error
        }
    }

    func stop() throws {
        guard active else { return }
        active = false
        onStop()
    }

    func dispose() throws {
        guard !disposed else { return }
        if active { try stop() }
        disposed = true
        onDispose()
    }
}

private struct NativeAssemblyError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
