import SwiftUI

@main
struct OpenRealtimeMacApp: App {
    @StateObject private var model = DeveloperModel()

    var body: some Scene {
        WindowGroup("OpenRealtime Developer") {
            ContentView(model: model)
                .frame(minWidth: 1160, minHeight: 760)
        }
        .windowResizability(.contentMinSize)
        .commands {
            CommandGroup(after: .appInfo) {
                Button("Connect") { model.connect() }
                    .keyboardShortcut("k", modifiers: [.command])
                    .disabled(model.connectionState != .disconnected && model.connectionState != .failed)
                Button("Disconnect") { model.disconnect() }
                    .disabled(model.connectionState == .disconnected)
            }
        }
    }
}
