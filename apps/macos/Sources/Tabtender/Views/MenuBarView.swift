import AppKit
import SwiftUI
import TabtenderKit

/// The menu bar popover: service state, endpoints, and active sessions.
struct MenuBarView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            header
            if let status = model.status, !status.pendingRestart.isEmpty {
                Banner(
                    message: "Restart the service to apply: \(status.pendingRestart.joined(separator: ", "))",
                    systemImage: "arrow.triangle.2.circlepath",
                    tint: .orange
                ) {
                    RestartServiceButton()
                }
            }
            ActionErrorBanner(kind: .service)
            ActionErrorBanner(kind: .session)
            // Shown where a sleeping Mac costs the most: when other machines
            // are meant to reach the service.
            if !model.sleepRisks.isEmpty, model.config?.effective.servesOtherMachines == true {
                Banner(
                    message: "This Mac's settings let it go offline while idle.",
                    systemImage: "moon.zzz.fill",
                    tint: .orange
                ) {
                    SettingsButton(title: "Review…")
                }
            }
            if let explanation = model.serviceState.explanation {
                Text(explanation)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let status = model.status {
                Divider()
                overview(status)
                Divider()
                sessionList(status)
            }
            Divider()
            footer
        }
        .padding(14)
        .frame(width: 360)
    }

    private var header: some View {
        HStack(spacing: 8) {
            StatusDot(color: model.serviceState.color)
            Text("Tabtender")
                .font(.headline)
            Text(model.serviceState.displayName)
                .foregroundStyle(.secondary)
            Spacer()
            ServiceActionButton()
        }
    }

    private func overview(_ status: DaemonStatus) -> some View {
        Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 6) {
            GridRow {
                Text("Health")
                    .foregroundStyle(.secondary)
                healthValue(status)
            }
            GridRow {
                Text("Local API")
                    .foregroundStyle(.secondary)
                URLValue(url: status.localUrl)
            }
            GridRow {
                Text("Public URL")
                    .foregroundStyle(.secondary)
                URLValue(url: status.publicUrl)
            }
            GridRow {
                Text("Tunnel")
                    .foregroundStyle(.secondary)
                HStack(spacing: 6) {
                    StatusDot(color: status.tunnel.state.color)
                    Text(status.tunnel.state.displayName)
                }
                .help(status.tunnel.error)
            }
        }
    }

    @ViewBuilder
    private func healthValue(_ status: DaemonStatus) -> some View {
        if model.health?.isHealthy ?? status.browser.available {
            Text("OK")
        } else {
            Label(status.browser.error ?? "Degraded", systemImage: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
                .lineLimit(2)
        }
    }

    private func sessionList(_ status: DaemonStatus) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("Sessions")
                    .font(.subheadline.weight(.semibold))
                Spacer()
                Text("\(status.sessions.active) of \(status.sessions.max) in use")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            if model.sessions.isEmpty {
                Text("No active sessions")
                    .foregroundStyle(.secondary)
            } else {
                ScrollView {
                    VStack(spacing: 8) {
                        ForEach(model.sessions) { session in
                            SessionRow(session: session)
                        }
                    }
                }
                .frame(maxHeight: 220)
            }
        }
    }

    private var footer: some View {
        HStack {
            SettingsButton()
            Spacer()
            Button("Quit") { NSApp.terminate(nil) }
        }
    }
}

/// A URL with a copy button, or a dash when there is none.
private struct URLValue: View {
    let url: String

    var body: some View {
        if url.isEmpty {
            Text("—")
                .foregroundStyle(.secondary)
        } else {
            HStack(spacing: 4) {
                Text(url)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .textSelection(.enabled)
                CopyButton(value: url, label: "Copy URL")
            }
        }
    }
}

/// Opens the Settings window and brings it to the front. Agent apps are not
/// activated automatically, so the window would otherwise open in the
/// background.
private struct SettingsButton: View {
    var title = "Settings…"

    var body: some View {
        if #available(macOS 14, *) {
            ModernSettingsButton(title: title)
        } else {
            Button(title) {
                NSApp.activate(ignoringOtherApps: true)
                NSApp.sendAction(Selector(("showSettingsWindow:")), to: nil, from: nil)
            }
        }
    }
}

@available(macOS 14, *)
private struct ModernSettingsButton: View {
    @Environment(\.openSettings) private var openSettings
    let title: String

    var body: some View {
        Button(title) {
            NSApp.activate(ignoringOtherApps: true)
            openSettings()
        }
    }
}
