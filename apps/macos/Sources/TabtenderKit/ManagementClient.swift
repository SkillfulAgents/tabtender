import Foundation

/// Async client for the daemon's local management API: plain HTTP/1.1 and
/// JSON over a Unix domain socket, one request per connection.
///
/// The daemon authenticates callers by socket peer credentials, so no API key
/// is involved. All methods throw `ManagementError`.
public struct ManagementClient: Sendable {
    /// Time allowed for read-only requests.
    public var timeout: TimeInterval
    /// Time allowed for requests that make the daemon do real work, such as
    /// launching a browser or reconfiguring a tunnel.
    public var mutationTimeout: TimeInterval

    private let transport: ManagementTransport

    public init(
        socketPath: String = ManagementSocket.resolvePath(),
        timeout: TimeInterval = 5,
        mutationTimeout: TimeInterval = 60
    ) {
        self.init(transport: UnixSocketTransport(path: socketPath), timeout: timeout, mutationTimeout: mutationTimeout)
    }

    init(transport: ManagementTransport, timeout: TimeInterval = 5, mutationTimeout: TimeInterval = 60) {
        self.transport = transport
        self.timeout = timeout
        self.mutationTimeout = mutationTimeout
    }

    // MARK: Status

    public func status() async throws -> DaemonStatus {
        try await get("/mgmt/v1/status")
    }

    public func health() async throws -> HealthStatus {
        try await get("/v1/health")
    }

    // MARK: Sessions

    /// Lists sessions, including recently terminated ones.
    public func sessions() async throws -> [Session] {
        struct Envelope: Decodable { var sessions: [Session] }
        let envelope: Envelope = try await get("/v1/sessions")
        return envelope.sessions
    }

    public func createSession(_ request: CreateSessionRequest = CreateSessionRequest()) async throws -> Session {
        try await send("POST", "/v1/sessions", body: request)
    }

    @discardableResult
    public func terminateSession(id: String) async throws -> Session {
        try await send("DELETE", "/v1/sessions/\(Self.escape(id))")
    }

    // MARK: Configuration

    public func config() async throws -> ConfigSnapshot {
        try await get("/mgmt/v1/config")
    }

    /// Replaces the persisted configuration. The daemon validates the
    /// document and rejects it with a `validation_error` when it is invalid.
    public func updateConfig(_ file: Configuration) async throws -> ConfigSnapshot {
        try await send("PUT", "/mgmt/v1/config", body: file)
    }

    // MARK: Secrets

    public func apiKey() async throws -> APIKeyInfo {
        try await get("/mgmt/v1/apikey")
    }

    /// Replaces the API key. Fails with `environment_controlled` when the key
    /// is pinned by the environment.
    public func rotateAPIKey() async throws -> APIKeyInfo {
        try await send("POST", "/mgmt/v1/apikey/rotate")
    }

    /// Stores the Cloudflare tunnel token and restarts the tunnel. An empty
    /// token deletes the stored one. Returns the resulting token source.
    @discardableResult
    public func setTunnelToken(_ token: String) async throws -> String {
        struct Response: Decodable { var tunnelTokenSource: String }
        let response: Response = try await send("PUT", "/mgmt/v1/tunnel/token", body: ["token": token])
        return response.tunnelTokenSource
    }

    // MARK: Tunnel

    @discardableResult
    public func restartTunnel() async throws -> TunnelStatus {
        try await send("POST", "/mgmt/v1/tunnel/restart")
    }

    // MARK: Logs

    /// Returns up to `limit` recent log entries at or above `minimumLevel`,
    /// oldest first.
    public func logs(limit: Int = 200, minimumLevel: LogLevel = .info) async throws -> [LogEntry] {
        struct Envelope: Decodable { var entries: [LogEntry] }
        let envelope: Envelope = try await get("/mgmt/v1/logs?limit=\(limit)&level=\(minimumLevel.rawValue)")
        return envelope.entries
    }

    // MARK: Plumbing

    private func get<Response: Decodable>(_ path: String) async throws -> Response {
        try await perform(HTTPRequest(method: "GET", path: path), timeout: timeout)
    }

    private func send<Response: Decodable>(_ method: String, _ path: String) async throws -> Response {
        try await perform(HTTPRequest(method: method, path: path), timeout: mutationTimeout)
    }

    private func send<Body: Encodable, Response: Decodable>(
        _ method: String, _ path: String, body: Body
    ) async throws -> Response {
        let encoded = try JSONEncoder().encode(body)
        return try await perform(HTTPRequest(method: method, path: path, body: encoded), timeout: mutationTimeout)
    }

    private func perform<Response: Decodable>(_ request: HTTPRequest, timeout: TimeInterval) async throws -> Response {
        let raw = try await roundTrip(request.serialized(), timeout: timeout)
        let response = try HTTPResponse(parsing: raw)
        guard (200...299).contains(response.status) else {
            throw Self.error(from: response)
        }
        do {
            return try JSONDecoder.management().decode(Response.self, from: response.body)
        } catch {
            throw ManagementError.invalidResponse("\(request.method) \(request.path): \(error)")
        }
    }

    /// Runs the blocking transport off the cooperative thread pool.
    private func roundTrip(_ request: Data, timeout: TimeInterval) async throws -> Data {
        let transport = self.transport
        return try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                continuation.resume(with: Result { try transport.roundTrip(request, timeout: timeout) })
            }
        }
    }

    private static func error(from response: HTTPResponse) -> ManagementError {
        struct Envelope: Decodable {
            struct Detail: Decodable {
                var code: String
                var message: String
            }
            var error: Detail
        }
        guard let envelope = try? JSONDecoder().decode(Envelope.self, from: response.body) else {
            return .invalidResponse("HTTP \(response.status) without an error body")
        }
        return .api(status: response.status, code: envelope.error.code, message: envelope.error.message)
    }

    private static func escape(_ pathComponent: String) -> String {
        var allowed = CharacterSet.urlPathAllowed
        allowed.remove("/")
        return pathComponent.addingPercentEncoding(withAllowedCharacters: allowed) ?? pathComponent
    }
}
