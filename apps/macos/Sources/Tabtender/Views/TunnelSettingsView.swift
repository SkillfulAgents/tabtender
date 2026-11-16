import SwiftUI
import TabtenderKit

/// Public access through a tunnel: provider, credentials, and live status.
struct TunnelSettingsView: View {
    @EnvironmentObject private var model: AppModel

    @State private var token = ""
    @State private var isWorking = false

    var body: some View {
        ConfigForm { $config, snapshot in
            Section {
                SettingRow(
                    title: "Provider",
                    key: "tunnel.provider",
                    snapshot: snapshot,
                    describe: { TunnelProvider(rawValue: $0)?.displayName ?? $0 }
                ) { label in
                    Picker(selection: $config.tunnel.provider) {
                        ForEach(TunnelProvider.allCases) { provider in
                            Text(provider.displayName).tag(provider.rawValue)
                        }
                    } label: {
                        label
                    }
                }
            } footer: {
                SectionFooter(Self.explanation(of: snapshot.envOverrides["tunnel.provider"]?.value ?? config.tunnel.provider))
            }

            if (snapshot.envOverrides["tunnel.provider"]?.value ?? config.tunnel.provider) == TunnelProvider.cloudflareNamed.rawValue {
                Section("Cloudflare Named Tunnel") {
                    TextField("Hostname", text: $config.tunnel.cloudflare.hostname, prompt: Text("browser.example.com"))
                    tokenRows
                }
            }

            if let status = model.status {
                statusSection(status)
            }
        }
    }

    // MARK: Token

    @ViewBuilder
    private var tokenRows: some View {
        let source = model.status?.tunnelTokenSource ?? ""
        if source == SecretSource.environment {
            LabeledContent("Tunnel token") {
                Text("Set by TABTENDER_CLOUDFLARE_TUNNEL_TOKEN")
                    .foregroundStyle(.secondary)
            }
        } else {
            LabeledContent {
                HStack {
                    SecureField("Tunnel token", text: $token, prompt: Text("Paste a new token"))
                        .labelsHidden()
                    Button("Save") { submitToken(token) }
                        .disabled(token.isEmpty || isWorking)
                    Button("Remove") { submitToken("") }
                        .disabled(source.isEmpty || isWorking)
                }
            } label: {
                SettingLabel(title: "Tunnel token", caption: source.isEmpty ? "No token" : "Token stored")
            }
        }
    }

    private func submitToken(_ value: String) {
        isWorking = true
        Task {
            await model.setTunnelToken(value)
            token = ""
            isWorking = false
        }
    }

    // MARK: Status

    private func statusSection(_ status: DaemonStatus) -> some View {
        Section("Status") {
            LabeledContent("State") {
                HStack(spacing: 6) {
                    StatusDot(color: status.tunnel.state.color)
                    Text(status.tunnel.state.displayName)
                }
            }
            if !status.publicUrl.isEmpty {
                LabeledContent("Public URL") {
                    HStack(spacing: 4) {
                        Text(status.publicUrl)
                            .textSelection(.enabled)
                        CopyButton(value: status.publicUrl, label: "Copy URL")
                    }
                }
            }
            if !status.tunnel.error.isEmpty {
                Banner.error(status.tunnel.error)
            }
            if let error = model.actionError {
                Banner.error(error)
            }
            if status.tunnel.state != .disabled {
                Button("Restart Tunnel") {
                    isWorking = true
                    Task {
                        await model.restartTunnel()
                        isWorking = false
                    }
                }
                .disabled(isWorking)
            }
        }
    }

    private static func explanation(of provider: String) -> String {
        switch TunnelProvider(rawValue: provider) {
        case .cloudflareQuick:
            return "Gives the service a temporary public trycloudflare.com URL. No Cloudflare account is needed; the URL changes whenever the tunnel restarts."
        case .cloudflareNamed:
            return "Serves a hostname of your own through a tunnel created in the Cloudflare dashboard."
        case .some(.none), nil:
            return "The service is only reachable from this Mac and its network."
        }
    }
}
