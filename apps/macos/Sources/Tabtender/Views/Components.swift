import AppKit
import SwiftUI
import TabtenderKit

/// A small colored dot conveying state at a glance.
struct StatusDot: View {
    let color: Color

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: 8, height: 8)
            .accessibilityHidden(true)
    }
}

/// Copies a string to the pasteboard, confirming briefly with a checkmark.
struct CopyButton: View {
    let value: String
    var label = "Copy"

    @State private var didCopy = false

    var body: some View {
        Button {
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(value, forType: .string)
            didCopy = true
            Task {
                try? await Task.sleep(nanoseconds: 1_200_000_000)
                didCopy = false
            }
        } label: {
            Image(systemName: didCopy ? "checkmark" : "doc.on.doc")
        }
        .buttonStyle(.borderless)
        .help(label)
        .accessibilityLabel(label)
    }
}

/// A tinted message strip with an optional trailing action.
struct Banner<Action: View>: View {
    let message: String
    let systemImage: String
    let tint: Color
    @ViewBuilder var action: Action

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: systemImage)
                .foregroundStyle(tint)
            Text(message)
                .font(.callout)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 8)
            action
        }
        .padding(8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(tint.opacity(0.12), in: RoundedRectangle(cornerRadius: 6))
    }
}

extension Banner where Action == EmptyView {
    init(message: String, systemImage: String, tint: Color) {
        self.init(message: message, systemImage: systemImage, tint: tint) { EmptyView() }
    }

    /// A banner describing a failure.
    static func error(_ message: String) -> Banner {
        Banner(message: message, systemImage: "exclamationmark.triangle.fill", tint: .red)
    }
}

/// Explanatory text under a form section, aligned with the section's rows.
struct SectionFooter: View {
    let text: String

    init(_ text: String) {
        self.text = text
    }

    var body: some View {
        Text(text)
            .frame(maxWidth: .infinity, alignment: .leading)
    }
}

extension TunnelStatus.State {
    var displayName: String {
        switch self {
        case .disabled: return "Off"
        case .starting: return "Starting…"
        case .running: return "Connected"
        case .error: return "Error"
        case .stopped: return "Stopped"
        case .unknown: return "Unknown"
        }
    }

    var color: Color {
        switch self {
        case .running: return .green
        case .starting: return .yellow
        case .error: return .red
        case .disabled, .stopped, .unknown: return .secondary
        }
    }
}

extension AppModel.ServiceState {
    var displayName: String {
        switch self {
        case .running: return "Running"
        case .stopped: return "Stopped"
        case .notRegistered: return "Not registered"
        case .requiresApproval: return "Needs approval"
        case .unavailable: return "Not installed"
        }
    }

    var color: Color {
        switch self {
        case .running: return .green
        case .requiresApproval: return .yellow
        case .stopped, .notRegistered, .unavailable: return .secondary
        }
    }
}
