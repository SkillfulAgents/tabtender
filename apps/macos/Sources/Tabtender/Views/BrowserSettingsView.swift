import AppKit
import SwiftUI
import TabtenderKit

/// Which browser sessions run and how it is launched.
struct BrowserSettingsView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        ConfigForm { $config, snapshot in
            Section {
                SettingRow(title: "Runtime", key: "browser.runtime", snapshot: snapshot) { label in
                    TextField(text: $config.browser.runtime, prompt: Text("chrome")) { label }
                }
            }
            Section {
                SettingRow(title: "Executable", key: "browser.executablePath", snapshot: snapshot) { label in
                    Toggle(isOn: findsAutomatically($config.browser.executablePath)) {
                        SettingLabel(title: "Find the browser automatically")
                    }
                    if config.browser.executablePath != Configuration.autoPath {
                        LabeledContent {
                            HStack {
                                TextField("Executable path", text: $config.browser.executablePath)
                                    .labelsHidden()
                                Button("Choose…") { chooseExecutable(for: $config.browser.executablePath) }
                            }
                        } label: {
                            label
                        }
                    }
                }
                if let browser = model.status?.browser {
                    LabeledContent("In use") {
                        if let executable = browser.executable, browser.available {
                            Text(executable)
                                .foregroundStyle(.secondary)
                                .textSelection(.enabled)
                        } else {
                            Text(browser.error ?? "No browser found")
                                .foregroundStyle(.red)
                        }
                    }
                }
            }
            Section {
                Toggle("Run browsers headless by default", isOn: $config.browser.headless)
            } footer: {
                SectionFooter("A session can override this when it is created.")
            }
        }
    }

    /// Maps the "auto" sentinel onto a toggle; turning it off starts from an
    /// empty path for the user to fill in.
    private func findsAutomatically(_ path: Binding<String>) -> Binding<Bool> {
        Binding(
            get: { path.wrappedValue == Configuration.autoPath },
            set: { path.wrappedValue = $0 ? Configuration.autoPath : "" }
        )
    }

    private func chooseExecutable(for path: Binding<String>) {
        let panel = NSOpenPanel()
        panel.message = "Choose a browser application or executable"
        panel.directoryURL = URL(fileURLWithPath: "/Applications")
        panel.canChooseFiles = true
        panel.canChooseDirectories = false
        panel.allowsMultipleSelection = false
        if panel.runModal() == .OK, let url = panel.url {
            // Picking an application means picking its main executable.
            path.wrappedValue = (Bundle(url: url)?.executableURL ?? url).path
        }
    }
}
