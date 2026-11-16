import SwiftUI
import TabtenderKit

/// Service registration, OS integration, and the listening address.
struct GeneralSettingsView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        ConfigForm {
            Section("Service") {
                LabeledContent {
                    HStack(spacing: 8) {
                        ServiceActionButton()
                        if model.registration == .enabled {
                            Button("Unregister") { Task { await model.unregisterService() } }
                                .disabled(model.isChangingService)
                        }
                    }
                } label: {
                    SettingLabel(title: "Background service", caption: serviceCaption)
                }
                if let error = model.actionError {
                    Banner.error(error)
                }
            }
        } content: { $config, snapshot in
            Section {
                Toggle("Start at login", isOn: $config.app.startOnLogin)
                Toggle("Prevent sleep while sessions are active", isOn: $config.app.preventSleep)
            }
            Section("Server") {
                SettingRow(title: "Host", key: "server.host", snapshot: snapshot) { label in
                    TextField(text: $config.server.host, prompt: Text("127.0.0.1")) { label }
                }
                SettingRow(title: "Port", key: "server.port", snapshot: snapshot) { label in
                    TextField(value: $config.server.port, format: .number.grouping(.never)) { label }
                }
            }
        }
    }

    private var serviceCaption: String {
        switch model.serviceState {
        case .running, .stopped, .notRegistered:
            return model.serviceState.displayName
        case .requiresApproval:
            return "Allow Tabtender in System Settings > General > Login Items"
        case .unavailable:
            return "Run Tabtender from its app bundle to install the service"
        }
    }
}
