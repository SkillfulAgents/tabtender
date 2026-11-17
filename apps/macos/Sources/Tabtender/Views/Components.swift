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
    /// Marks the copy as concealed, which clipboard managers honor by not
    /// recording it in their history.
    var isSecret = false

    /// The nspasteboard.org convention for sensitive pasteboard contents.
    private static let concealedType = NSPasteboard.PasteboardType("org.nspasteboard.ConcealedType")

    @State private var didCopy = false

    var body: some View {
        Button {
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(value, forType: .string)
            if isSecret {
                NSPasteboard.general.setString("", forType: Self.concealedType)
            }
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

/// The most recent failed action of one kind, with a button to dismiss it.
struct ActionErrorBanner: View {
    @EnvironmentObject private var model: AppModel
    let kind: AppModel.ActionKind

    var body: some View {
        if let message = model.actionErrors[kind] {
            Banner(message: message, systemImage: "exclamationmark.triangle.fill", tint: .red) {
                Button {
                    model.dismissError(kind)
                } label: {
                    Image(systemName: "xmark")
                }
                .buttonStyle(.borderless)
                .help("Dismiss")
                .accessibilityLabel("Dismiss")
            }
        }
    }
}

/// A text field for a whole number that updates its value on every
/// keystroke. A formatted `TextField` only commits on Return or when focus
/// moves, which leaves Save disabled right after typing.
struct IntegerField<Label: View>: View {
    @Binding var value: Int
    @ViewBuilder var label: Label

    @State private var text: String

    init(value: Binding<Int>, @ViewBuilder label: () -> Label) {
        _value = value
        self.label = label()
        _text = State(initialValue: String(value.wrappedValue))
    }

    var body: some View {
        TextField(text: $text) { label }
            .onChange(of: text) { text in
                if let number = Int(text.trimmingCharacters(in: .whitespaces)), number != value {
                    value = number
                }
            }
            .onChange(of: value) { value in
                // Follows changes made elsewhere, such as Revert.
                if Int(text.trimmingCharacters(in: .whitespaces)) != value {
                    text = String(value)
                }
            }
            .onSubmit { text = String(value) }
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

extension ServiceState {
    var displayName: String {
        switch self {
        case .running: return "Running"
        case .unresponsive: return "Not responding"
        case .stopped: return "Stopped"
        case .notRegistered: return "Not registered"
        case .requiresApproval: return "Needs approval"
        case .needsRelocation, .unavailable: return "Not installed"
        }
    }

    var color: Color {
        switch self {
        case .running: return .green
        case .requiresApproval: return .yellow
        case .unresponsive: return .red
        case .stopped, .notRegistered, .needsRelocation, .unavailable: return .secondary
        }
    }

    /// What the user has to do, or what is wrong, when the state's name
    /// alone does not say.
    var explanation: String? {
        switch self {
        case .running, .stopped, .notRegistered:
            return nil
        case let .unresponsive(reason):
            return "The service is running but not responding. \(reason)"
        case .requiresApproval:
            return "Allow Tabtender in System Settings > General > Login Items to run the service."
        case .needsRelocation:
            return "Move Tabtender to the Applications folder, then open it again."
        case .unavailable:
            return "Run Tabtender from its app bundle to install the service."
        }
    }
}
