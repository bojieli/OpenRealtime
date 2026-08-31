import Foundation
import SwiftUI
import OpenRealtimeClientCore

/// Owns the one live client and the deployment it is bound to.
///
/// Deployment wiring stays an immutable, fingerprinted value: nothing mutates
/// a directory in place. Choosing a different deployment disposes the whole
/// provider graph and builds a new one against a newly frozen directory, so a
/// mounted provider is only ever reachable at the address it was constructed
/// with.
@MainActor
final class NativeClientHost: ObservableObject {
    /// A directory supplied by the environment is deployment policy, not a
    /// preference. Managed and CI launches keep the exact wiring they were
    /// given and the editor is read-only.
    let pinnedByEnvironment: Bool
    let distribution: NativeClientDistribution

    @Published private(set) var model: DeveloperModel?
    @Published private(set) var launchFailure: String
    @Published private(set) var applied: NativeDeploymentSelection?
    @Published private(set) var deploymentDiagnostic = ""
    @Published var draft: NativeDeploymentSelection

    private let pinnedDirectoryData: Data?

    /// A composition that was refused before any client could be built. The
    /// window still opens and states why.
    init(refusal: String) {
        distribution = .observerDeveloper
        pinnedDirectoryData = nil
        pinnedByEnvironment = true
        draft = .presentationHostDefault
        applied = nil
        model = nil
        launchFailure = refusal
    }

    init(distribution: NativeClientDistribution, pinnedDirectoryData: Data?) {
        self.distribution = distribution
        self.pinnedDirectoryData = pinnedDirectoryData
        pinnedByEnvironment = pinnedDirectoryData != nil
        let remembered = pinnedDirectoryData == nil ? NativeDeploymentStore.load() : nil
        draft = remembered ?? .presentationHostDefault
        applied = nil
        launchFailure = ""
        model = nil
        if let remembered {
            // A remembered deployment must not be able to wedge the app: a
            // directory that no longer freezes falls back to the bundled one
            // and says why.
            if build(remembered, remember: false) { return }
        }
        buildBundled()
    }

    /// Rebuilds the client against the drafted deployment.
    func applyDraft() {
        guard !pinnedByEnvironment else { return }
        _ = build(draft, remember: true)
    }

    /// Returns to the directory compiled into the app.
    func resetToBundled() {
        guard !pinnedByEnvironment else { return }
        NativeDeploymentStore.clear()
        draft = .presentationHostDefault
        buildBundled()
    }

    /// The exact URLs the drafted deployment would produce, for display before
    /// anything connects.
    func preview() -> [(String, String)] {
        guard let directory = try? NativeDeploymentDirectory.freeze(
            draft, distribution: distribution
        ) else { return [] }
        return directory.endpoints.map { ($0.name.rawValue, $0.url) }
    }

    private func buildBundled() {
        dispose()
        do {
            let developer = try DeveloperModel(
                distribution: distribution, endpointDirectoryData: pinnedDirectoryData
            )
            model = developer
            applied = nil
            launchFailure = ""
        } catch {
            model = nil
            applied = nil
            launchFailure = error.localizedDescription
        }
    }

    @discardableResult
    private func build(_ selection: NativeDeploymentSelection, remember: Bool) -> Bool {
        let payload: Data
        do {
            let directory = try NativeDeploymentDirectory.freeze(
                selection, distribution: distribution
            )
            payload = try JSONEncoder().encode(directory)
        } catch {
            deploymentDiagnostic = String(error.localizedDescription.prefix(1_024))
            return false
        }
        dispose()
        do {
            let developer = try DeveloperModel(
                distribution: distribution, endpointDirectoryData: payload
            )
            model = developer
            applied = selection
            launchFailure = ""
            deploymentDiagnostic = ""
            if remember { NativeDeploymentStore.save(selection) }
            return true
        } catch {
            model = nil
            applied = nil
            launchFailure = error.localizedDescription
            deploymentDiagnostic = String(error.localizedDescription.prefix(1_024))
            return false
        }
    }

    private func dispose() {
        model?.shutdown()
        model = nil
    }
}
