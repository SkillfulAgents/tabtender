import Foundation

/// Where the app is running from, which decides whether its launch agent can
/// be registered.
public enum AppInstallation: Equatable, Sendable {
    /// An app bundle with the launch agent, in a place that stays put.
    case registrable
    /// An app bundle on a disk image or at the randomized path Gatekeeper
    /// runs quarantined apps from. launchd would be pointed at a daemon that
    /// disappears when the volume is ejected or the app quits.
    case temporaryLocation
    /// No launch agent to register, e.g. a `swift run` build.
    case missingLaunchAgent

    /// The launch agent plist, relative to the app bundle.
    static let launchAgentPath = "Contents/Library/LaunchAgents/\(ServiceController.plistName)"

    public static func inspect(bundleURL: URL = Bundle.main.bundleURL) -> AppInstallation {
        let launchAgent = bundleURL.appendingPathComponent(launchAgentPath)
        guard FileManager.default.fileExists(atPath: launchAgent.path) else {
            return .missingLaunchAgent
        }
        let values = try? bundleURL.resourceValues(forKeys: [.volumeIsReadOnlyKey])
        let isTemporary = isTemporaryLocation(
            path: bundleURL.path, onReadOnlyVolume: values?.volumeIsReadOnly ?? false
        )
        return isTemporary ? .temporaryLocation : .registrable
    }

    static func isTemporaryLocation(path: String, onReadOnlyVolume: Bool) -> Bool {
        onReadOnlyVolume || path.contains("/AppTranslocation/") || path.hasPrefix("/Volumes/")
    }
}
