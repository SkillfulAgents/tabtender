import SwiftUI
import TabtenderKit

/// The one action that moves the service forward from its current state:
/// Start, Stop, Register, or a shortcut to approve it in System Settings.
struct ServiceActionButton: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        if model.isChangingService {
            ProgressView()
                .controlSize(.small)
        } else {
            switch model.serviceState {
            case .running:
                Button("Stop") { Task { await model.stopService() } }
            case .stopped:
                Button("Start") { Task { await model.startService() } }
            case .notRegistered:
                Button("Register") { Task { await model.registerService() } }
            case .requiresApproval:
                Button("Open Login Items…") { LaunchAgentRegistration.openLoginItemsSettings() }
            case .unavailable:
                EmptyView()
            }
        }
    }
}
