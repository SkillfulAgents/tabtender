import SwiftUI
import TabtenderKit

/// Session lifetime and concurrency limits.
struct SessionsSettingsView: View {
    var body: some View {
        ConfigForm { $config, snapshot in
            Section {
                SettingRow(
                    title: "Default session lifetime",
                    key: "sessions.defaultTTL",
                    snapshot: snapshot,
                    hint: Self.describe(seconds: config.sessions.defaultTTL)
                ) { label in
                    IntegerField(value: $config.sessions.defaultTTL) { label }
                }
                SettingRow(
                    title: "Maximum session lifetime",
                    key: "sessions.maxTTL",
                    snapshot: snapshot,
                    hint: Self.describe(seconds: config.sessions.maxTTL)
                ) { label in
                    IntegerField(value: $config.sessions.maxTTL) { label }
                }
            } footer: {
                SectionFooter("Lifetimes are in seconds. A session is terminated when its lifetime runs out.")
            }
            Section {
                SettingRow(title: "Maximum concurrent sessions", key: "sessions.maxConcurrent", snapshot: snapshot) { label in
                    IntegerField(value: $config.sessions.maxConcurrent) { label }
                }
            }
        }
    }

    /// Spells a number of seconds out, e.g. "1 hour, 30 minutes".
    private static func describe(seconds: Int) -> String? {
        guard seconds > 0 else { return nil }
        return durationFormatter.string(from: TimeInterval(seconds))
    }

    private static let durationFormatter: DateComponentsFormatter = {
        let formatter = DateComponentsFormatter()
        formatter.allowedUnits = [.day, .hour, .minute, .second]
        formatter.unitsStyle = .full
        return formatter
    }()
}
