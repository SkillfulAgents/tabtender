import SwiftUI
import TabtenderKit

/// Shows the API key that clients authenticate with and lets it be replaced.
struct APIKeySettingsView: View {
    @EnvironmentObject private var model: AppModel

    @State private var info: APIKeyInfo?
    @State private var isRevealed = false
    @State private var isConfirmingRegeneration = false
    @State private var error: String?

    var body: some View {
        Form {
            if let info {
                Section {
                    LabeledContent("API key") {
                        Text(isRevealed ? info.apiKey : String(repeating: "•", count: 24))
                            .font(.system(.body, design: .monospaced))
                            .textSelection(.enabled)
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                    HStack {
                        Button(isRevealed ? "Hide" : "Reveal") { isRevealed.toggle() }
                        CopyButton(value: info.apiKey, label: "Copy API key")
                        Spacer()
                        Button("Regenerate…") { isConfirmingRegeneration = true }
                            .disabled(!info.rotatable)
                    }
                } footer: {
                    SectionFooter(info.rotatable
                        ? "Clients send the key as \"Authorization: Bearer <key>\"."
                        : "The API key is set by the TABTENDER_API_KEY environment variable and cannot be regenerated here.")
                }
            } else {
                Section {
                    Text(model.status == nil ? "Start the service to see its API key." : "Loading…")
                        .foregroundStyle(.secondary)
                }
            }
            if let error {
                Banner.error(error)
            }
        }
        .formStyle(.grouped)
        .confirmationDialog("Regenerate the API key?", isPresented: $isConfirmingRegeneration) {
            Button("Regenerate", role: .destructive) {
                Task { await load { try await model.client.rotateAPIKey() } }
            }
        } message: {
            Text("The current key stops working immediately. Every client must be updated with the new key.")
        }
        .task(id: model.status?.pid) {
            await load { try await model.client.apiKey() }
        }
    }

    private func load(_ fetch: () async throws -> APIKeyInfo) async {
        guard model.status != nil else {
            info = nil
            return
        }
        do {
            info = try await fetch()
            error = nil
        } catch let failure {
            error = failure.localizedDescription
        }
    }
}
