import Foundation
import TabtenderKit

/// The app's single source of truth: a mirror of daemon state refreshed by
/// polling, plus the configuration draft being edited in Settings.
@MainActor
final class AppModel: ObservableObject {
    /// The kinds of action whose failures are reported separately, each next
    /// to the controls that caused it.
    enum ActionKind {
        case service
        case session
        case tunnel
    }

    // MARK: Daemon state

    @Published private(set) var registration: ServiceRegistrationStatus
    @Published private(set) var reachability = DaemonReachability.notRunning
    /// `nil` unless the daemon is running and answering.
    @Published private(set) var status: DaemonStatus?
    @Published private(set) var health: HealthStatus?
    /// Sessions whose browser is still alive.
    @Published private(set) var sessions: [Session] = []
    @Published private(set) var isChangingService = false
    /// The most recent failure of each kind of action, shown until the next
    /// action of that kind or until dismissed.
    @Published private(set) var actionErrors: [ActionKind: String] = [:]

    /// This Mac's sleep and logout settings; `nil` until read.
    @Published private(set) var powerSettings: PowerSettings?

    // MARK: Configuration editing

    @Published private(set) var config: ConfigSnapshot?
    /// The persisted (`file`) configuration with the user's unsaved edits.
    @Published var draft: Configuration?
    @Published private(set) var saveError: String?
    @Published private(set) var isSaving = false

    let client: ManagementClient
    private let service: ServiceController
    private let defaults: UserDefaults
    private let powerSettingsReader = PowerSettingsReader()
    private var pollTask: Task<Void, Never>?
    /// Known even when the rest of the daemon's status cannot be decoded.
    private var identity: DaemonIdentity?
    private var refreshGeneration = 0

