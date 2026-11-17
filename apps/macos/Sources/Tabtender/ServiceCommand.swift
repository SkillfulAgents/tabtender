import Foundation
import TabtenderKit

/// Headless service management, selected by a command-line flag.
enum ServiceCommand: String {
    case register = "--register-service"
    case unregister = "--unregister-service"
    case start = "--start-service"
    case stop = "--stop-service"
    case status = "--service-status"

    /// How long to wait for the daemon to come up or go away.
    private static let settleTimeout: TimeInterval = 20
    /// How long a kickstarted daemon gets before its registration is
    /// considered stale.
    private static let startTimeout: TimeInterval = 8

    init?(arguments: [String]) {
        guard let command = arguments.dropFirst().lazy.compactMap(ServiceCommand.init(rawValue:)).first else {
            return nil
        }
        self = command
    }

    /// Runs the command, prints a short result, and returns the exit code.
    func run() async -> Int32 {
        let service = ServiceController()
        let client = ManagementClient(timeout: 2)
        do {
            switch self {
            case .register:
                return try await register(service, client)
            case .unregister:
                return try unregister(service)
            case .start:
                return try await start(service, client)
            case .stop:
                return try await stop(service, client)
            case .status:
                print("service: \(service.registrationStatus.rawValue)")
                do {
                    let daemon = try await client.identity()
                    print("daemon: running (pid \(daemon.pid), version \(daemon.version))")
                } catch ManagementError.notRunning {
                    print("daemon: not running")
                } catch {
                    print("daemon: not responding (\(error.localizedDescription))")
                }
                return 0
            }
        } catch {
            FileHandle.standardError.write(Data("error: \(error.localizedDescription)\n".utf8))
            return 1
        }
    }

    private func register(_ service: ServiceController, _ client: ManagementClient) async throws -> Int32 {
        if service.registrationStatus == .enabled {
            print("service already registered")
            return 0
        }
        do {
            _ = try await service.registerVerified { try? await client.identity() }
        } catch where service.registrationStatus == .requiresApproval {
            print("service registered, but it must be allowed in System Settings > General > Login Items")
            return 2
        }
        if service.registrationStatus == .requiresApproval {
            print("service registered, but it must be allowed in System Settings > General > Login Items")
            return 2
        }
        return await reportStarted(client, prefix: "service registered", timeout: 2)
    }

    private func unregister(_ service: ServiceController) throws -> Int32 {
        // An agent that was never registered is reported as not found.
        if [.notRegistered, .notFound].contains(service.registrationStatus) {
            print("service not registered")
            return 0
        }
        try service.unregister()
        print("service unregistered")
        return 0
    }

    private func start(_ service: ServiceController, _ client: ManagementClient) async throws -> Int32 {
        // Fails for a daemon that is running but not answering: the repair
        // below would otherwise kill it and its sessions.
        if try await runningDaemon(client) != nil {
            print("daemon already running")
            return 0
        }
        try service.start()
        if await reportStarted(client, prefix: "service started", timeout: Self.startTimeout) == 0 {
            return 0
        }
        if let daemon = try await runningDaemon(client) {
            print("service started; daemon running (pid \(daemon.pid))")
            return 0
        }
        // launchd only spawns the build the service was registered with;
        // a registration left over from another build has to be replaced.
        print("replacing a stale service registration")
        _ = try await service.repairRegistration { try? await client.identity() }
        return await reportStarted(client, prefix: "service registered again", timeout: 2)
    }

    private func stop(_ service: ServiceController, _ client: ManagementClient) async throws -> Int32 {
        let daemon: DaemonIdentity
        do {
            guard let running = try await runningDaemon(client) else {
                print("daemon not running")
                return 0
            }
            daemon = running
        } catch {
            try service.stop()
            print("stop requested; the daemon was not responding (\(error.localizedDescription))")
            return 0
        }
        try service.stop()
        guard await service.waitForExit(of: daemon.pid, timeout: Self.settleTimeout) else {
            print("daemon (pid \(daemon.pid)) did not exit")
            return 1
        }
        print("daemon stopped")
        return 0
    }

    /// The daemon answering on the management socket, or `nil` when nothing
    /// is listening. Throws when a daemon is there but does not answer.
    private func runningDaemon(_ client: ManagementClient) async throws -> DaemonIdentity? {
        do {
            return try await client.identity()
        } catch ManagementError.notRunning {
            return nil
        }
    }

    private func reportStarted(
        _ client: ManagementClient,
        prefix: String,
        timeout: TimeInterval = ServiceCommand.settleTimeout
    ) async -> Int32 {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if let daemon = try? await client.identity() {
                print("\(prefix); daemon running (pid \(daemon.pid))")
                return 0
            }
            try? await Task.sleep(nanoseconds: 200_000_000)
        }
        print("\(prefix), but the daemon is not answering")
        return 1
    }
}
