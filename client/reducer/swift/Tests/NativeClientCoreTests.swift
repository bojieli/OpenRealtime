import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif
import XCTest
@testable import OpenRealtimeClientCore

final class NativeClientCoreTests: XCTestCase {
    func testAllSharedVectors() throws {
        let corpusURL = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .appendingPathComponent("../../testdata/reducer_vectors.json")
            .standardizedFileURL
        let data = try Data(contentsOf: corpusURL)
        var parser = StrictJSONParser(data: data)
        let corpus = try objectValue(parser.parse(), "corpus")
        let limits = ReducerLimits()
        XCTAssertTrue(limits.matches(try objectValue(corpus["limits"], "corpus.limits")))
        let vectors = try XCTUnwrap(corpus["vectors"] as? [Any])
        XCTAssertEqual(vectors.count, 9)
        for value in vectors {
            let vector = try objectValue(value, "vector")
            let machine = PortableClientReducer(limits: limits)
            var errors: [String] = []
            for (index, stepValue) in (vector["steps"] as? [Any] ?? []).enumerated() {
                let step = try objectValue(stepValue, "step")
                do {
                    try machine.apply(
                        atMS: try integerValue(step["at_ms"], "at_ms", minimum: 0),
                        operation: try objectValue(step["operation"], "operation")
                    )
                } catch {
                    errors.append("step \(index): \(error.localizedDescription)")
                }
            }
            let expect = try objectValue(vector["expect"], "expect")
            XCTAssertEqual(try testCanonicalJSON(machine.snapshotObject()), try testCanonicalJSON(expect["snapshot"] as Any))
            XCTAssertEqual(try testCanonicalJSON(machine.outbound), try testCanonicalJSON(expect["outbound"] as Any))
            XCTAssertEqual(try testCanonicalJSON(errors), try testCanonicalJSON(expect["errors"] as Any))
        }
    }

