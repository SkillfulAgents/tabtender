import SwiftUI
import TabtenderKit

/// One active session with its vital signs and a Terminate button.
struct SessionRow: View {
    @EnvironmentObject private var model: AppModel
    let session: Session

    @State private var isTerminating = false

    var body: some View {
        HStack(alignment: .center, spacing: 8) {
            VStack(alignment: .leading, spacing: 2) {
                Text(session.id)
                    .font(.system(.callout, design: .monospaced))
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .textSelection(.enabled)
                TimelineView(.periodic(from: .now, by: 1)) { timeline in
                    Text(details(at: timeline.date))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
            }
            Spacer(minLength: 8)
            if isTerminating {
                ProgressView()
                    .controlSize(.small)
            } else {
                Button("Terminate") {
                    isTerminating = true
                    Task {
                        await model.terminate(session)
                        isTerminating = false
                    }
                }
                .controlSize(.small)
            }
        }
    }

    private func details(at date: Date) -> String {
        let clients = session.connectedClients == 1 ? "1 client" : "\(session.connectedClients) clients"
        return [
            session.profile ?? "ephemeral",
            session.headless ? "headless" : "headed",
            Self.remaining(from: date, to: session.expiresAt),
            clients,
        ].joined(separator: " · ")
    }

    private static func remaining(from date: Date, to expiry: Date) -> String {
        let seconds = expiry.timeIntervalSince(date)
        guard seconds > 0, let text = remainingFormatter.string(from: seconds) else {
            return "expiring"
        }
        return "\(text) left"
    }

    private static let remainingFormatter: DateComponentsFormatter = {
        let formatter = DateComponentsFormatter()
        formatter.allowedUnits = [.hour, .minute, .second]
        formatter.unitsStyle = .abbreviated
        formatter.maximumUnitCount = 2
        return formatter
    }()
}
