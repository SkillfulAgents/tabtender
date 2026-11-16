import Foundation

/// Failures while controlling the daemon's launchd service.
public enum ServiceError: Error, Equatable, Sendable {
    /// The bundled `tabtenderd` could not be located.
    case daemonNotFound
    case launchFailed(executable: String, reason: String)
    case commandFailed(command: String, exitCode: Int32, output: String)
}

extension ServiceError: LocalizedError {
    public var errorDescription: String? {
        switch self {
        case .daemonNotFound:
            return "The tabtenderd executable could not be found."
        case let .launchFailed(executable, reason):
            return "Could not run \(executable): \(reason)"
        case let .commandFailed(command, exitCode, output):
            let detail = output.isEmpty ? "" : ": \(output)"
            return "\(command) failed with exit code \(exitCode)\(detail)"
        }
    }
}

/// Controls the lifecycle of the daemon's launchd service: registration via
/// `SMAppService`, and start/stop via `launchctl`.
///
/// The controller never talks to the daemon itself; whether the daemon is
/// running is determined by asking it over the management socket.
public struct ServiceController: Sendable {
    /// The launchd label of the daemon's launch agent.
    public static let label = "com.skillfulagents.tabtender.daemon"
    /// The launch agent plist inside the app bundle.
    public static let plistName = "\(label).plist"

    static let launchctlURL = URL(fileURLWithPath: "/bin/launchctl")

    private let registration: ServiceRegistration
    private let runner: CommandRunner
    private let daemonURL: URL?
    private let userID: uid_t
    private let processExists: @Sendable (pid_t) -> Bool

    /// - Parameters:
    ///   - daemonURL: The bundled `tabtenderd`, or `nil` when it cannot be located.
    ///   - processExists: Reports whether a process is still alive.
    public init(
        registration: ServiceRegistration = LaunchAgentRegistration(),
        runner: CommandRunner = ProcessCommandRunner(),
        daemonURL: URL? = DaemonLocator.locate(),
        userID: uid_t = getuid(),
        processExists: @escaping @Sendable (pid_t) -> Bool = { kill($0, 0) == 0 || errno == EPERM }
    ) {
        self.registration = registration
        self.runner = runner
        self.daemonURL = daemonURL
        self.userID = userID
        self.processExists = processExists
    }

    /// The launchd service target, e.g. `gui/501/com.skillfulagents.tabtender.daemon`.
    public var serviceTarget: String {
        "gui/\(userID)/\(Self.label)"
    }

    public var registrationStatus: ServiceRegistrationStatus {
        registration.status
    }

    /// Registers the launch agent, which also starts the daemon.
    ///
    /// A start request is recorded first so the daemon stays up even when
    /// `app.startOnLogin` is off.
    public func register() throws {
        try requestStart()
        try registration.register()
    }

    /// Unregisters the launch agent, which also stops the daemon.
    public func unregister() throws {
        try registration.unregister()
    }

    /// Starts the registered service. Does nothing if it is already running.
    public func start() throws {
        try requestStart()
        try launchctl(["kickstart", serviceTarget])
    }

    /// Asks the daemon to shut down cleanly. launchd leaves it stopped after
    /// a clean exit.
    public func stop() throws {
        try launchctl(["kill", "SIGTERM", serviceTarget])
    }

    /// Waits for a process to exit, polling until `timeout` elapses. Returns
    /// whether the process is gone.
    ///
    /// Used between `stop()` and `start()`: launchd ignores a kickstart while
    /// the old instance is still shutting down.
    public func waitForExit(of pid: pid_t, timeout: TimeInterval = 30) async -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while processExists(pid) {
            guard Date() < deadline else { return false }
            try? await Task.sleep(nanoseconds: 100_000_000)
        }
        return true
    }

    /// Records that the user wants the service running, which the daemon
    /// honors when started by launchd with `app.startOnLogin` off.
    private func requestStart() throws {
        guard let daemonURL else {
            throw ServiceError.daemonNotFound
        }
        try run(daemonURL, ["service", "request-start"])
    }

    private func launchctl(_ arguments: [String]) throws {
        try run(Self.launchctlURL, arguments)
    }

    private func run(_ executable: URL, _ arguments: [String]) throws {
        let result = try runner.run(executable, arguments: arguments)
        guard result.exitCode == 0 else {
            throw ServiceError.commandFailed(
                command: ([executable.lastPathComponent] + arguments).joined(separator: " "),
                exitCode: result.exitCode,
                output: result.output
            )
        }
    }
}
