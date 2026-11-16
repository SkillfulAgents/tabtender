import Foundation

/// Decodes the RFC 3339 timestamps Go emits, whose fractional part has
/// anywhere from zero to nine digits.
enum RFC3339 {
    static func date(from string: String) -> Date? {
        guard let dot = string.firstIndex(of: ".") else {
            return wholeSeconds.date(from: string)
        }
        let afterDot = string.index(after: dot)
        let fractionEnd = string[afterDot...].firstIndex { !$0.isNumber } ?? string.endIndex
        guard afterDot < fractionEnd,
              let fraction = Double("0." + string[afterDot..<fractionEnd]),
              let base = wholeSeconds.date(from: String(string[..<dot] + string[fractionEnd...])) else {
            return nil
        }
        return base.addingTimeInterval(fraction)
    }

    private static let wholeSeconds: ISO8601DateFormatter = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        return formatter
    }()
}

extension JSONDecoder {
    /// A decoder configured for the daemon's JSON.
    static func management() -> JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let container = try decoder.singleValueContainer()
            let raw = try container.decode(String.self)
            guard let date = RFC3339.date(from: raw) else {
                throw DecodingError.dataCorruptedError(in: container, debugDescription: "invalid RFC 3339 timestamp \"\(raw)\"")
            }
            return date
        }
        return decoder
    }
}
