import Foundation

/// Finds the `tabtenderd` executable that ships inside the app bundle.
public enum DaemonLocator {
    /// Environment variable that overrides the daemon location (development).
    public static let pathVariable = "TABTENDERD_PATH"

    static let executableName = "tabtenderd"

    /// Returns `$TABTENDERD_PATH` when set, otherwise the `tabtenderd` that
    /// sits next to the running executable (`Contents/MacOS` in the bundle).
    public static func locate(
        environment: [String: String] = ProcessInfo.processInfo.environment,
        executableURL: URL? = Bundle.main.executableURL
    ) -> URL? {
        if let path = environment[pathVariable], !path.isEmpty {
            return URL(fileURLWithPath: path)
        }
        return executableURL?.deletingLastPathComponent().appendingPathComponent(executableName)
    }
}
