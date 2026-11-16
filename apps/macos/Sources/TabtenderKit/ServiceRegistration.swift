import Foundation
import ServiceManagement

/// Whether the daemon's launch agent is registered with the system.
public enum ServiceRegistrationStatus: String, Equatable, Sendable {
    case notRegistered
    case enabled
    /// Registered, but the user must allow it in System Settings.
    case requiresApproval
    /// The launch agent plist is missing, e.g. when running outside the app bundle.
    case notFound
}

/// Registers the daemon's launch agent. Injectable so service control logic
/// can be tested without touching the system.
public protocol ServiceRegistration: Sendable {
    var status: ServiceRegistrationStatus { get }
    func register() throws
    func unregister() throws
}

/// Registration backed by `SMAppService`, using the launch agent plist that
/// ships in the app bundle's `Contents/Library/LaunchAgents`.
///
/// `SMAppService` is an immutable handle whose calls go to the system's
/// service management daemon, so sharing it across threads is safe.
public struct LaunchAgentRegistration: ServiceRegistration, @unchecked Sendable {
    private let service: SMAppService

    public init(plistName: String = ServiceController.plistName) {
        service = SMAppService.agent(plistName: plistName)
    }

    public var status: ServiceRegistrationStatus {
        switch service.status {
        case .notRegistered: return .notRegistered
        case .enabled: return .enabled
        case .requiresApproval: return .requiresApproval
        case .notFound: return .notFound
        @unknown default: return .notFound
        }
    }

    public func register() throws {
        try service.register()
    }

    public func unregister() throws {
        try service.unregister()
    }

    /// Opens System Settings at Login Items, where a launch agent that
    /// requires approval can be allowed.
    public static func openLoginItemsSettings() {
        SMAppService.openSystemSettingsLoginItems()
    }
}