    private static let pollInterval: UInt64 = 2_000_000_000
    /// System settings change rarely; they are re-read every this many polls
    /// and whenever the user comes back to the app.
    private static let pollsPerPowerSettingsRefresh = 30
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
        ServiceState(daemon: reachability, registration: registration, installation: service.installation)
    }

    var hasUnsavedChanges: Bool {
        draft != config?.file
    }

    // MARK: Lifecycle

    /// Connects to the daemon and starts polling. On the first launch the
    /// service is registered, which also starts it; later launches only
    /// reconnect and never restart a running daemon.
    func activate() {
        guard pollTask == nil else { return }
        pollTask = Task {
            await refresh()
            await registerOnFirstLaunch()
            await repairStaleRegistration()
            var polls = 0
            while !Task.isCancelled {
                if polls % Self.pollsPerPowerSettingsRefresh == 0 {
                    await refreshPowerSettings()
                }
                polls += 1
                try? await Task.sleep(nanoseconds: Self.pollInterval)
                await refresh()
            }
        }
    }

    /// The first launch counts as done only once the service is registered,
    /// so a launch from where that is impossible (a disk image, say) leaves
    /// it for the next one.
    private func registerOnFirstLaunch() async {
        guard !defaults.bool(forKey: Self.firstLaunchKey) else { return }
        switch serviceState.firstLaunchStep {
        case .register:
            await registerService()
            if registration == .enabled || registration == .requiresApproval {
                defaults.set(true, forKey: Self.firstLaunchKey)
            }
        case .finish:
            defaults.set(true, forKey: Self.firstLaunchKey)
        case .postpone:
            break
        }
    }

    /// Registers the service again when the registration belongs to another
    /// build of the app: launchd only spawns the daemon build it was
    /// registered with, and a daemon that kept running across an update is
    /// still the old one.
    private func repairStaleRegistration() async {
        guard registration == .enabled, service.installation == .registrable else { return }
        let service = self.service
        let runningVersion = identity?.version
        let isStopped = reachability == .notRunning
        let isStale = await Task.detached {
            if let runningVersion {
                guard let bundled = service.bundledDaemonVersion else { return false }
                return bundled != runningVersion
            }
            return isStopped && service.isSpawnFailing
        }.value
        if isStale {
            await reregisterService()
        }
    }

    /// Re-reads everything the popover shows.
    func refresh() async {
        refreshGeneration += 1
        let generation = refreshGeneration
        let service = self.service
        let registration = await Task.detached { service.registrationStatus }.value

        var reachability = DaemonReachability.running
        var identity: DaemonIdentity?
        var details: (status: DaemonStatus, health: HealthStatus, sessions: [Session])?
        do {
            identity = try await client.identity()
            async let status = client.status()
            async let health = client.health()
            async let sessions = client.sessions()
            details = try await (status, health, sessions)
        } catch {
            reachability = DaemonReachability(failure: error)
            if reachability == .notRunning {
                identity = nil
            }
        }

        // A refresh that started later has seen newer state; letting this
        // one finish would flip the UI back for a poll cycle.
        guard generation == refreshGeneration else { return }
        let wasRunning = self.reachability == .running
        self.registration = registration
        self.reachability = reachability
        self.identity = identity
        status = details?.status
        health = details?.health
        sessions = details?.sessions.filter(\.isActive) ?? []
        if details != nil, !wasRunning || config == nil {
            await reloadConfig()
        }
    }

    // MARK: Staying awake

    /// The macOS settings that will take the service offline, given how the
    /// running service prevents sleep.
    var sleepRisks: [SleepRisk] {
        guard let powerSettings, let app = config?.effective.app, status != nil else { return [] }
        return SleepRisk.risks(settings: powerSettings, prevention: app.sleepPrevention)
    }

    func refreshPowerSettings() async {
        let reader = powerSettingsReader
        powerSettings = await Task.detached { reader.read() }.value
    }

    // MARK: Service control

    func registerService() async {
        let client = self.client
        await changeService(expectRunning: true) { service in
            _ = try await service.registerVerified { try? await client.identity() }
        }
    }

    /// Replaces a registration that launchd can no longer start.
    func reregisterService() async {
        let client = self.client
        await changeService(expectRunning: true) { service in
            _ = try await service.repairRegistration { try? await client.identity() }
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
        // Replacing the registration kills whatever daemon it runs, so it is
        // reserved for a daemon that is really gone, not one that is merely
        // slow to answer.
        guard reachability == .notRunning, actionErrors[.service] == nil, registration == .enabled else { return }
        await reregisterService()
        if reachability == .notRunning, actionErrors[.service] == nil {
            actionErrors[.service] = Self.startFailureMessage
        }
    }

    func stopService() async {
        await changeService(expectRunning: false) { try $0.stop() }
    }

    /// Stops the daemon, waits for it to exit, and starts it again so that
    /// restart-required settings take effect.
    func restartService() async {
        let pid = identity?.pid
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
        actionErrors[.service] = nil
        let service = self.service
        do {
            try await Task.detached { try await operation(service) }.value
        } catch {
            await refresh()
            // Registering a service that needs approval fails, but the state
            // shown to the user already explains what to do.
            if registration != .requiresApproval {
                actionErrors[.service] = error.localizedDescription
            }
            return
        }
        let settled = expectRunning ? DaemonReachability.running : .notRunning
        for _ in 0..<25 {
            await refresh()
            if reachability == settled {
                return
            }
            try? await Task.sleep(nanoseconds: 200_000_000)
        }
    }

    // MARK: Sessions

    func terminate(_ session: Session) async {
        await perform(.session) {
            try await self.client.terminateSession(id: session.id)
        }
    }

    // MARK: Tunnel

    func restartTunnel() async {
        await perform(.tunnel) {
            try await self.client.restartTunnel()
        }
    }

    /// Stores the Cloudflare tunnel token; an empty token removes it.
    /// Returns whether the daemon accepted it.
    func setTunnelToken(_ token: String) async -> Bool {
        await perform(.tunnel) {
            try await self.client.setTunnelToken(token)
        }
    }

    func dismissError(_ kind: ActionKind) {
        actionErrors[kind] = nil
    }

    @discardableResult
    private func perform(_ kind: ActionKind, _ action: () async throws -> Void) async -> Bool {
        actionErrors[kind] = nil
        var succeeded = true
        do {
            try await action()
        } catch {
            actionErrors[kind] = error.localizedDescription
            succeeded = false
        }
        await refresh()
        return succeeded
    }

    // MARK: Configuration

    /// Fetches the configuration, keeping unsaved edits intact.
    func reloadConfig() async {
        guard let snapshot = try? await client.config() else { return }
        apply(snapshot)
    }

    /// Persists the draft's edits. Only the settings the user changed are
    /// sent, so changes made elsewhere since the draft was loaded survive.
    /// Validation errors reported by the daemon are kept in `saveError` and
    /// the draft is left as edited.
    func saveConfig() async {
        guard let draft, let base = config?.file else { return }
        isSaving = true
        defer { isSaving = false }
        do {
            let snapshot = try await client.updateConfig(draft, changedFrom: base)
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
        if let draft, let base = config?.file, draft != base {
            // Carry the unsaved edits over to the new document, so that
            // saving them does not undo what changed underneath.
            self.draft = (try? draft.rebased(from: base, onto: snapshot.file)) ?? draft
        } else {
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
