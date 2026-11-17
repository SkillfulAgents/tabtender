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
                            Button("Unregister") {
                                if SessionLossConfirmation.confirm("Unregister", model: model) {
                                    Task { await model.unregisterService() }
                                }
                            }
                                .disabled(model.isChangingService)
                        }
                    }
                } label: {
                    SettingLabel(
                        title: "Background service",
                        caption: model.serviceState.explanation ?? model.serviceState.displayName
                    )
                }
                ActionErrorBanner(kind: .service)
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
                    IntegerField(value: $config.server.port) { label }
                }
            }
        }
    }
}
