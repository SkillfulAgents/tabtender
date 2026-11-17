import AppKit
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
                Picker(selection: $config.app.sleepPrevention) {
                    Text("Never").tag(Configuration.SleepPrevention.never)
                    Text("While sessions are active").tag(Configuration.SleepPrevention.whileSessionsAreActive)
                    Text("Always").tag(Configuration.SleepPrevention.always)
                } label: {
                    SettingLabel(
                        title: "Keep this Mac awake",
                        caption: config.app.sleepPrevention == .always
                            ? "Awake whenever the service runs, so it stays reachable. "
                                + "The display can still sleep; a closed laptop lid still sleeps the Mac."
                            : nil
                    )
                }
                SleepRiskList()
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

/// The macOS settings that will take the service offline, each with a way
/// to the setting that removes it.
private struct SleepRiskList: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        let hasBattery = model.powerSettings?.hasBattery ?? false
        let prevention = model.config?.effective.app.sleepPrevention ?? .never
        ForEach(model.sleepRisks) { risk in
            let pane = risk.settingsPane(
                hasBattery: hasBattery,
                macOSMajorVersion: ProcessInfo.processInfo.operatingSystemVersion.majorVersion
            )
            VStack(alignment: .leading, spacing: 6) {
                Label(risk.summary(prevention: prevention), systemImage: "moon.zzz.fill")
                    .foregroundStyle(.orange)
                Text(risk.remedy(hasBattery: hasBattery))
                    .font(.callout)
                    .foregroundStyle(.secondary)
                if let pane {
                    Button(pane.buttonTitle) {
                        NSWorkspace.shared.open(pane.url)
                    }
                }
            }
            .fixedSize(horizontal: false, vertical: true)
        }
        // The user may have just changed a setting in System Settings.
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
            Task { await model.refreshPowerSettings() }
        }
        .task { await model.refreshPowerSettings() }
    }
}
