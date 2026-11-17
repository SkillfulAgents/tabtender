import AppKit
import SwiftUI
import TabtenderKit

/// The one action that moves the service forward from its current state:
/// Start, Stop, Register, Restart, or a shortcut to approve it in System
/// Settings.
struct ServiceActionButton: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        if model.isChangingService {
            ProgressView()
                .controlSize(.small)
        } else {
            switch model.serviceState {
            case .running:
                Button("Stop") {
                    if SessionLossConfirmation.confirm("Stop", model: model) {
                        Task { await model.stopService() }
                    }
                }
            case .unresponsive:
                RestartServiceButton(title: "Restart…")
            case .stopped:
                Button("Start") { Task { await model.startService() } }
            case .notRegistered:
                Button("Register") { Task { await model.registerService() } }
            case .requiresApproval:
                Button("Open Login Items…") { LaunchAgentRegistration.openLoginItemsSettings() }
            case .needsRelocation, .unavailable:
                EmptyView()
            }
        }
    }
}

/// Restarts the service, asking first when that would end sessions.
struct RestartServiceButton: View {
    @EnvironmentObject private var model: AppModel
    var title = "Restart"

    var body: some View {
        Button(title) {
            if SessionLossConfirmation.confirm("Restart", model: model) {
                Task { await model.restartService() }
            }
        }
        .disabled(model.isChangingService)
    }
}

/// Stopping the daemon closes every browser it runs, so actions that do are
/// confirmed while sessions are active.
@MainActor
enum SessionLossConfirmation {
    /// Returns whether to go ahead with `action`, a verb such as "Stop".
    static func confirm(_ action: String, model: AppModel) -> Bool {
        let warning: String
        if case .unresponsive = model.reachability {
            warning = "The service is not responding, so any browser sessions it is running will be closed."
        } else if model.sessions.count == 1 {
            warning = "This closes 1 active browser session."
        } else if model.sessions.count > 1 {
            warning = "This closes \(model.sessions.count) active browser sessions."
        } else {
            return true
        }
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = "\(action) the Tabtender service?"
        alert.informativeText = warning
        alert.addButton(withTitle: action)
        alert.addButton(withTitle: "Cancel")
        // Agent apps are not activated automatically; without this the alert
        // opens behind other windows.
        NSApp.activate(ignoringOtherApps: true)
        return alert.runModal() == .alertFirstButtonReturn
    }
}
