import Foundation

/// Everything that can go wrong when talking to the daemon.
public enum ManagementError: Error, Equatable, Sendable {
    /// Nothing is listening on the management socket.
    case notRunning
    /// The daemon accepted the connection but did not answer in time.
    case timedOut
    /// The connection failed for another reason.
    case transport(String)
    /// The daemon answered with something that is not valid HTTP or JSON.
    case invalidResponse(String)
    /// The daemon rejected the request with an error body.
    case api(status: Int, code: String, message: String)

    /// The daemon's machine-readable error code, for `.api` errors.
    public var code: String? {
        if case let .api(_, code, _) = self { return code }
        return nil
    }
}

extension ManagementError: LocalizedError {
    public var errorDescription: String? {
        switch self {
        case .notRunning:
            return "The Tabtender service is not running."
        case .timedOut:
            return "The Tabtender service did not respond in time."
        case let .transport(detail):
            return "Could not reach the Tabtender service: \(detail)"
        case let .invalidResponse(detail):
            return "The Tabtender service sent an unexpected response: \(detail)"
        case let .api(_, _, message):
            return message
        }
    }
}
