import Foundation
import TabtenderKit

/// The app's single source of truth: a mirror of daemon state refreshed by
/// polling, plus the configuration draft being edited in Settings.
@MainActor
final class AppModel: ObservableObject {
    /// What the user needs to know about the background service.
    enum ServiceState {
        case running
        case stopped
        case notRegistered
        case requiresApproval
        /// The launch agent is missing, e.g. when running outside the app bundle.
        case unavailable
    }

    // MARK: Daemon state

    @Published private(set) var registration: ServiceRegistrationStatus
    /// `nil` while the daemon is not answering.
    @Published private(set) var status: DaemonStatus?
    @Published private(set) var health: HealthStatus?
    /// Sessions whose browser is still alive.
    @Published private(set) var sessions: [Session] = []
    @Published private(set) var isChangingService = false
    /// The most recent failed action, shown until the next one succeeds.
    @Published private(set) var actionError: String?

    // MARK: Configuration editing

    @Published private(set) var config: ConfigSnapshot?
    /// The persisted (`file`) configuration with the user's unsaved edits.
    @Published var draft: Configuration?
    @Published private(set) var saveError: String?
    @Published private(set) var isSaving = false

    let client: ManagementClient
    private let service: ServiceController
    private let defaults: UserDefaults
    private var pollTask: Task<Void, Never>?

    private static let pollInterval: UInt64 = 2_000_000_000
    private static let firstLaunchKey = "didCompleteFirstLaunch"
    private static let startFailureMessage =
        "The service did not start. Try unregistering and registering it again in Settings."

    init(
        client: ManagementClient = ManagementClient(),
        service: ServiceController = ServiceController(),
        defaults: UserDefaults = .standard
    ) {
        self.client = client
        self.service = service
        self.defaults = defaults
        registration = service.registrationStatus
    }

    var serviceState: ServiceState {
        if status != nil {
            return .running
        }
        switch registration {
        case .enabled: return .stopped
        case .notRegistered: return .notRegistered
        case .requiresApproval: return .requiresApproval
        case .notFound: return .unavailable
        }
    }

    var hasUnsavedChanges: Bool {
        draft != config?.file
    }

    // MARK: Lifecycle

