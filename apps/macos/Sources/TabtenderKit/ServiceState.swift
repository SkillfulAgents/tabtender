import Foundation

/// Whether the daemon answers on its management socket.
public enum DaemonReachability: Equatable, Sendable {
    case running
    /// Nothing is listening on the socket.
    case notRunning
    /// A daemon is there but did not give a usable answer: it is hung,
    /// overloaded, or a version this app cannot talk to.
    case unresponsive(reason: String)

    /// Classifies a failed management request.
    public init(failure: Error) {
        if case ManagementError.notRunning = failure {
            self = .notRunning
        } else {
            self = .unresponsive(reason: failure.localizedDescription)
        }
    }
}

/// What the user needs to know about the background service.
public enum ServiceState: Equatable, Sendable {
    case running
    /// The daemon is running but not answering; see `DaemonReachability`.
    case unresponsive(reason: String)
    case stopped
    case notRegistered
    case requiresApproval
    /// The app must be moved to a permanent location before its service can
    /// be registered.
    case needsRelocation
    /// There is no launch agent to register, e.g. when running outside the
    /// app bundle.
    case unavailable

    public init(
        daemon: DaemonReachability,
        registration: ServiceRegistrationStatus,
        installation: AppInstallation
    ) {
        switch (daemon, registration) {
        case (.running, _):
            self = .running
        case let (.unresponsive(reason), _):
            self = .unresponsive(reason: reason)
        case (.notRunning, .enabled):
            self = .stopped
        case (.notRunning, .requiresApproval):
            self = .requiresApproval
        // `SMAppService` reports an agent that was never registered as not
        // found, so only the bundle tells whether there is one to register.
        case (.notRunning, .notRegistered), (.notRunning, .notFound):
            switch installation {
            case .registrable: self = .notRegistered
            case .temporaryLocation: self = .needsRelocation
            case .missingLaunchAgent: self = .unavailable
            }
        }
    }

    /// What the app's first launch does about the service.
    public enum FirstLaunchStep: Equatable, Sendable {
        /// Register the service, which also starts it.
        case register
        /// The service is already registered; leave it as the user has it.
        case finish
        /// Registration is not possible yet; try again on the next launch.
        case postpone
    }

    public var firstLaunchStep: FirstLaunchStep {
        switch self {
        case .notRegistered: return .register
        case .running, .unresponsive, .stopped, .requiresApproval: return .finish
        case .needsRelocation, .unavailable: return .postpone
        }
    }
}
