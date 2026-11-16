import SwiftUI

/// The Settings window.
struct SettingsView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        TabView {
            GeneralSettingsView()
                .tabItem { Label("General", systemImage: "gearshape") }
            SessionsSettingsView()
                .tabItem { Label("Sessions", systemImage: "rectangle.stack") }
            BrowserSettingsView()
                .tabItem { Label("Browser", systemImage: "globe") }
            TunnelSettingsView()
                .tabItem { Label("Tunnel", systemImage: "network") }
            APIKeySettingsView()
                .tabItem { Label("API Key", systemImage: "key") }
            LogsView()
                .tabItem { Label("Logs", systemImage: "list.bullet.rectangle") }
        }
        .frame(width: 580, height: 460)
        .task { await model.reloadConfig() }
    }
}
