import SwiftUI
import TabtenderKit

/// Recent daemon log entries.
struct LogsView: View {
    @EnvironmentObject private var model: AppModel

    @State private var entries: [LogEntry] = []
    @State private var problemsOnly = false
    @State private var error: String?

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Toggle("Errors and warnings only", isOn: $problemsOnly)
                Spacer()
                Button {
                    Task { await load() }
                } label: {
                    Label("Refresh", systemImage: "arrow.clockwise")
                }
            }
            .padding(12)
            Divider()
            if let error {
                placeholder(error)
            } else if entries.isEmpty {
                placeholder("No log entries")
            } else {
                List(Array(entries.enumerated().reversed()), id: \.offset) { _, entry in
                    LogRow(entry: entry)
                }
                .listStyle(.plain)
            }
        }
        .task(id: problemsOnly) { await load() }
    }

    private func placeholder(_ text: String) -> some View {
        Text(text)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func load() async {
        do {
            entries = try await model.client.logs(minimumLevel: problemsOnly ? .warn : .info)
            error = nil
        } catch let failure {
            entries = []
            error = failure.localizedDescription
        }
    }
}

private struct LogRow: View {
    let entry: LogEntry

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(entry.time, format: .dateTime.hour().minute().second())
                .foregroundStyle(.secondary)
            Text(entry.level)
                .foregroundStyle(levelColor)
                .frame(width: 44, alignment: .leading)
            VStack(alignment: .leading, spacing: 2) {
                Text(entry.message)
                if let attrs = entry.attrs, !attrs.isEmpty {
                    Text(attrs.sorted { $0.key < $1.key }.map { "\($0.key)=\($0.value)" }.joined(separator: "  "))
                        .foregroundStyle(.secondary)
                }
            }
            .textSelection(.enabled)
        }
        .font(.system(.caption, design: .monospaced))
    }

    private var levelColor: Color {
        switch entry.level {
        case "ERROR": return .red
        case "WARN": return .orange
        default: return .secondary
        }
    }
}
