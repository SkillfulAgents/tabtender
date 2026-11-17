import AppKit
import SwiftUI

/// The menu bar app: a status popover plus a settings window. All state
/// lives in the daemon; quitting the app leaves the service running.
struct TabtenderApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        MenuBarExtra {
            MenuBarView()
                .environmentObject(delegate.model)
        } label: {
            MenuBarIcon(model: delegate.model)
        }
        .menuBarExtraStyle(.window)

        Settings {
            SettingsView()
                .environmentObject(delegate.model)
        }
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let model = AppModel()

    func applicationDidFinishLaunching(_ notification: Notification) {
        // The packaged app sets LSUIElement; this covers unbundled runs.
        NSApp.setActivationPolicy(.accessory)
        model.activate()
    }
}

private struct MenuBarIcon: View {
    @ObservedObject var model: AppModel

    var body: some View {
        Image(systemName: model.reachability == .running ? "rectangle.stack.fill" : "rectangle.stack")
            .accessibilityLabel("Tabtender")
    }
}