    /// Connects to the daemon and starts polling. On the very first launch
    /// the service is registered, which also starts it; later launches only
    /// reconnect and never restart a running daemon.
    func activate() {
        guard pollTask == nil else { return }
        pollTask = Task {
            await refresh()
            await registerOnFirstLaunch()
            await repairStaleRegistration()
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: Self.pollInterval)
                await refresh()
            }
        }
    }

    private func registerOnFirstLaunch() async {
        guard !defaults.bool(forKey: Self.firstLaunchKey) else { return }
        defaults.set(true, forKey: Self.firstLaunchKey)
        if serviceState == .notRegistered {
            await registerService()
        }
    }

    /// Registers the service again when the registration belongs to another
    /// build of the app: launchd only spawns the daemon build it was
    /// registered with, and a daemon that kept running across an update is
    /// still the old one.
    private func repairStaleRegistration() async {
        guard registration == .enabled else { return }
        let service = self.service
        let runningVersion = status?.version
        let isStale = await Task.detached {
            if let runningVersion {
                guard let bundled = service.bundledDaemonVersion else { return false }
                return bundled != runningVersion
            }
            return service.isSpawnFailing
        }.value
        if isStale {
            await reregisterService()
        }
    }

    /// Re-reads everything the popover shows.
    func refresh() async {
        registration = service.registrationStatus
        do {
            async let status = client.status()
            async let health = client.health()
            async let sessions = client.sessions()
            let wasRunning = self.status != nil
            self.status = try await status
            self.health = try await health
            self.sessions = try await sessions.filter(\.isActive)
            if !wasRunning || config == nil {
                await reloadConfig()
            }
        } catch {
            status = nil
            health = nil
            sessions = []
        }
    }

    // MARK: Service control

    func registerService() async {
        let client = self.client
        await changeService(expectRunning: true) { service in
            _ = try await service.registerVerified { (try? await client.status()) != nil }
        }
    }

    /// Replaces a registration that launchd can no longer start.
    func reregisterService() async {
        let client = self.client
        await changeService(expectRunning: true) { service in
            _ = try await service.repairRegistration { (try? await client.status()) != nil }
        }
    }

    func unregisterService() async {
        await changeService(expectRunning: false) { try $0.unregister() }
    }

    /// Starts the service. If launchd accepts the request but the daemon
    /// never comes up, the registration is stale; it is replaced once, and
    /// a failure after that is reported instead of leaving the user guessing.
    func startService() async {
        await changeService(expectRunning: true) { try $0.start() }
        guard status == nil, actionError == nil, registration == .enabled else { return }
        await reregisterService()
        if status == nil, actionError == nil {
            actionError = Self.startFailureMessage
        }
    }

    func stopService() async {
        await changeService(expectRunning: false) { try $0.stop() }
    }

    /// Stops the daemon, waits for it to exit, and starts it again so that
    /// restart-required settings take effect.
    func restartService() async {
        let pid = status?.pid
        await changeService(expectRunning: true) { service in
            try service.stop()
            if let pid, await !service.waitForExit(of: pid) {
                throw ServiceRestartError.stillRunning
            }
            try service.start()
        }
    }

    /// Runs a blocking service operation off the main actor, then polls
    /// briefly so the UI settles on the resulting state.
    private func changeService(
        expectRunning: Bool,
        _ operation: @escaping @Sendable (ServiceController) async throws -> Void
    ) async {
        isChangingService = true
        defer { isChangingService = false }
        let service = self.service
        do {
            try await Task.detached { try await operation(service) }.value
            actionError = nil
        } catch {
            // Registering a service that needs approval fails, but the state
            // shown to the user already explains what to do.
            actionError = service.registrationStatus == .requiresApproval ? nil : error.localizedDescription
            await refresh()
            return
        }
        for _ in 0..<25 {
            await refresh()
            if (status != nil) == expectRunning {
                return
            }
            try? await Task.sleep(nanoseconds: 200_000_000)
        }
    }

    // MARK: Sessions

    func terminate(_ session: Session) async {
        await perform {
            try await self.client.terminateSession(id: session.id)
        }
    }

    // MARK: Tunnel

    func restartTunnel() async {
        await perform {
            try await self.client.restartTunnel()
        }
    }

    /// Stores the Cloudflare tunnel token; an empty token removes it.
    func setTunnelToken(_ token: String) async {
        await perform {
            try await self.client.setTunnelToken(token)
        }
    }

    private func perform(_ action: () async throws -> Void) async {
        do {
            try await action()
            actionError = nil
        } catch {
            actionError = error.localizedDescription
        }
        await refresh()
    }

    // MARK: Configuration

    /// Fetches the configuration, keeping unsaved edits intact.
    func reloadConfig() async {
        guard let snapshot = try? await client.config() else { return }
        apply(snapshot)
    }

    /// Persists the draft. Validation errors reported by the daemon are kept
    /// in `saveError` and the draft is left as edited.
    func saveConfig() async {
        guard let draft else { return }
        isSaving = true
        defer { isSaving = false }
        do {
            let snapshot = try await client.updateConfig(draft)
            config = snapshot
            self.draft = snapshot.file
            saveError = nil
        } catch {
            saveError = error.localizedDescription
            // The daemon may have saved the file and failed afterwards.
            await reloadConfig()
        }
        await refresh()
    }

    func discardChanges() {
        draft = config?.file
        saveError = nil
    }

    private func apply(_ snapshot: ConfigSnapshot) {
        if !hasUnsavedChanges {
            draft = snapshot.file
        }
        config = snapshot
    }
}

private enum ServiceRestartError: LocalizedError {
    case stillRunning

    var errorDescription: String? {
        "The service did not stop in time. Try again in a moment."
    }
}
