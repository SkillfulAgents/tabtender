import Foundation

// MARK: - Status

/// `GET /mgmt/v1/status`: a snapshot of the running daemon.
public struct DaemonStatus: Codable, Equatable, Sendable {
    public var version: String
    public var pid: Int32
    public var startedAt: Date
    public var uptimeSeconds: Int
    /// Loopback base URL of the REST API.
    public var localUrl: String
    /// Externally reachable base URL, or "" when there is none.
    public var publicUrl: String
    public var sessions: Capacity
    public var browser: BrowserStatus
    public var tunnel: TunnelStatus
    /// Where the Cloudflare tunnel token comes from; "" when none is stored.
    public var tunnelTokenSource: String
    public var configPath: String
    public var dataDir: String
    public var logFile: String
    /// Restart-required settings that were changed since the daemon started.
    public var pendingRestart: [String]
}

/// `GET /v1/health`.
public struct HealthStatus: Codable, Equatable, Sendable {
    /// "ok" or "degraded".
    public var status: String
    public var version: String
    public var uptimeSeconds: Int
    public var sessions: Capacity
    public var browser: BrowserStatus
    public var tunnel: TunnelStatus

    public var isHealthy: Bool { status == "ok" }
}

/// Session slots in use and available.
public struct Capacity: Codable, Equatable, Sendable {
    public var active: Int
    public var max: Int
    public var available: Int
}

/// Whether the configured browser can be launched.
public struct BrowserStatus: Codable, Equatable, Sendable {
    public var runtime: String
    public var available: Bool
    public var executable: String?
    public var error: String?
}

/// The state of the tunnel that exposes the service publicly.
public struct TunnelStatus: Codable, Equatable, Sendable {
    /// Lifecycle states reported by the daemon.
    public enum State: String, Codable, Sendable {
        case disabled, starting, running, error, stopped
        /// A state introduced by a newer daemon.
        case unknown

        public init(from decoder: Decoder) throws {
            let raw = try decoder.singleValueContainer().decode(String.self)
            self = State(rawValue: raw) ?? .unknown
        }
    }

    public var provider: String
    public var state: State
    /// Public base URL while the tunnel is running.
    public var url: String
    public var error: String
}

// MARK: - Sessions

/// A browser session, as returned by `/v1/sessions`.
public struct Session: Codable, Equatable, Sendable, Identifiable {
    public var id: String
    public var status: String
    public var headless: Bool
    /// Named persistent profile; `nil` for an ephemeral session.
    public var profile: String?
    public var createdAt: Date
    public var expiresAt: Date
    public var cdpUrl: String?
    public var connectedClients: Int
    public var endedAt: Date?
    public var endReason: String?

    /// False once the browser has exited.
    public var isActive: Bool { status != "terminated" }
}

/// Body of `POST /v1/sessions`. Omitted fields use the daemon's defaults.
public struct CreateSessionRequest: Codable, Equatable, Sendable {
    public var ttlSeconds: Int?
    public var headless: Bool?
    public var profile: String?

    public init(ttlSeconds: Int? = nil, headless: Bool? = nil, profile: String? = nil) {
        self.ttlSeconds = ttlSeconds
        self.headless = headless
        self.profile = profile
    }
}

// MARK: - Configuration

/// `GET`/`PUT /mgmt/v1/config`: the configuration layers.
public struct ConfigSnapshot: Codable, Equatable, Sendable {
    /// Location of the configuration file.
    public var path: String
    /// What is persisted. This is the only layer a client edits.
    public var file: Configuration
    /// What is in force after environment overrides.
    public var effective: Configuration
    /// Settings pinned by the environment, keyed by dotted name ("server.port").
    public var envOverrides: [String: EnvOverride]
    /// Settings that only take effect after the service restarts.
    public var restartRequiredKeys: [String]
    public var pendingRestart: [String]
}

/// A setting controlled by an environment variable.
public struct EnvOverride: Codable, Equatable, Sendable {
    public var variable: String
    public var value: String
}

/// The Tabtender configuration document.
public struct Configuration: Codable, Equatable, Sendable {
    public var server: Server
    public var sessions: Sessions
    public var browser: Browser
    public var tunnel: Tunnel
    public var app: App

    public struct Server: Codable, Equatable, Sendable {
        public var host: String
        public var port: Int
        public var publicUrl: String
    }

    /// Session limits. TTLs are in seconds.
    public struct Sessions: Codable, Equatable, Sendable {
        public var defaultTTL: Int
        public var maxTTL: Int
        public var maxConcurrent: Int
    }

    public struct Browser: Codable, Equatable, Sendable {
        public var runtime: String
        /// `Configuration.autoPath` or an absolute path.
        public var executablePath: String
        public var headless: Bool
        public var extraArgs: [String]

        public init(from decoder: Decoder) throws {
            let container = try decoder.container(keyedBy: CodingKeys.self)
            runtime = try container.decode(String.self, forKey: .runtime)
            executablePath = try container.decode(String.self, forKey: .executablePath)
            headless = try container.decode(Bool.self, forKey: .headless)
            // A file without extra arguments serializes the list as null.
            extraArgs = try container.decodeIfPresent([String].self, forKey: .extraArgs) ?? []
        }
    }

    public struct Tunnel: Codable, Equatable, Sendable {
        public var provider: String
        public var cloudflare: Cloudflare
    }

    public struct Cloudflare: Codable, Equatable, Sendable {
        public var executablePath: String
        public var hostname: String
    }

    public struct App: Codable, Equatable, Sendable {
        public var startOnLogin: Bool
        public var preventSleep: Bool
    }

    /// Asks the daemon to discover an executable on its own.
    public static let autoPath = "auto"
}

/// Tunnel providers understood by the daemon.
public enum TunnelProvider: String, CaseIterable, Identifiable, Sendable {
    case none
    case cloudflareQuick = "cloudflare-quick"
    case cloudflareNamed = "cloudflare-named"

    public var id: String { rawValue }

    public var displayName: String {
        switch self {
        case .none: return "None"
        case .cloudflareQuick: return "Cloudflare Quick Tunnel"
        case .cloudflareNamed: return "Cloudflare Named Tunnel"
        }
    }
}

// MARK: - Secrets

/// `GET /mgmt/v1/apikey`.
public struct APIKeyInfo: Codable, Equatable, Sendable {
    public var apiKey: String
    /// "environment" when pinned by `TABTENDER_API_KEY`, else the secret store.
    public var source: String
    /// False when the key is pinned by the environment.
    public var rotatable: Bool
}

/// Well-known values of the secret `source` fields.
public enum SecretSource {
    /// The secret is pinned by an environment variable and cannot be changed.
    public static let environment = "environment"
}

// MARK: - Logs

/// One buffered daemon log record.
public struct LogEntry: Codable, Equatable, Sendable {
    public var time: Date
    /// "INFO", "WARN" or "ERROR".
    public var level: String
    public var message: String
    public var attrs: [String: String]?
}

/// Minimum severity filter for `GET /mgmt/v1/logs`.
public enum LogLevel: String, Sendable {
    case info, warn, error
}
