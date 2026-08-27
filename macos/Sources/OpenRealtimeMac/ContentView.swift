import SwiftUI
import AppKit
import WebKit

struct ContentView: View {
    @ObservedObject var model: DeveloperModel

    var body: some View {
        NavigationSplitView {
            configuration
                .navigationSplitViewColumnWidth(min: 300, ideal: 340, max: 430)
        } detail: {
            VStack(spacing: 0) {
                sessionBar
                Divider()
                TabView {
                    liveSession.tabItem { Label("Session", systemImage: "waveform") }
                    channelInspector.tabItem { Label("Channels", systemImage: "rectangle.3.group") }
                    artifactInspector.tabItem { Label("Artifacts", systemImage: "doc.richtext") }
                    timelineInspector.tabItem { Label("Timeline", systemImage: "clock.arrow.2.circlepath") }
                    protocolInspector.tabItem { Label("Protocol", systemImage: "chevron.left.forwardslash.chevron.right") }
                }
                .padding(10)
            }
        }
        .sheet(item: $model.pendingConfirmation) { request in
            ConfirmationView(request: request) { approved in
                model.decideConfirmation(approved)
            }
        }
    }

    private var configuration: some View {
        Form {
            Section("Connection") {
                TextField("WebSocket endpoint", text: $model.endpoint)
                    .textFieldStyle(.roundedBorder)
                SecureField("Bearer token (optional)", text: $model.token)
                    .textFieldStyle(.roundedBorder)
                HStack {
                    TextField("Workspace root", text: $model.workspaceRoot)
                        .textFieldStyle(.roundedBorder)
                    Button("Choose…") { model.chooseWorkspace() }
                }
            }

            Section("Initial system prompt") {
                TextEditor(text: $model.systemPrompt)
                    .font(.body)
                    .frame(minHeight: 150)
                Text("Sent in the first session.update and editable while connected.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Button("Apply prompt now") { model.applySystemPrompt() }
                    .disabled(model.connectionState != .connected)
            }

            Section("Bounded computer target") {
                Picker("Mode", selection: $model.computerMode) {
                    ForEach(ComputerMode.allCases) { mode in Text(mode.rawValue).tag(mode) }
                }
                .pickerStyle(.segmented)
                .disabled(model.connectionState != .disconnected)

                if model.computerMode == .browser {
                    TextField("Chrome CDP URL", text: $model.cdpURL)
                        .textFieldStyle(.roundedBorder)
                    Text("Frames carry browser-use set-of-mark labels. Actions cannot reach the desktop.")
                        .font(.caption).foregroundStyle(.secondary)
                } else {
                    Picker("Display", selection: $model.selectedDisplayID) {
                        ForEach(model.displays) { display in
                            Text(display.label).tag(display.id)
                        }
                    }
                    Text("Pixel actions are transformed only into this display and require Accessibility.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }

            Section("Privacy") {
                permissionRow("Microphone", model.microphonePermission)
                permissionRow("Camera", model.cameraPermission)
                permissionRow("Screen Recording", model.screenPermission)
                permissionRow("Accessibility", model.accessibilityPermission)
            }
        }
        .formStyle(.grouped)
        .navigationTitle("OpenRealtime")
    }

    private func permissionRow(_ name: String, _ value: String) -> some View {
        HStack {
            Text(name)
            Spacer()
            Text(value).foregroundStyle(value == "granted" ? .green : .secondary)
        }
    }

    private var sessionBar: some View {
        HStack(spacing: 12) {
            Circle()
                .fill(model.connectionState == .connected ? .green :
                      model.connectionState == .failed ? .red : .secondary)
                .frame(width: 10, height: 10)
            Text(model.statusText).font(.headline)
            if !model.negotiationText.isEmpty {
                Text(model.negotiationText).font(.caption).foregroundStyle(.secondary)
                    .lineLimit(1)
            }
            Spacer()
            Button("Connect") { model.connect() }
                .buttonStyle(.borderedProminent)
                .disabled(model.connectionState != .disconnected && model.connectionState != .failed)
            Button("Disconnect") { model.disconnect() }
                .disabled(model.connectionState == .disconnected)
        }
        .padding(12)
    }

    private var liveSession: some View {
        VStack(alignment: .leading, spacing: 12) {
            GroupBox("Live media") {
                HStack(spacing: 10) {
                    mediaButton(model.microphoneActive
                                ? (model.microphoneMuted ? "Unmute microphone" : "Mute microphone")
                                : "Start microphone",
                                icon: model.microphoneMuted ? "mic" : (model.microphoneActive ? "mic.slash" : "mic")) {
                        model.toggleMicrophone()
                    }
                    mediaButton(model.screenActive ? "Stop screen" : "Share screen", icon: "rectangle.on.rectangle") {
                        model.toggleScreen()
                    }
                    mediaButton(model.cameraActive ? "Stop camera" : "Start camera", icon: "video") {
                        model.toggleCamera()
                    }
                    mediaButton(model.browserActive ? "Stop browser" : "Share marked browser", icon: "safari") {
                        model.toggleBrowser()
                    }
                    Spacer()
                    Button("End turn") { model.endTurn() }
                }
                .padding(6)
            }

            HStack(alignment: .top, spacing: 12) {
                GroupBox("Conversation") {
                    List(model.conversation) { record in
                        VStack(alignment: .leading, spacing: 4) {
                            HStack {
                                Text(record.title).font(.caption.bold())
                                Spacer()
                                Text(record.timestamp, style: .time).font(.caption2).foregroundStyle(.secondary)
                            }
                            Text(record.body).textSelection(.enabled)
                                .foregroundStyle(record.failed ? .red : .primary)
                        }
                        .padding(.vertical, 3)
                    }
                    .listStyle(.plain)
                }

                GroupBox("Traffic and target") {
                    VStack(alignment: .leading, spacing: 8) {
                        statusRow("Audio input", model.microphoneActive ? "live · PCM16 24 kHz" : "off")
                        statusRow("Audio output", model.audioOutputStatus)
                        statusRow("Screen", model.screenActive ? "live · \(model.frameStatus["screen"] ?? "waiting")" : "off")
                        statusRow("Camera", model.cameraActive ? "live · \(model.frameStatus["camera"] ?? "waiting")" : "off")
                        statusRow("Browser", model.browserActive ? "live · \(model.browserCaption)" : "off")
                        statusRow("Computer", model.computerMode.rawValue)
                        statusRow("Workspace", model.workspaceRoot)
                        Divider()
                        Text("All filesystem paths are resolved after symlinks inside the workspace. Writes, shell commands, and consequential computer actions wait for local confirmation.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    .padding(6)
                    .frame(minWidth: 300)
                }
            }

            HStack {
                TextField("Send a text message", text: $model.composer)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit { model.submitText() }
                Button("Send") { model.submitText() }
                    .keyboardShortcut(.return, modifiers: [.command])
                    .disabled(model.composer.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }
    }

    private func mediaButton(_ title: String, icon: String, action: @escaping () -> Void) -> some View {
        Button(action: action) { Label(title, systemImage: icon) }
            .disabled(model.connectionState != .connected)
    }

    private func statusRow(_ name: String, _ value: String) -> some View {
        HStack(alignment: .firstTextBaseline) {
            Text(name).foregroundStyle(.secondary)
            Spacer()
            Text(value).lineLimit(2).multilineTextAlignment(.trailing)
        }
    }

    private var channelInspector: some View {
        VStack(alignment: .leading) {
            HStack {
                Text("Observation and action channels").font(.headline)
                Spacer()
                Text("\(model.channelRecords.count) events").foregroundStyle(.secondary)
                Button("Clear") { model.channelRecords.removeAll() }
            }
            List(model.channelRecords.reversed()) { record in
                HStack(alignment: .top, spacing: 12) {
                    Text(record.channel).font(.caption.monospaced()).frame(width: 105, alignment: .leading)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(record.title).fontWeight(.semibold)
                        Text(record.body).font(.body).textSelection(.enabled)
                    }
                    Spacer()
                    Text(record.timestamp, format: .dateTime.hour().minute().second().secondFraction(.fractional(3)))
                        .font(.caption2.monospaced()).foregroundStyle(.secondary)
                }
                .foregroundStyle(record.failed ? .red : .primary)
            }
            .listStyle(.inset)
        }
    }

    private var artifactInspector: some View {
        HSplitView {
            VStack(alignment: .leading) {
                Text("HTML artifacts").font(.headline)
                if let artifact = model.selectedArtifact {
                    ArtifactWebView(html: artifact.html) { text in model.submitArtifactText(text) }
                        .id("\(artifact.id)-\(artifact.version)")
                    Text("\(artifact.title) · revision \(artifact.version)")
                        .font(.caption).foregroundStyle(.secondary)
                } else {
                    ContentUnavailableView("No artifact yet", systemImage: "doc.richtext",
                                           description: Text("display_artifact renders here in an isolated web view."))
                }
            }
            .frame(minWidth: 430)

            VStack(alignment: .leading) {
                Text("Generated files").font(.headline)
                if model.downloads.isEmpty {
                    ContentUnavailableView("No generated files", systemImage: "arrow.down.doc",
                                           description: Text("publish_download adds bounded files here."))
                } else {
                    List(model.downloads) { download in
                        VStack(alignment: .leading, spacing: 5) {
                            Text(download.filename).fontWeight(.semibold)
                            Text("\(download.mediaType) · \(download.bytes) bytes · revision \(download.version)")
                                .font(.caption).foregroundStyle(.secondary)
                            HStack {
                                Button("Open") { NSWorkspace.shared.open(download.url) }
                                Button("Show in Finder") { NSWorkspace.shared.activateFileViewerSelecting([download.url]) }
                                Button("Save As…") { model.saveDownload(download) }
                            }
                        }
                    }
                }
            }
            .frame(minWidth: 310)
        }
    }

    private var timelineInspector: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("Server debug timeline").font(.headline)
                TextField("Filter category, name, correlation, or detail", text: $model.timelineFilter)
                    .textFieldStyle(.roundedBorder)
                    .frame(maxWidth: 430)
                Spacer()
                Text(model.latencySummary).font(.caption.monospaced()).foregroundStyle(.secondary)
                Button("Clear") { model.debugRecords.removeAll() }
            }
            Table(model.filteredDebugRecords) {
                TableColumn("Time") { record in
                    Text(record.date, format: .dateTime.hour().minute().second().secondFraction(.fractional(3)))
                        .font(.caption.monospaced())
                }.width(105)
                TableColumn("Category") { record in Text(record.category).font(.caption.monospaced()) }.width(85)
                TableColumn("Event") { record in Text(record.name) }.width(min: 170, ideal: 230)
                TableColumn("Phase") { record in Text(record.phase) }.width(70)
                TableColumn("Duration") { record in
                    Text(record.durationMS.map { String(format: "%.1f ms", $0) } ?? "—")
                        .font(.caption.monospaced())
                }.width(90)
                TableColumn("Correlation") { record in Text(record.correlationID).font(.caption.monospaced()) }.width(150)
                TableColumn("Detail") { record in Text(record.detail).lineLimit(2).textSelection(.enabled) }
            }
        }
    }

    private var protocolInspector: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("Raw protocol log (media elided)").font(.headline)
                Spacer()
                Button("Export…") { model.exportProtocolLog() }
                Button("Clear") { model.protocolRecords.removeAll() }
            }
            List(model.protocolRecords.reversed()) { record in
                HStack(alignment: .top) {
                    Text(record.direction).font(.caption.bold().monospaced())
                        .foregroundStyle(record.direction == "OUT" ? .orange : .blue)
                        .frame(width: 34)
                    Text(record.timestamp, format: .dateTime.hour().minute().second().secondFraction(.fractional(3)))
                        .font(.caption2.monospaced()).foregroundStyle(.secondary).frame(width: 105)
                    Text(record.type).font(.caption.bold().monospaced()).frame(width: 260, alignment: .leading)
                    Text(record.payload).font(.caption.monospaced()).textSelection(.enabled)
                }
            }
            .listStyle(.plain)
        }
    }
}

private struct ConfirmationView: View {
    let request: ConfirmationRequest
    let decide: (Bool) -> Void
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Label("Local confirmation required", systemImage: "exclamationmark.shield")
                .font(.title2.bold())
            Text(request.name).font(.headline.monospaced())
            Text(request.consequence).foregroundStyle(.secondary)
            ScrollView { Text(request.arguments).font(.body.monospaced()).textSelection(.enabled) }
                .frame(minWidth: 560, minHeight: 180)
                .padding(8).background(.quaternary, in: RoundedRectangle(cornerRadius: 7))
            HStack {
                Spacer()
                Button("Decline", role: .cancel) { decide(false); dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button("Allow once") { decide(true); dismiss() }
                    .buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction)
            }
        }
        .padding(24)
        .interactiveDismissDisabled()
    }
}

