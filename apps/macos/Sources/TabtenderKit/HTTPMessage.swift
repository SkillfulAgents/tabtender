import Foundation

/// An HTTP/1.1 request destined for the management socket.
struct HTTPRequest: Equatable {
    var method: String
    var path: String
    var body: Data?

    /// Serializes the request. Every request asks the server to close the
    /// connection so the response ends at end-of-stream.
    func serialized() -> Data {
        var head = "\(method) \(path) HTTP/1.1\r\n"
        head += "Host: tabtender\r\n"
        head += "Accept: application/json\r\n"
        head += "Connection: close\r\n"
        if let body {
            head += "Content-Type: application/json\r\n"
            head += "Content-Length: \(body.count)\r\n"
        }
        head += "\r\n"
        var data = Data(head.utf8)
        if let body {
            data.append(body)
        }
        return data
    }
}

/// A parsed HTTP/1.1 response.
struct HTTPResponse: Equatable {
    var status: Int
    /// Header fields keyed by lowercased name.
    var headers: [String: String]
    var body: Data

    private static let headerTerminator = Data("\r\n\r\n".utf8)

    /// Parses a complete response read up to end-of-stream. The daemon always
    /// sends `Content-Length`, so chunked transfer encoding is rejected rather
    /// than decoded.
    init(parsing data: Data) throws {
        guard let terminator = data.range(of: Self.headerTerminator) else {
            throw ManagementError.invalidResponse("incomplete response headers")
        }
        guard let head = String(data: data[data.startIndex..<terminator.lowerBound], encoding: .utf8) else {
            throw ManagementError.invalidResponse("response headers are not valid UTF-8")
        }
        var lines = head.components(separatedBy: "\r\n")
        let statusLine = lines.removeFirst()
        let statusParts = statusLine.split(separator: " ", maxSplits: 2)
        guard statusParts.count >= 2, statusParts[0].hasPrefix("HTTP/1."),
              let status = Int(statusParts[1]), (100...599).contains(status) else {
            throw ManagementError.invalidResponse("malformed status line \"\(statusLine)\"")
        }

        var headers: [String: String] = [:]
        for line in lines {
            guard let colon = line.firstIndex(of: ":") else {
                throw ManagementError.invalidResponse("malformed header line \"\(line)\"")
            }
            let name = line[..<colon].trimmingCharacters(in: .whitespaces).lowercased()
            headers[name] = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        if let encoding = headers["transfer-encoding"], encoding.lowercased() != "identity" {
            throw ManagementError.invalidResponse("unsupported transfer encoding \"\(encoding)\"")
        }

        var body = Data(data[terminator.upperBound...])
        if let rawLength = headers["content-length"] {
            guard let length = Int(rawLength), length >= 0 else {
                throw ManagementError.invalidResponse("malformed Content-Length \"\(rawLength)\"")
            }
            guard body.count >= length else {
                throw ManagementError.invalidResponse("response body was truncated")
            }
            body = body.prefix(length)
        }

        self.status = status
        self.headers = headers
        self.body = body
    }
}
