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
                let daemon = try? await client.status()
                print("service: \(service.registrationStatus.rawValue)")
                print("daemon: \(daemon.map { "running (pid \($0.pid), version \($0.version))" } ?? "not running")")
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
            try service.register()
        } catch where service.registrationStatus == .requiresApproval {
            print("service registered, but it must be allowed in System Settings > General > Login Items")
            return 2
        }
        return await reportStarted(client, prefix: "service registered")
    }

    private func unregister(_ service: ServiceController) throws -> Int32 {
        if service.registrationStatus == .notRegistered {
            print("service not registered")
            return 0
        }
        try service.unregister()
        print("service unregistered")
        return 0
    }

    private func start(_ service: ServiceController, _ client: ManagementClient) async throws -> Int32 {
        if (try? await client.status()) != nil {
            print("daemon already running")
            return 0
        }
        try service.start()
        return await reportStarted(client, prefix: "service started")
    }

    private func stop(_ service: ServiceController, _ client: ManagementClient) async throws -> Int32 {
        guard let daemon = try? await client.status() else {
            print("daemon not running")
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

    private func reportStarted(_ client: ManagementClient, prefix: String) async -> Int32 {
        let deadline = Date().addingTimeInterval(Self.settleTimeout)
        while Date() < deadline {
            if let daemon = try? await client.status() {
                print("\(prefix); daemon running (pid \(daemon.pid))")
                return 0
            }
            try? await Task.sleep(nanoseconds: 200_000_000)
        }
        print("\(prefix), but the daemon is not answering")
        return 1
    }
}