private struct ArtifactWebView: NSViewRepresentable {
    let html: String
    let interaction: (String) -> Void

    func makeCoordinator() -> Coordinator { Coordinator(interaction: interaction) }

    func makeNSView(context: Context) -> WKWebView {
        let configuration = WKWebViewConfiguration()
        configuration.websiteDataStore = .nonPersistent()
        configuration.userContentController.add(context.coordinator, name: "openrealtime")
        let bridge = WKUserScript(source: """
          window.addEventListener('message', event => {
            if (event.data && typeof event.data.text === 'string')
              window.webkit.messageHandlers.openrealtime.postMessage(event.data.text);
          });
        """, injectionTime: .atDocumentStart, forMainFrameOnly: true)
        configuration.userContentController.addUserScript(bridge)
        let webView = WKWebView(frame: .zero, configuration: configuration)
        webView.navigationDelegate = context.coordinator
        return webView
    }

    func updateNSView(_ webView: WKWebView, context: Context) {
        webView.loadHTMLString(html, baseURL: nil)
    }

    final class Coordinator: NSObject, WKScriptMessageHandler, WKNavigationDelegate {
        let interaction: (String) -> Void
        init(interaction: @escaping (String) -> Void) { self.interaction = interaction }
        func userContentController(_ userContentController: WKUserContentController,
                                   didReceive message: WKScriptMessage) {
            if let text = message.body as? String { interaction(text) }
        }

        func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            let scheme = navigationAction.request.url?.scheme?.lowercased()
            decisionHandler(scheme == nil || scheme == "about" ? .allow : .cancel)
        }
    }
}