    func testStrictEventParserRejectsDuplicateKeysAndBoundsInput() throws {
        XCTAssertThrowsError(try StrictRealtimeJSON.object(from: Data(#"{"type":"one","type":"two"}"#.utf8)))
        XCTAssertThrowsError(try StrictRealtimeJSON.object(from: Data(repeating: 0x20, count: RealtimeReducerService.maxEventBytes + 1)))
        let nested = String(repeating: "[", count: 257) + "0" + String(repeating: "]", count: 257)
        XCTAssertThrowsError(try StrictRealtimeJSON.object(from: Data(nested.utf8)))
        let event = try StrictRealtimeJSON.object(from: Data(#"{"type":"session.created","session":{"id":"s1"}}"#.utf8))
        XCTAssertEqual(event["type"] as? String, "session.created")
    }

    func testStrictJSONAndValidatedProtocolEventsAreScopedBoundedAndImmutable() async throws {
        try await MainActor.run {
            let strict = StrictJSONService()
            XCTAssertThrowsError(try strict.parse(Data(#"{"type":"one","type":"two"}"#.utf8)))
            let stable = try strict.stable(["z": 1, "type": "session.created"])
            XCTAssertEqual(String(data: stable, encoding: .utf8), #"{"type":"session.created","z":1}"#)

            let channel = ValidatedProtocolEventService.makeChannel()
            var received: [ValidatedProtocolEvent] = []
            let unsubscribe = try channel.service.subscribe { received.append($0) }
            var mutable: [String: Any] = [
                "type": "response.output_text.delta", "delta": "hello",
                "nested": ["value": "original"],
            ]
            try channel.publisher.publish(mutable)
            mutable["nested"] = ["value": "changed"]
            XCTAssertEqual(received.count, 1)
            XCTAssertEqual(
                (try received[0].snapshot()["nested"] as? [String: Any])?["value"] as? String,
                "original"
            )
            XCTAssertThrowsError(try channel.publisher.publish(["delta": "missing type"]))
            unsubscribe()
            try channel.publisher.publish(["type": "session.updated"])
            XCTAssertEqual(received.count, 1)
            channel.service.dispose()
            XCTAssertThrowsError(try channel.publisher.publish(["type": "session.updated"]))
            strict.dispose()
            XCTAssertThrowsError(try strict.parse(Data(#"{"type":"session.created"}"#.utf8)))
        }
    }

    func testClientEffectInvocationRequiresExactAuthorityAndOnlyReturnsBoundedWire() async throws {
        try await MainActor.run {
            let codec = StrictJSONService()
            let digest = "sha256:" + String(repeating: "a", count: 64)
            let authority = "opaque_server_authority"
            let event: [String: Any] = [
                "type": "response.function_call_arguments.done",
                "call_id": "call:1", "name": "display_artifact",
                "arguments": #"{"title":"Report"}"#,
                "openrealtime": ["client_effect": [
                    "version": 1, "declaration_digest": digest,
                    "authority": authority,
                ]],
            ]
            let call = try ClientEffectInvocationEncoder.encode(
                event: event, sessionID: "sess:test-1",
                declarationName: "display_artifact", declarationDigest: digest,
                maximumMessageBytes: 16 << 10, codec: codec
            )
            XCTAssertEqual(call.id, "call:1")
            XCTAssertEqual(call.name, "display_artifact")
            XCTAssertEqual(
                try codec.parse(call.arguments())["title"] as? String, "Report"
            )
            let wire = try codec.parse(call.encoded(), maximumBytes: 16 << 10)
            XCTAssertEqual(wire["authority"] as? String, authority)
            XCTAssertEqual(wire["session_id"] as? String, "sess:test-1")
            XCTAssertEqual(Set(wire.keys), Set([
                "type", "session_id", "id", "name", "arguments", "authority",
            ]))
            XCTAssertFalse(String(describing: call).contains(authority))
            let projection = ProtocolEventPresentation.redacted(event)
            let projectionData = try JSONSerialization.data(
                withJSONObject: projection, options: [.sortedKeys]
            )
            let projectionText = String(data: projectionData, encoding: .utf8)!
            XCTAssertFalse(projectionText.contains(authority))
            XCTAssertTrue(projectionText.contains(digest))
            let nested = ProtocolEventPresentation.redacted([
                "items": [["token": "mgmt_secret", "frame": String(repeating: "a", count: 100)]]
            ])
            let nestedText = String(data: try JSONSerialization.data(
                withJSONObject: nested, options: [.sortedKeys]
            ), encoding: .utf8)!
            XCTAssertFalse(nestedText.contains("mgmt_secret"))
            XCTAssertFalse(nestedText.contains(String(repeating: "a", count: 100)))

            var missing = event
            missing["openrealtime"] = ["client_effect": [
                "version": 1, "declaration_digest": digest,
            ]]
            XCTAssertThrowsError(try ClientEffectInvocationEncoder.encode(
                event: missing, sessionID: "sess:test-1",
                declarationName: "display_artifact", declarationDigest: digest,
                maximumMessageBytes: 16 << 10, codec: codec
            ))
            var fabricated = event
            fabricated["openrealtime"] = ["client_effect": [
                "version": 1, "declaration_digest": digest,
                "authority": "contains whitespace", "extra": true,
            ]]
            XCTAssertThrowsError(try ClientEffectInvocationEncoder.encode(
                event: fabricated, sessionID: "sess:test-1",
                declarationName: "display_artifact", declarationDigest: digest,
                maximumMessageBytes: 16 << 10, codec: codec
            ))
            var booleanVersion = event
            booleanVersion["openrealtime"] = ["client_effect": [
                "version": true, "declaration_digest": digest,
                "authority": authority,
            ]]
            XCTAssertThrowsError(try ClientEffectInvocationEncoder.encode(
                event: booleanVersion, sessionID: "sess:test-1",
                declarationName: "display_artifact", declarationDigest: digest,
                maximumMessageBytes: 16 << 10, codec: codec
            ))
        }
    }

    func testSessionConfigurationMergesAtomicallyAndReconcilesLifecycle() async throws {
        try await MainActor.run {
            var applied: [[String: Any]] = []
            var failOnce = true
            let service = SessionConfigurationService { value in
                if failOnce {
                    failOnce = false
                    throw ReducerFailure("synthetic update failure")
                }
                applied.append(value)
            }
            var snapshots: [SessionConfigurationSnapshot] = []
            let unsubscribe = try service.subscribe { snapshots.append($0) }
            let removeVideo = try service.contribute([
                "supports": ["video.input", "observations", "video.input"],
                "observers": ["video", "audio"],
            ])
            let tool: [String: Any] = [
                "type": "function", "name": "read_file",
                "parameters": ["type": "object", "properties": [:]],
            ]
            let removeTools = try service.contribute([
                "supports": ["computer_use"], "tools": [tool],
                "debug": ["enabled": true, "categories": ["tools", "video"]],
            ])
            let connected: [String: Any] = [
                "connection": ["phase": "connected"], "session": ["id": "sess-1"],
            ]
            service.observe(state: connected)
            XCTAssertEqual(applied.count, 0)
            XCTAssertFalse(try service.snapshot().diagnostic.isEmpty)
            service.observe(state: connected)
            XCTAssertEqual(applied.count, 1)
            let merged = try service.snapshot().configuration()
            let extensionObject = try XCTUnwrap(merged["openrealtime"] as? [String: Any])
            XCTAssertEqual(
                extensionObject["supports"] as? [String],
                ["computer_use", "observations", "video.input"]
            )
            XCTAssertEqual(extensionObject["observers"] as? [String], ["audio", "video"])
            XCTAssertEqual((merged["tools"] as? [[String: Any]])?.count, 1)

            var callerCopy = merged
            callerCopy["tools"] = []
            XCTAssertEqual((try service.snapshot().configuration()["tools"] as? [[String: Any]])?.count, 1)
            XCTAssertThrowsError(try service.contribute(["tools": "not an array"]))
            XCTAssertThrowsError(try service.contribute([
                "tools": [["type": "function", "name": "read_file", "description": "conflict"]],
            ]))
            XCTAssertEqual(try service.snapshot().contributions, 2)

            removeVideo()
            XCTAssertEqual(applied.count, 2)
            removeTools()
            XCTAssertEqual(applied.count, 3)
            service.observe(state: ["connection": ["phase": "disconnected"], "session": [:]])
            XCTAssertEqual(try service.snapshot().sessionID, "")
            unsubscribe()
            service.dispose()
            XCTAssertThrowsError(try service.contribute(["supports": ["video.input"]]))
            XCTAssertFalse(snapshots.isEmpty)
        }
    }

    func testTransportDiagnosticsRemainPayloadFreeAndDisposeCleanly() async throws {
        try await MainActor.run {
            let channel = TransportDiagnosticsService.makeChannel()
            var snapshots: [TransportDiagnosticsSnapshot] = []
            let unsubscribe = try channel.service.subscribe { snapshots.append($0) }
            channel.publisher.updateState("connected")
            channel.publisher.recordOutbound([
                "type": "input_audio_buffer.append", "audio": Data(repeating: 7, count: 12).base64EncodedString(),
            ], queuedMessages: 2)
            channel.publisher.recordOutbound([
                "type": "openrealtime.input_video_frame.append", "frame": Data(repeating: 8, count: 15).base64EncodedString(),
            ])
            channel.publisher.recordInbound([
                "type": "response.output_audio.delta", "delta": Data(repeating: 9, count: 18).base64EncodedString(),
            ])
            let current = try channel.service.snapshot()
            XCTAssertEqual(current.state, "connected")
            XCTAssertEqual(current.outboundEvents, 2)
            XCTAssertEqual(current.inboundEvents, 1)
            XCTAssertEqual(current.inputAudioBytes, 12)
            XCTAssertEqual(current.inputVideoBytes, 15)
            XCTAssertEqual(current.outputAudioBytes, 18)
            XCTAssertEqual(current.queuedMessages, 0)
            unsubscribe()
            let count = snapshots.count
            channel.publisher.updateState("disconnected")
            XCTAssertEqual(snapshots.count, count)
            channel.service.dispose()
            XCTAssertThrowsError(try channel.service.snapshot())
        }
    }

    func testArtifactReferencesAreBoundedImmutableVersionedAndFailClosed() async throws {
        try await MainActor.run {
            let channel = ClientArtifactsService.makeChannel()
            var updates: [ClientArtifactsSnapshot] = []
            let unsubscribe = try channel.service.subscribe { updates.append($0) }
            try channel.publisher.publishArtifact(
                id: "report", title: "Report", path: "/client/v1/artifacts/report",
                digest: "sha256:" + String(repeating: "a", count: 64),
                bytes: 128, version: 1, updatedAt: "2026-08-29T19:00:00.123456789Z"
            )
            var copy = try channel.service.snapshot().artifacts
            copy.removeAll()
            XCTAssertEqual(try channel.service.snapshot().artifacts.count, 1)
            XCTAssertThrowsError(try channel.publisher.publishArtifact(
                id: "report", title: "Report", path: "/client/v1/artifacts/report",
                digest: "sha256:" + String(repeating: "a", count: 64),
                bytes: 128, version: 1, updatedAt: "2026-08-29T19:00:00Z"
            ))
            try channel.publisher.publishDownload(
                id: "file", filename: "generated.txt", mediaType: "text/plain",
                path: "/client/v1/downloads/file",
                digest: "sha256:" + String(repeating: "b", count: 64),
                bytes: 8, version: 1, updatedAt: "2026-08-29T19:00:00Z"
            )
            XCTAssertThrowsError(try channel.publisher.publishDownload(
                id: "escape", filename: "../escape", mediaType: "text/plain",
                path: "/client/v1/downloads/escape",
                digest: "sha256:" + String(repeating: "c", count: 64),
                bytes: 8, version: 1, updatedAt: "2026-08-29T19:00:00Z"
            ))
            XCTAssertEqual(try channel.service.snapshot().artifacts.count, 1)
            XCTAssertEqual(try channel.service.snapshot().downloads.count, 1)
            channel.publisher.report("invalid resource reference")
            XCTAssertEqual(try channel.service.snapshot().diagnostic, "invalid resource reference")
            unsubscribe()
            XCTAssertFalse(updates.isEmpty)
            channel.service.dispose()
            XCTAssertThrowsError(try channel.service.snapshot())
        }
    }

    func testManifestCompositionLifecycleAndDeclaredDependencies() async throws {
        try await MainActor.run {
            let manifest = try fixtureManifest()
            var lifecycle: [String] = []
            let providers = manifest.providers.map { row in
                FixtureProvider(selection: row, lifecycle: { lifecycle.append($0) })
            }
            let composition = try NativeClientComposition(manifest: manifest, providers: providers)
            try composition.start()
            XCTAssertEqual(composition.state, .active)
            XCTAssertNotNil(try composition.service(.reducer))
            try composition.stop()
            XCTAssertEqual(composition.state, .stopped)
            XCTAssertEqual(lifecycle.filter { $0.hasPrefix("stop:") }, manifest.providers.reversed().map { "stop:\($0.id)" })
            XCTAssertThrowsError(try composition.start())
        }
    }

    func testObserverManifestOmitsEffectsArtifactsAndImplicitEndpoint() throws {
        let full = try fixtureManifest()
        var rows = full.providers.filter {
            $0.service != .effects && $0.service != .artifacts && $0.service != .view
        }
        rows.append(NativeProviderSelection(
            id: "view", service: .view,
            implementation: "macos.swiftui-observer-view.v1",
            requires: [
                .slots, .transportDiagnostics, .reducer, .media, .video, .inspection,
            ]
        ))
        let observer = NativeClientManifest(endpoints: [], providers: rows)
        try observer.validate()
        XCTAssertFalse(observer.providers.contains { $0.service == .effects })
        XCTAssertFalse(observer.providers.contains { $0.service == .artifacts })
        XCTAssertTrue(observer.endpoints.isEmpty)

        XCTAssertThrowsError(try NativeClientManifest(
            endpoints: full.endpoints, providers: rows
        ).validate())
        XCTAssertThrowsError(try NativeClientManifest(
            endpoints: [], providers: full.providers
        ).validate())
    }

    func testRegistryPreflightsExactSelectionsAndInstantiatesOnlySelectedProviders() async throws {
        try await MainActor.run {
            let full = try fixtureManifest()
            let selectedRows = Array(full.providers.prefix(6))
            let selected = NativeClientManifest(endpoints: [], providers: selectedRows)
            let registry = NativeClientProviderRegistry()
            var constructed: [String] = []
            for row in full.providers {
                try registry.register(NativeClientProviderFactoryRegistration(
                    selection: row
                ) { context in
                    constructed.append(context.selection.id)
                    return FixtureProvider(selection: context.selection, lifecycle: { _ in })
                })
            }

            let providers = try registry.providers(for: selected)
            XCTAssertEqual(constructed, selectedRows.map(\.id))
            XCTAssertEqual(providers.count, selectedRows.count)
            XCTAssertFalse(constructed.contains("effects"))
            XCTAssertFalse(constructed.contains("artifacts"))

            let missing = NativeClientProviderRegistry()
            var unexpectedConstruction = 0
            for row in selectedRows.dropLast() {
                try missing.register(NativeClientProviderFactoryRegistration(
                    selection: row
                ) { context in
                    unexpectedConstruction += 1
                    return FixtureProvider(selection: context.selection, lifecycle: { _ in })
                })
            }
            XCTAssertThrowsError(try missing.providers(for: selected))
            XCTAssertEqual(unexpectedConstruction, 0)

            let substituted = NativeClientProviderRegistry()
            for row in selectedRows {
                var installed = row
                if row.service == .reducer {
                    installed = NativeProviderSelection(
                        id: row.id, service: row.service,
                        implementation: row.implementation,
                        requires: [.connection], permissions: row.permissions
                    )
                }
                try substituted.register(NativeClientProviderFactoryRegistration(
                    selection: installed
                ) { context in
                    FixtureProvider(selection: context.selection, lifecycle: { _ in })
                })
            }
            XCTAssertThrowsError(try substituted.providers(for: selected))
        }
    }

    func testRegistryConstructionUsesOnlyDeclaredDependenciesAndRollsBack() async throws {
        try await MainActor.run {
            let full = try fixtureManifest()
            let selectedRows = Array(full.providers.prefix(6))
            let selected = NativeClientManifest(endpoints: [], providers: selectedRows)
            let registry = NativeClientProviderRegistry()
            var lifecycle: [String] = []
            for row in selectedRows {
                try registry.register(NativeClientProviderFactoryRegistration(
                    selection: row
                ) { context in
                    if context.selection.service == .reducer {
                        _ = try context.service(.effects)
                    }
                    return FixtureProvider(
                        selection: context.selection,
                        lifecycle: { lifecycle.append($0) }
                    )
                })
            }
            XCTAssertThrowsError(try registry.providers(for: selected))
            XCTAssertEqual(
                lifecycle.filter { $0.hasPrefix("stop:") },
                selectedRows.prefix { $0.service != .reducer }.reversed().map { "stop:\($0.id)" }
            )
        }
    }

    func testProviderLossQuiescesDependentsAndSupportsCleanRemount() async throws {
        try await MainActor.run {
            let manifest = try fixtureManifest()
            var lifecycle: [String] = []
            let providers = manifest.providers.map { row in
                FixtureProvider(selection: row, lifecycle: { lifecycle.append($0) })
            }
            let composition = try NativeClientComposition(manifest: manifest, providers: providers)
            try composition.start()
            lifecycle.removeAll()
            try composition.providerLost(.media)
            XCTAssertEqual(composition.state, .degraded)
            XCTAssertNotNil(try composition.service(.artifacts))
            XCTAssertThrowsError(try composition.service(.media))
            XCTAssertEqual(lifecycle, ["stop:view", "stop:media"])
            lifecycle.removeAll()
            try composition.remount()
            XCTAssertEqual(composition.state, .active)
            XCTAssertNotNil(try composition.service(.video))
            XCTAssertEqual(lifecycle, ["start:media", "start:view"])
            lifecycle.removeAll()
            try composition.providerLost(.effects)
            XCTAssertNotNil(try composition.service(.video))
            XCTAssertThrowsError(try composition.service(.artifacts))
            XCTAssertEqual(lifecycle, ["stop:view", "stop:artifacts", "stop:effects"])
            lifecycle.removeAll()
            try composition.remount()
            XCTAssertEqual(lifecycle, ["start:effects", "start:artifacts", "start:view"])
            try composition.stop()
        }
    }

    func testCompositionRollsBackAndReplacementMustMatchManifest() async throws {
        try await MainActor.run {
            let manifest = try fixtureManifest()
            var lifecycle: [String] = []
            let providers = manifest.providers.map { row in
                FixtureProvider(selection: row, failStart: row.service == .media, lifecycle: { lifecycle.append($0) })
            }
            let composition = try NativeClientComposition(manifest: manifest, providers: providers)
            XCTAssertThrowsError(try composition.start())
            XCTAssertEqual(composition.state, .stopped)
            let mountedBeforeFailure = manifest.providers.prefix { $0.service != .media }
            XCTAssertEqual(
                lifecycle.filter { $0.hasPrefix("stop:") },
                mountedBeforeFailure.reversed().map { "stop:\($0.id)" }
            )

            var wrong = manifest.providers.map { FixtureProvider(selection: $0, lifecycle: { _ in }) }
            let selected = manifest.providers.first(where: { $0.service == .reducer })!
            wrong.removeAll { $0.service == .reducer }
            wrong.append(FixtureProvider(selection: NativeProviderSelection(
                id: selected.id, service: selected.service, implementation: "macos.reducer.replacement"
            ), lifecycle: { _ in }))
            XCTAssertThrowsError(try NativeClientComposition(manifest: manifest, providers: wrong))
        }
    }

    func testFailedRemountRollsBackAndCanBeRetried() async throws {
        try await MainActor.run {
            let manifest = try fixtureManifest()
            var lifecycle: [String] = []
            let providers = manifest.providers.map { row in
                RetryFixtureProvider(
                    selection: row, lifecycle: { lifecycle.append($0) }
                )
            }
            let composition = try NativeClientComposition(
                manifest: manifest, providers: providers
            )
            try composition.start()
            try composition.providerLost(.media)
            lifecycle.removeAll()
            let media = try XCTUnwrap(providers.first { $0.service == .media })
            media.failNextStart = true
            XCTAssertThrowsError(try composition.remount())
            XCTAssertEqual(composition.state, .degraded)
            XCTAssertThrowsError(try composition.service(.media))
            XCTAssertNotNil(try composition.service(.artifacts))
            try composition.remount()
            XCTAssertEqual(composition.state, .active)
            XCTAssertNotNil(try composition.service(.view))
            try composition.stop()
        }
    }

    func testManifestRejectsUnknownKeysOrderingAndPermissionEscalation() throws {
        let valid = try fixtureManifest()
        let encoded = try JSONEncoder().encode(valid)
        XCTAssertEqual(try NativeClientManifest.decodeStrict(encoded), valid)

        var object = try JSONSerialization.jsonObject(with: encoded) as! [String: Any]
        object["secret"] = true
        XCTAssertThrowsError(try NativeClientManifest.decodeStrict(JSONSerialization.data(withJSONObject: object)))

        var rows = valid.providers
        let transport = rows.remove(at: 1)
        rows.append(transport)
        XCTAssertThrowsError(try NativeClientManifest(providers: rows).validate())

        rows = valid.providers
        rows[1] = NativeProviderSelection(
            id: rows[1].id, service: .connection, implementation: rows[1].implementation,
            permissions: [NativeClientPermission(kind: "network.connect", resource: "realtime-endpoint", operations: ["raw-socket"])]
        )
        XCTAssertThrowsError(try NativeClientManifest(providers: rows).validate())
    }

    func testManifestPinsExactHostEffectsProtocolPathAndCatalog() throws {
        let valid = try fixtureManifest()
        let endpoint = try XCTUnwrap(valid.endpoints.first)
        XCTAssertEqual(endpoint.name, "effects.local")
        XCTAssertEqual(endpoint.method, "GET")
        XCTAssertEqual(endpoint.path, "/client/v1/effects")
        XCTAssertEqual(endpoint.protocolName, "openrealtime.client-effects.v1")
        XCTAssertEqual(endpoint.catalogDigest, NativeManifestEndpoint.defaultEffectsCatalogDigest)

        let alternateDigest = "sha256:" + String(repeating: "9", count: 64)
        let alternate = NativeClientManifest(
            endpoints: [NativeManifestEndpoint(
                name: endpoint.name, method: endpoint.method, path: endpoint.path,
                protocolName: endpoint.protocolName, catalogDigest: alternateDigest
            )],
            providers: valid.providers
        )
        try alternate.validate()
        XCTAssertEqual(alternate.endpoints.first?.catalogDigest, alternateDigest)
        let alternateURL = try NativeEffectEndpointResolver.websocketURL(
            endpoint: "wss://effects.example:9443/client/v1/effects",
            declaration: try XCTUnwrap(alternate.endpoints.first)
        )
        XCTAssertEqual(alternateURL.absoluteString, "wss://effects.example:9443/client/v1/effects")
        for replacement in [
            NativeManifestEndpoint(
                name: "effects.local", method: "POST", path: endpoint.path,
                protocolName: endpoint.protocolName, catalogDigest: endpoint.catalogDigest
            ),
            NativeManifestEndpoint(
                name: endpoint.name, method: endpoint.method, path: "/client/v1/other",
                protocolName: endpoint.protocolName, catalogDigest: endpoint.catalogDigest
            ),
            NativeManifestEndpoint(
                name: endpoint.name, method: endpoint.method, path: endpoint.path,
                protocolName: "openrealtime.client-effects.v2", catalogDigest: endpoint.catalogDigest
            ),
            NativeManifestEndpoint(
                name: endpoint.name, method: endpoint.method, path: endpoint.path,
                protocolName: endpoint.protocolName, catalogDigest: "sha256:not-a-digest"
            ),
        ] {
            XCTAssertThrowsError(try NativeClientManifest(
                endpoints: [replacement], providers: valid.providers
            ).validate())
            XCTAssertThrowsError(try NativeEffectEndpointResolver.websocketURL(
                endpoint: "wss://effects.example/client/v1/effects",
                declaration: replacement
            ))
        }

        var object = try JSONSerialization.jsonObject(
            with: JSONEncoder().encode(valid)
        ) as! [String: Any]
        var endpoints = object["endpoints"] as! [[String: Any]]
        endpoints[0]["authority"] = "must never be accepted from a manifest"
        object["endpoints"] = endpoints
        XCTAssertThrowsError(try NativeClientManifest.decodeStrict(
            JSONSerialization.data(withJSONObject: object)
        ))
    }

    func testEndpointDirectoryIsExactTamperEvidentAndSnapshotIsolated() throws {
        var source = [
            NativeEndpoint(
                name: .management, protocolName: NativeEndpoint.managementProtocol,
                url: "https://management.example/custom/v7"
            ),
            NativeEndpoint(
                name: .realtimeWebSocket,
                protocolName: NativeEndpoint.realtimeWebSocketProtocol,
                url: "wss://realtime.example/client/v1/realtime"
            ),
            NativeEndpoint(
                name: .effects, protocolName: NativeEndpoint.effectsProtocol,
                url: "wss://effects.example/client/v1/effects"
            ),
        ]
        let directory = try NativeEndpointDirectory.freeze(source)
        try directory.validate()
        XCTAssertTrue(directory.fingerprint.hasPrefix("sha256:"))
        XCTAssertEqual(directory.endpoints.map(\.name), [.effects, .management, .realtimeWebSocket])
        let reordered = try NativeEndpointDirectory.freeze(source.reversed())
        XCTAssertEqual(reordered.fingerprint, directory.fingerprint)

        source[0] = NativeEndpoint(
            name: .management, protocolName: NativeEndpoint.managementProtocol,
            url: "https://attacker.invalid/custom/v7"
        )
        XCTAssertEqual(
            try directory.endpoint(named: .management, protocol: NativeEndpoint.managementProtocol).url,
            "https://management.example/custom/v7"
        )
        XCTAssertThrowsError(try directory.endpoint(
            named: .artifacts, protocol: NativeEndpoint.artifactsProtocol
        ))

        let encoded = try JSONEncoder().encode(directory)
        XCTAssertEqual(try NativeEndpointDirectory.decodeStrict(encoded), directory)
        var object = try JSONSerialization.jsonObject(with: encoded) as! [String: Any]
        var endpoints = object["endpoints"] as! [[String: Any]]
        endpoints[0]["url"] = "wss://other.example/client/v1/effects"
        object["endpoints"] = endpoints
        XCTAssertThrowsError(try NativeEndpointDirectory.decodeStrict(
            JSONSerialization.data(withJSONObject: object)
        ))
        endpoints[0]["credential"] = "forbidden"
        object["endpoints"] = endpoints
        XCTAssertThrowsError(try NativeEndpointDirectory.decodeStrict(
            JSONSerialization.data(withJSONObject: object)
        ))

        for invalid in [
            NativeEndpoint(
                name: .management, protocolName: NativeEndpoint.managementProtocol,
                url: "https://operator:secret@management.example/custom/v7"
            ),
            NativeEndpoint(
                name: .realtimeWebSocket,
                protocolName: NativeEndpoint.realtimeWebSocketProtocol,
                url: "wss://realtime.example/client/v1/realtime?token=secret"
            ),
            NativeEndpoint(
                name: .effects, protocolName: "openrealtime.client-effects.v2",
                url: "wss://effects.example/client/v1/effects"
            ),
        ] {
            XCTAssertThrowsError(try NativeEndpointDirectory.freeze([invalid]))
        }

        let reducer = RealtimeReducerService()
        try reducer.apply(atMS: 1, operation: ["kind": "connect", "transport": "websocket"])
        let snapshot = try JSONSerialization.data(
            withJSONObject: reducer.snapshot(), options: [.sortedKeys]
        )
        let text = try XCTUnwrap(String(data: snapshot, encoding: .utf8))
        XCTAssertFalse(text.contains("realtime.example"))
        XCTAssertFalse(text.contains(directory.fingerprint))

        let observerDefault = try NativeEndpointDirectory.freeze([
            NativeEndpoint(
                name: .realtimeWebSocket,
                protocolName: NativeEndpoint.realtimeWebSocketProtocol,
                url: "ws://127.0.0.1:8765/client/v1/realtime"
            ),
            NativeEndpoint(
                name: .management, protocolName: NativeEndpoint.managementProtocol,
                url: "http://127.0.0.1:8765/client/v1/management"
            ),
        ])
        XCTAssertEqual(
            observerDefault.fingerprint,
            "sha256:fdedb71a808407f1fd9926fe52ff91ecd623d8f345858159c925148a3c98f93f"
        )
        let effectsDefault = try NativeEndpointDirectory.freeze(
            observerDefault.endpoints + [
                NativeEndpoint(
                    name: .effects, protocolName: NativeEndpoint.effectsProtocol,
                    url: "ws://127.0.0.1:8765/client/v1/effects"
                ),
                NativeEndpoint(
                    name: .artifacts, protocolName: NativeEndpoint.artifactsProtocol,
                    url: "http://127.0.0.1:8765/client/v1/artifacts"
                ),
                NativeEndpoint(
                    name: .downloads, protocolName: NativeEndpoint.downloadsProtocol,
                    url: "http://127.0.0.1:8765/client/v1/downloads"
                ),
            ]
        )
        XCTAssertEqual(
            effectsDefault.fingerprint,
            "sha256:d19b581f1a0be1ef2128307987c5fa70828bddc1c1bb474b608fcae5db07d0b2"
        )

        let selectedManifest = try fixtureManifest()
        let selectedDirectory = try fixtureEndpointDirectory()
        try selectedDirectory.validate(selectedBy: selectedManifest)
        let withoutManagement = try NativeEndpointDirectory.freeze(
            selectedDirectory.endpoints.filter { $0.name != .management }
        )
        XCTAssertThrowsError(try withoutManagement.validate(selectedBy: selectedManifest))
        let substitutedEffect = try NativeEndpointDirectory.freeze(
            selectedDirectory.endpoints.map { endpoint in
                endpoint.name == .effects ? NativeEndpoint(
                    name: .effects, protocolName: NativeEndpoint.effectsProtocol,
                    url: "wss://effects.example/client/v1/substituted"
                ) : endpoint
            }
        )
        XCTAssertThrowsError(try substitutedEffect.validate(selectedBy: selectedManifest))
        let substitutedArtifact = try NativeEndpointDirectory.freeze(
            selectedDirectory.endpoints.map { endpoint in
                endpoint.name == .artifacts ? NativeEndpoint(
                    name: .artifacts, protocolName: NativeEndpoint.artifactsProtocol,
                    url: "https://artifacts.example/client/v1/substituted"
                ) : endpoint
            }
        )
        XCTAssertThrowsError(try substitutedArtifact.validate(selectedBy: selectedManifest))
    }

    func testNativeDistributionParserFailsClosedOnUnknownValues() throws {
        XCTAssertEqual(try NativeClientDistribution.parse(nil), .observerDeveloper)
        XCTAssertEqual(try NativeClientDistribution.parse(""), .observerDeveloper)
        XCTAssertEqual(
            try NativeClientDistribution.parse("effects-developer"), .effectsDeveloper
        )
        XCTAssertEqual(
            try NativeClientDistribution.parse("observer-developer"), .observerDeveloper
        )
        XCTAssertThrowsError(try NativeClientDistribution.parse("observer"))
        XCTAssertThrowsError(try NativeClientDistribution.parse(" effects-developer"))
    }

    func testInspectionAccessIsStrictRedactedRotatedExpiredAndDisposed() throws {
        let clock = TestClock(1_000)
        let access = SessionInspectionAccessService(clock: { clock.value })
        var projections: [SessionInspectionAccessProjection?] = []
        let unsubscribe = access.subscribe { projections.append($0) }

        try access.capture(event: inspectionEvent(
            token: "mgmt_first-token", expiresAtMS: 2_000
        ))
        XCTAssertEqual(access.current(), SessionInspectionAccessProjection(
            sessionID: "sess:test-1", expiresAtMS: 2_000
        ))
        let publicJSON = try JSONEncoder().encode(try XCTUnwrap(access.current()))
        let publicText = try XCTUnwrap(String(data: publicJSON, encoding: .utf8))
        XCTAssertFalse(publicText.contains("mgmt_first-token"))
        XCTAssertFalse(publicText.contains("/v1/realtime"))
        XCTAssertThrowsError(try access.capture(event: inspectionEvent(
            token: "mgmt_stale-path", expiresAtMS: 2_500,
            path: "/v1/realtime/sessions/sess:test-1/live"
        )))
        XCTAssertEqual(access.current()?.expiresAtMS, 2_000)

        // An ordinary session update must not replay or revoke a capability.
        try access.capture(event: [
            "type": "session.updated",
            "session": ["id": "sess:test-1", "openrealtime": ["debug": ["enabled": true]]],
        ])
        XCTAssertEqual(access.current()?.expiresAtMS, 2_000)

        try access.capture(event: inspectionEvent(
            token: "mgmt_rotated-token", expiresAtMS: 3_000
        ))
        XCTAssertEqual(access.current()?.expiresAtMS, 3_000)
        XCTAssertThrowsError(try access.capture(event: inspectionEvent(
            token: "Bearer escaped", expiresAtMS: 4_000
        )))
        XCTAssertEqual(access.current()?.expiresAtMS, 3_000)

        try access.capture(event: [
            "type": "session.updated",
            "session": ["id": "sess:test-1", "openrealtime": ["debug": ["enabled": false]]],
        ])
        XCTAssertNil(access.current())

        try access.capture(event: inspectionEvent(
            token: "mgmt_expiring-token", expiresAtMS: 5_000
        ))
        clock.value = 5_000
        XCTAssertNil(access.current())
        access.dispose()
        XCTAssertThrowsError(try access.capture(event: inspectionEvent(
            token: "mgmt_after-disposal", expiresAtMS: 6_000
        )))
        XCTAssertNil(projections.last!)
        unsubscribe()
    }

    func testInspectionHTTPClientUsesCanonicalBoundedHeaderOnlyAPIAndImmutableDocuments() async throws {
        InspectionURLProtocol.reset()
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [InspectionURLProtocol.self]
        let clock = TestClock(1_000)
        let access = SessionInspectionAccessService(clock: { clock.value })
        let client = SessionInspectionClient(accessSource: access, configuration: configuration)
        try client.configure(managementEndpoint: "https://management.example:9443/custom/management/v7")
        try access.capture(event: inspectionEvent(
            token: "mgmt_header-only", expiresAtMS: 10_000
        ))

        let live = try await client.live()
        XCTAssertEqual(live.resource, .live)
        XCTAssertEqual(live.sessionID, "sess:test-1")
        XCTAssertEqual(
            live.responseURL,
            "https://management.example:9443/custom/management/v7/sessions/sess%3Atest-1/live"
        )
        var first = try live.snapshot()
        first["graph_id"] = "mutated"
        XCTAssertEqual(try live.snapshot()["graph_id"] as? String, "compat_graph")
        _ = try await client.deltas(after: 7, limit: 9)
        _ = try await client.trace()

        let requests = InspectionURLProtocol.requests()
        XCTAssertEqual(requests.count, 3)
        XCTAssertEqual(requests.map { $0.url?.scheme }, ["https", "https", "https"])
        XCTAssertEqual(requests.map { $0.url?.host }, ["management.example", "management.example", "management.example"])
        XCTAssertEqual(requests.map { $0.url?.port }, [9443, 9443, 9443])
        XCTAssertEqual(
            URLComponents(url: requests[0].url!, resolvingAgainstBaseURL: false)?.percentEncodedPath,
            "/custom/management/v7/sessions/sess%3Atest-1/live"
        )
        XCTAssertEqual(
            URLComponents(url: requests[1].url!, resolvingAgainstBaseURL: false)?.percentEncodedPath,
            "/custom/management/v7/sessions/sess%3Atest-1/deltas"
        )
        XCTAssertEqual(URLComponents(url: requests[1].url!, resolvingAgainstBaseURL: false)?.queryItems, [
            URLQueryItem(name: "after", value: "7"), URLQueryItem(name: "limit", value: "9"),
        ])
        for request in requests {
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertNil(request.httpBody)
            XCTAssertNil(request.value(forHTTPHeaderField: "Authorization"))
            XCTAssertEqual(
                request.value(forHTTPHeaderField: SessionInspectionClient.capabilityHeader),
                "mgmt_header-only"
            )
            XCTAssertFalse(request.url!.absoluteString.contains("mgmt_header-only"))
        }

        InspectionURLProtocol.setOnRequest {
            try? access.capture(event: inspectionEvent(
                token: "mgmt_rotated-during-read", expiresAtMS: 10_500
            ))
        }
        do {
            _ = try await client.live()
            XCTFail("inspection accepted a response after its capability changed")
        } catch {
            XCTAssertTrue(error.localizedDescription.contains("changed during request"))
        }
        InspectionURLProtocol.setOnRequest(nil)

        try await withThrowingTaskGroup(of: String?.self) { group in
            for _ in 0..<32 {
                group.addTask { try await client.live().snapshot()["graph_id"] as? String }
            }
            for try await graphID in group { XCTAssertEqual(graphID, "compat_graph") }
        }

        do {
            _ = try await client.deltas(after: -1, limit: 1)
            XCTFail("an unbounded delta query was accepted")
        } catch {}
        InspectionURLProtocol.setOversized(true)
        do {
            _ = try await client.live()
            XCTFail("an oversized inspection response was accepted")
        } catch {}
        InspectionURLProtocol.setOversized(false)

        XCTAssertThrowsError(try client.configure(
            managementEndpoint: "https://operator:secret@management.example/custom/v7"
        ))
        XCTAssertThrowsError(try client.configure(
            managementEndpoint: "https://management.example/custom/v7?token=secret"
        ))

        client.deactivate()
        XCTAssertFalse(client.available())
        do {
            _ = try await client.trace()
            XCTFail("inspection remained available after provider loss")
        } catch {}
        try access.capture(event: inspectionEvent(
            token: "mgmt_provider-loss", expiresAtMS: 11_000
        ))
        XCTAssertTrue(client.available())
        access.dispose()
        XCTAssertFalse(client.available())
        client.dispose()
        client.dispose()
        XCTAssertThrowsError(try client.configure(
            managementEndpoint: "https://management.example/custom/v7"
        ))
    }

    func testNativeAuthoringClientRendersExactMetadataThroughHeaderOnlyCapability() async throws {
        AuthoringURLProtocol.reset()
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [AuthoringURLProtocol.self]
        let clock = TestClock(1_000)
        let client = NativeAuthoringClient(configuration: configuration, clock: { clock.value })
        try client.configure(managementEndpoint: "https://management.example:9443/custom/management/v7")
        let initial = try client.replaceCapability("operator_header-only", expiresAtMS: 10_000)
        XCTAssertTrue(initial.available)
        AuthoringURLProtocol.setResponse(
            try testCanonicalJSON(authoringAnalysisFixture()),
            identity: "authoring:analyze:\(authoringSourceDigest)"
        )

        let presentation = try await client.analyze(
            path: "agent.ortg", source: authoringSource, revision: 7
        )
        XCTAssertEqual(presentation.sourceDigest, authoringSourceDigest)
        XCTAssertEqual(presentation.total, 2)
        XCTAssertFalse(presentation.incomplete)
        XCTAssertEqual(presentation.contracts.map(\.elementName), ["test.Empty", "test.Source"])
        let empty = presentation.contracts[0]
        XCTAssertEqual(empty.schemaStatus, "empty-object-only")
        XCTAssertTrue(empty.emptyObjectOnly)
        XCTAssertTrue(empty.propertiesComplete)
        XCTAssertNil(empty.schemaReference)
        let source = presentation.contracts[1]
        XCTAssertEqual(source.elementRevision, 2)
        XCTAssertEqual(source.schemaReference, "schema://test/source-config/v1")
        XCTAssertEqual(source.schemaID, "https://schemas.example.test/source-config-v1.json")
        XCTAssertEqual(source.additionalPropertiesJSON, "false")
        XCTAssertEqual(source.properties.map(\.name), ["model", "temperature"])
        XCTAssertEqual(source.properties[0].types, ["null", "string"])
        XCTAssertTrue(source.properties[0].required)
        XCTAssertEqual(source.properties[0].title, "<b>Model</b>")
        XCTAssertEqual(source.properties[0].description, "first line\nsecond line")
        XCTAssertEqual(source.properties[0].format, "model-name")
        XCTAssertEqual(source.properties[0].defaultJSON, "null")
        XCTAssertEqual(source.properties[0].enumJSON ?? [], ["null", #""large""#])
        XCTAssertTrue(source.properties[0].schemaJSON.contains(#""title":"<b>Model</b>""#))
        XCTAssertNil(source.properties[1].title)
        XCTAssertNil(source.properties[1].defaultJSON)
        XCTAssertNil(source.properties[1].enumJSON)
        for fragment in [
            #"element: "test.Source""#, "element revision: 2",
            #"schema status: "resolved""#, #"schema reference: "schema://test/source-config/v1""#,
            "additional properties: false", #"property[0].title: "<b>Model</b>""#,
            #"property[0].description: "first line\nsecond line""#,
            "property[0].default: null", #"property[0].enum: [null,"large"]"#,
            "property[1].title: absent", "property[1].default: absent",
        ] {
            XCTAssertTrue(presentation.plaintext.contains(fragment), "missing \(fragment)")
        }

        let requests = AuthoringURLProtocol.requests()
        XCTAssertEqual(requests.count, 1)
        let request = try XCTUnwrap(requests.first)
        XCTAssertEqual(request.httpMethod, "POST")
        XCTAssertEqual(
            URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.percentEncodedPath,
            "/custom/management/v7/authoring/analyze"
        )
        XCTAssertEqual(
            request.value(forHTTPHeaderField: NativeAuthoringClient.capabilityHeader),
            "operator_header-only"
        )
        XCTAssertNil(request.value(forHTTPHeaderField: "Authorization"))
        XCTAssertFalse(request.url!.absoluteString.contains("operator_header-only"))
        let requestBody = try StrictRealtimeJSON.object(from: try XCTUnwrap(request.httpBody), maximumBytes: 16 << 20)
        XCTAssertEqual(requestBody["path"] as? String, "agent.ortg")
        XCTAssertEqual(requestBody["source"] as? String, authoringSource)
        XCTAssertEqual(try integerValue(requestBody["revision"], "revision", minimum: 1), 7)
        XCTAssertFalse(String(data: request.httpBody!, encoding: .utf8)!.contains("operator_header-only"))

        AuthoringURLProtocol.setOnRequest {
            _ = try? client.replaceCapability("operator_rotated", expiresAtMS: 11_000)
        }
        do {
            _ = try await client.analyze(path: "agent.ortg", source: authoringSource)
            XCTFail("native authoring admitted a response after capability rotation")
        } catch {
            XCTAssertTrue(error.localizedDescription.contains("changed during request"))
        }
        AuthoringURLProtocol.setOnRequest(nil)
        clock.value = 11_000
        XCTAssertFalse(client.capabilityStatus().available)
        XCTAssertThrowsError(try client.replaceCapability(" bad "))
        XCTAssertThrowsError(try client.replaceCapability("line\nbreak"))
        XCTAssertThrowsError(try client.replaceCapability("expired", expiresAtMS: 10_999))
        XCTAssertThrowsError(try client.configure(
            managementEndpoint: "https://operator:secret@management.example/custom/v7"
        ))
        client.dispose()
        client.dispose()
        XCTAssertThrowsError(try client.replaceCapability("after-disposal"))
    }

    func testNativeAuthoringMetadataAndTransportAdversariesFailClosed() async throws {
        AuthoringURLProtocol.reset()
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [AuthoringURLProtocol.self]
        let client = NativeAuthoringClient(configuration: configuration, clock: { 1_000 })
        try client.configure(managementEndpoint: "https://management.example/client/v1/management")
        _ = try client.replaceCapability("operator_adversarial", expiresAtMS: 10_000)

        func rejected(_ analysis: [String: Any], identity: String = "authoring:analyze:\(authoringSourceDigest)") async throws {
            AuthoringURLProtocol.setResponse(try testCanonicalJSON(analysis), identity: identity)
            do {
                _ = try await client.analyze(path: "agent.ortg", source: authoringSource)
                XCTFail("native authoring admitted malformed metadata")
            } catch {}
        }

        var unknown = authoringAnalysisFixture()
        unknown = mutateFirstResolvedConfig(unknown) { $0["invented"] = true }
        try await rejected(unknown)

        var wrongPointer = authoringAnalysisFixture()
        wrongPointer = mutateFirstResolvedProperty(wrongPointer) { $0["pointer"] = "#/properties/other" }
        try await rejected(wrongPointer)

        var unorderedTypes = authoringAnalysisFixture()
        unorderedTypes = mutateFirstResolvedProperty(unorderedTypes) { $0["types"] = ["string", "null"] }
        try await rejected(unorderedTypes)

        var invented = authoringAnalysisFixture()
        invented = mutateFirstResolvedConfig(invented) { $0["schema_status"] = "unresolved" }
        try await rejected(invented)

        try await rejected(
            authoringAnalysisFixture(),
            identity: "authoring:analyze:sha256:\(String(repeating: "0", count: 64))"
        )

        AuthoringURLProtocol.setRawResponse(
            Data(#"{"source_digest":"first","source_digest":"second"}"#.utf8),
            identity: "authoring:analyze:\(authoringSourceDigest)"
        )
        do {
            _ = try await client.analyze(path: "agent.ortg", source: authoringSource)
            XCTFail("native authoring admitted duplicate JSON keys")
        } catch {}

        AuthoringURLProtocol.setOversized(true)
        do {
            _ = try await client.analyze(path: "agent.ortg", source: authoringSource)
            XCTFail("native authoring admitted an oversized response")
        } catch {}
        AuthoringURLProtocol.setOversized(false)
        XCTAssertFalse(client.clearCapability().available)
        do {
            _ = try await client.analyze(path: "agent.ortg", source: authoringSource)
            XCTFail("native authoring ran without an operator capability")
        } catch {}
        client.dispose()
    }

}

private final class FixtureProvider: NativeClientProvider {
    let service: NativeClientService
    let implementation: String
    let permissions: [NativeClientPermission]
    let instance: AnyObject = NSObject()
    private let id: String
    private let requires: [NativeClientService]
    private let failStart: Bool
    private let lifecycle: (String) -> Void

    init(selection: NativeProviderSelection, failStart: Bool = false, lifecycle: @escaping (String) -> Void) {
        id = selection.id
        service = selection.service
        implementation = selection.implementation
        permissions = selection.permissions
        requires = selection.requires
        self.failStart = failStart
        self.lifecycle = lifecycle
    }

    func start(dependencies: NativeProviderDependencies) throws {
        lifecycle("start:\(id)")
        for requirement in requires { _ = try dependencies.service(requirement) }
        for permission in permissions {
            guard let operation = permission.operations.first,
                  dependencies.allows(kind: permission.kind, resource: permission.resource, operation: operation) else {
                throw ReducerFailure("fixture permission was unavailable")
            }
        }
        if failStart { throw ReducerFailure("fixture start failed") }
    }

    func stop() throws { lifecycle("stop:\(id)") }
}

private final class RetryFixtureProvider: NativeClientProvider {
    let service: NativeClientService
    let implementation: String
    let permissions: [NativeClientPermission]
    let instance: AnyObject = NSObject()
    var failNextStart = false
    private let id: String
    private let requires: [NativeClientService]
    private let lifecycle: (String) -> Void

    init(selection: NativeProviderSelection, lifecycle: @escaping (String) -> Void) {
        id = selection.id
        service = selection.service
        implementation = selection.implementation
        permissions = selection.permissions
        requires = selection.requires
        self.lifecycle = lifecycle
    }

    func start(dependencies: NativeProviderDependencies) throws {
        lifecycle("start:\(id)")
        for requirement in requires { _ = try dependencies.service(requirement) }
        if failNextStart {
            failNextStart = false
            throw ReducerFailure("fixture remount failed")
        }
    }

    func stop() throws { lifecycle("stop:\(id)") }
}

private func fixtureManifest() throws -> NativeClientManifest {
    let manifest = NativeClientManifest(providers: [
        NativeProviderSelection(id: "slots", service: .slots, implementation: "macos.swiftui-slots.v1"),
        NativeProviderSelection(
            id: "strict-json", service: .strictJSON,
            implementation: "portable.swift-strict-json.v1"
        ),
        NativeProviderSelection(
            id: "transport", service: .connection, implementation: "macos.urlsession-websocket.v1",
            requires: [.strictJSON],
            permissions: [NativeClientPermission(kind: "network.connect", resource: "realtime-endpoint", operations: ["websocket"])]
        ),
        NativeProviderSelection(
            id: "transport-diagnostics", service: .transportDiagnostics,
            implementation: "macos.websocket-diagnostics.v1", requires: [.connection]
        ),
        NativeProviderSelection(
            id: "reducer", service: .reducer, implementation: "portable.swift-reducer.v1",
            requires: [.strictJSON, .connection]
        ),
        NativeProviderSelection(
            id: "protocol-events", service: .protocolEvents,
            implementation: "portable.swift-protocol-events.v1", requires: [.reducer]
        ),
        NativeProviderSelection(
            id: "inspection-access", service: .inspectionAccess,
            implementation: "portable.swift-inspection-access.v1", requires: [.reducer]
        ),
        NativeProviderSelection(
            id: "session-configuration", service: .sessionConfiguration,
            implementation: "portable.swift-session-configuration.v1", requires: [.reducer]
        ),
        NativeProviderSelection(
            id: "media", service: .media, implementation: "macos.av-media.v2",
            requires: [.connection, .reducer, .protocolEvents],
            permissions: [
                NativeClientPermission(
                    kind: "device.media", resource: "native-audio",
                    operations: ["microphone", "playout"]
                ),
            ]
        ),
        NativeProviderSelection(
            id: "video", service: .video, implementation: "macos.video-protocol.v2",
            requires: [.connection, .reducer, .sessionConfiguration],
            permissions: [
                NativeClientPermission(
                    kind: "device.media", resource: "native-video",
                    operations: ["camera", "screen"]
                ),
                NativeClientPermission(
                    kind: "device.media", resource: "native-browser",
                    operations: ["capture"]
                ),
            ]
        ),
        NativeProviderSelection(
            id: "effects", service: .effects, implementation: "macos.host-effects.v1",
            requires: [.strictJSON, .reducer, .protocolEvents, .sessionConfiguration],
            permissions: [NativeClientPermission(
                kind: "network.connect", resource: "host-effects", operations: ["websocket"]
            )]
        ),
        NativeProviderSelection(
            id: "artifacts", service: .artifacts,
            implementation: "macos.host-resource-references.v1", requires: [.effects],
            permissions: [NativeClientPermission(
                kind: "network.connect", resource: "host-resources", operations: ["http"]
            )]
        ),
        NativeProviderSelection(
            id: "inspection", service: .inspection, implementation: "macos.inspection.v1",
            requires: [.inspectionAccess],
            permissions: [NativeClientPermission(
                kind: "network.connect", resource: "management-endpoint", operations: ["http"]
            )]
        ),
        NativeProviderSelection(
            id: "view", service: .view, implementation: "macos.swiftui-view.v1",
            requires: [
                .slots, .transportDiagnostics, .reducer, .media, .video,
                .effects, .artifacts, .inspection,
            ]
        ),
    ])
    try manifest.validate()
    return manifest
}

private func fixtureEndpointDirectory() throws -> NativeEndpointDirectory {
    try NativeEndpointDirectory.freeze([
        NativeEndpoint(
            name: .realtimeWebSocket,
            protocolName: NativeEndpoint.realtimeWebSocketProtocol,
            url: "wss://realtime.example/client/v1/realtime"
        ),
        NativeEndpoint(
            name: .management, protocolName: NativeEndpoint.managementProtocol,
            url: "https://management.example/custom/v7"
        ),
        NativeEndpoint(
            name: .effects, protocolName: NativeEndpoint.effectsProtocol,
            url: "wss://effects.example/client/v1/effects"
        ),
        NativeEndpoint(
            name: .artifacts, protocolName: NativeEndpoint.artifactsProtocol,
            url: "https://artifacts.example/client/v1/artifacts"
        ),
        NativeEndpoint(
            name: .downloads, protocolName: NativeEndpoint.downloadsProtocol,
            url: "https://downloads.example/client/v1/downloads"
        ),
    ])
}

private final class TestClock: @unchecked Sendable {
    var value: Int64
    init(_ value: Int64) { self.value = value }
}

private func inspectionEvent(
    token: String, expiresAtMS: Int64, path: String = "/openrealtime/v1/sessions/sess:test-1/live"
) -> [String: Any] {
    [
        "type": "session.updated",
        "session": [
            "id": "sess:test-1",
            "openrealtime": [
                "debug": [
                    "enabled": true,
                    "inspection": [
                        "session_id": "sess:test-1",
                        "path": path,
                        "token": token,
                        "expires_at_ms": expiresAtMS,
                    ],
                ],
            ],
        ],
    ]
}

private let authoringSource = "graph agent {\n}\n"
private let authoringSourceDigest = "sha256:1a5b83dbd051bb38c8fb4eef0abd26236884eb84f6839aedd2ce1b7db71c7afb"
private let authoringElementDigest = "sha256:" + String(repeating: "a", count: 64)
private let authoringSchemaDigest = "sha256:" + String(repeating: "b", count: 64)

private func authoringAnalysisFixture() -> [String: Any] {
    [
        "source_digest": authoringSourceDigest,
        "parsed": true,
        "recovered": false,
        "canonical": true,
        "diagnostics": ["items": [], "total": 0],
        "catalog": [
            "elements": [
                [
                    "identity": [
                        "name": "test.Empty", "revision": 1,
                        "digest": "sha256:" + String(repeating: "0", count: 64),
                    ],
                    "config": [
                        "artifact": "openrealtime.ai/config/v1alpha1",
                        "resolved": true,
                        "inline_topology_values": false,
                        "empty_object_only": true,
                        "schema_status": "empty-object-only",
                        "properties_complete": true,
                        "properties": [],
                    ],
                ],
                [
                    "identity": [
                        "name": "test.Source", "revision": 2,
                        "digest": authoringElementDigest,
                    ],
                    "config": [
                        "artifact": "openrealtime.ai/config/v1alpha1",
                        "resolved": true,
                        "schema_reference": "schema://test/source-config/v1",
                        "inline_topology_values": false,
                        "empty_object_only": false,
                        "schema_status": "resolved",
                        "schema_id": "https://schemas.example.test/source-config-v1.json",
                        "schema_digest": authoringSchemaDigest,
                        "properties_complete": true,
                        "additional_properties": false,
                        "properties": [
                            [
                                "name": "model",
                                "pointer": "#/properties/model",
                                "required": true,
                                "types": ["null", "string"],
                                "title": "<b>Model</b>",
                                "description": "first line\nsecond line",
                                "format": "model-name",
                                "default": NSNull(),
                                "enum": [NSNull(), "large"],
                                "schema": [
                                    "type": ["null", "string"],
                                    "title": "<b>Model</b>",
                                    "description": "first line\nsecond line",
                                    "format": "model-name",
                                    "default": NSNull(),
                                    "enum": [NSNull(), "large"],
                                ],
                            ],
                            [
                                "name": "temperature",
                                "pointer": "#/properties/temperature",
                                "types": ["number"],
                                "schema": ["type": "number"],
                            ],
                        ],
                    ],
                ],
            ],
            "total": 2,
        ],
        "formatting": [
            "path": "agent.ortg", "source_digest": authoringSourceDigest, "edits": [],
        ],
    ]
}

private func mutateFirstResolvedConfig(
    _ source: [String: Any], _ mutation: (inout [String: Any]) -> Void
) -> [String: Any] {
    var result = source
    var catalog = result["catalog"] as! [String: Any]
    var elements = catalog["elements"] as! [[String: Any]]
    var row = elements[1]
    var config = row["config"] as! [String: Any]
    mutation(&config)
    row["config"] = config
    elements[1] = row
    catalog["elements"] = elements
    result["catalog"] = catalog
    return result
}

private func mutateFirstResolvedProperty(
    _ source: [String: Any], _ mutation: (inout [String: Any]) -> Void
) -> [String: Any] {
    mutateFirstResolvedConfig(source) { config in
        var properties = config["properties"] as! [[String: Any]]
        mutation(&properties[0])
        config["properties"] = properties
    }
}

private final class InspectionURLProtocol: URLProtocol {
    private static let state = InspectionURLProtocolState()

    static func reset() {
        state.reset()
    }

    static func requests() -> [URLRequest] {
        state.requests()
    }

    static func setOversized(_ value: Bool) {
        state.setOversized(value)
    }

    static func setOnRequest(_ value: (@Sendable () -> Void)?) {
        state.setOnRequest(value)
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let (tooLarge, onRequest) = Self.state.record(request)
        onRequest?()
        let headers = tooLarge
            ? ["Content-Type": "application/json", "Content-Length": String(SessionInspectionClient.maximumResponseBytes + 1)]
            : ["Content-Type": "application/json", "Cache-Control": "no-store"]
        let response = HTTPURLResponse(
            url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: headers
        )!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        if !tooLarge {
            client?.urlProtocol(self, didLoad: Data(#"{"graph_id":"compat_graph","nested":{"value":"original"}}"#.utf8))
        }
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

private final class AuthoringURLProtocol: URLProtocol {
    private static let state = AuthoringURLProtocolState()

    static func reset() { state.reset() }
    static func requests() -> [URLRequest] { state.requests() }
    static func setResponse(_ value: Data, identity: String) {
        state.setResponse(value, identity: identity)
    }
    static func setRawResponse(_ value: Data, identity: String) {
        state.setResponse(value, identity: identity)
    }
    static func setOversized(_ value: Bool) { state.setOversized(value) }
    static func setOnRequest(_ value: (@Sendable () -> Void)?) { state.setOnRequest(value) }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let snapshot = Self.state.record(request)
        snapshot.onRequest?()
        var headers = ["Content-Type": "application/json", "Cache-Control": "no-store"]
        headers[NativeAuthoringClient.identityHeader] = snapshot.identity
        if snapshot.oversized {
            headers["Content-Length"] = String(NativeAuthoringClient.maximumResponseBytes + 1)
        }
        let response = HTTPURLResponse(
            url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: headers
        )!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        if !snapshot.oversized { client?.urlProtocol(self, didLoad: snapshot.response) }
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

private final class AuthoringURLProtocolState: @unchecked Sendable {
    struct Snapshot {
        let response: Data
        let identity: String
        let oversized: Bool
        let onRequest: (@Sendable () -> Void)?
    }

    private let lock = NSLock()
    private var captured: [URLRequest] = []
    private var response = Data("{}".utf8)
    private var identity = ""
    private var oversized = false
    private var onRequest: (@Sendable () -> Void)?

    func reset() {
        lock.lock()
        captured.removeAll()
        response = Data("{}".utf8)
        identity = ""
        oversized = false
        onRequest = nil
        lock.unlock()
    }

    func requests() -> [URLRequest] {
        lock.lock()
        let result = captured
        lock.unlock()
        return result
    }

    func setResponse(_ value: Data, identity: String) {
        lock.lock()
        response = value
        self.identity = identity
        lock.unlock()
    }

    func setOversized(_ value: Bool) {
        lock.lock()
        oversized = value
        lock.unlock()
    }

    func setOnRequest(_ value: (@Sendable () -> Void)?) {
        lock.lock()
        onRequest = value
        lock.unlock()
    }

    func record(_ request: URLRequest) -> Snapshot {
        lock.lock()
        captured.append(request)
        let result = Snapshot(
            response: response, identity: identity,
            oversized: oversized, onRequest: onRequest
        )
        lock.unlock()
        return result
    }
}

private final class InspectionURLProtocolState: @unchecked Sendable {
    private let lock = NSLock()
    private var captured: [URLRequest] = []
    private var oversized = false
    private var onRequest: (@Sendable () -> Void)?

    func reset() {
        lock.lock()
        captured.removeAll()
        oversized = false
        onRequest = nil
        lock.unlock()
    }

    func requests() -> [URLRequest] {
        lock.lock()
        let result = captured
        lock.unlock()
        return result
    }

    func setOversized(_ value: Bool) {
        lock.lock()
        oversized = value
        lock.unlock()
    }

    func setOnRequest(_ value: (@Sendable () -> Void)?) {
        lock.lock()
        onRequest = value
        lock.unlock()
    }

    func record(_ request: URLRequest) -> (Bool, (@Sendable () -> Void)?) {
        lock.lock()
        captured.append(request)
        let result = (oversized, onRequest)
        lock.unlock()
        return result
    }
}

private func testCanonicalJSON(_ value: Any) throws -> Data {
    try JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
}
