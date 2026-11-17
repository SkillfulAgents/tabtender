import SwiftUI
import TabtenderKit

/// A settings pane that edits the configuration draft.
///
/// `header` rows are always shown; `content` rows need the daemon and are
/// replaced by a hint while it is unreachable. The Save bar at the bottom
/// sends the draft's edits to the daemon and shows its validation errors.
struct ConfigForm<Header: View, Content: View>: View {
    @EnvironmentObject private var model: AppModel
    @ViewBuilder var header: Header
    @ViewBuilder var content: (Binding<Configuration>, ConfigSnapshot) -> Content

    var body: some View {
        VStack(spacing: 0) {
            Form {
                header
                if let snapshot = model.config, model.status != nil {
                    content(draftBinding(fallback: snapshot.file), snapshot)
                } else {
                    Section {
                        Text("Start the service to change its settings.")
                            .foregroundStyle(.secondary)
                    }
                }
            }
            .formStyle(.grouped)
            Divider()
            saveBar
        }
    }

    private func draftBinding(fallback: Configuration) -> Binding<Configuration> {
        Binding(
            get: { model.draft ?? fallback },
            set: { model.draft = $0 }
        )
    }

    private var saveBar: some View {
        HStack(spacing: 8) {
            if let error = model.saveError {
                Label(error, systemImage: "exclamationmark.triangle.fill")
                    .foregroundStyle(.red)
                    .font(.callout)
                    .fixedSize(horizontal: false, vertical: true)
                    .textSelection(.enabled)
            }
            Spacer()
            Button("Revert") { model.discardChanges() }
                .disabled(!model.hasUnsavedChanges || model.isSaving)
            Button("Save") { Task { await model.saveConfig() } }
                .keyboardShortcut(.defaultAction)
                .disabled(!model.hasUnsavedChanges || model.isSaving || model.status == nil)
        }
        .padding(12)
    }
}

extension ConfigForm where Header == EmptyView {
    init(@ViewBuilder content: @escaping (Binding<Configuration>, ConfigSnapshot) -> Content) {
        self.init(header: { EmptyView() }, content: content)
    }
}

/// One configuration setting.
///
/// A setting pinned by an environment variable is shown read-only with its
/// effective value and the variable's name; otherwise `editor` is shown with
/// a label that notes when the setting requires a restart.
struct SettingRow<Editor: View>: View {
    let title: String
    /// Dotted setting name, e.g. "server.port".
    let key: String
    let snapshot: ConfigSnapshot
    var hint: String?
    /// Turns a raw overridden value into what the editor would display.
    var describe: (String) -> String = { $0 }
    @ViewBuilder var editor: (SettingLabel) -> Editor

    var body: some View {
        if let override = snapshot.envOverrides[key] {
            LabeledContent {
                Text(describe(override.value))
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            } label: {
                SettingLabel(title: title, caption: "Set by \(override.variable)")
            }
        } else {
            editor(SettingLabel(title: title, caption: caption))
        }
    }

    private var caption: String? {
        let notes = [hint, snapshot.restartRequiredKeys.contains(key) ? "Requires restart" : nil]
        let text = notes.compactMap { $0 }.joined(separator: " · ")
        return text.isEmpty ? nil : text
    }
}

/// A setting's title with an optional caption underneath.
struct SettingLabel: View {
    let title: String
    var caption: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title)
            if let caption {
                Text(caption)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
    }
}
