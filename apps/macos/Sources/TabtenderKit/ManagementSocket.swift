import Foundation

/// Locates the daemon's management socket.
public enum ManagementSocket {
    /// Environment variable that overrides the socket path.
    public static let pathVariable = "TABTENDER_MGMT_SOCKET"
    /// Environment variable that relocates the daemon's data directory, which
    /// holds the socket by default.
    public static let dataDirectoryVariable = "TABTENDER_DATA_DIR"

    static let fileName = "tabtenderd.sock"

    /// Resolves the socket path the same way `tabtenderd` does:
    /// `$TABTENDER_MGMT_SOCKET`, else `$TABTENDER_DATA_DIR/tabtenderd.sock`,
    /// else `~/Library/Application Support/Tabtender/tabtenderd.sock`.
    public static func resolvePath(
        environment: [String: String] = ProcessInfo.processInfo.environment,
        homeDirectory: URL = FileManager.default.homeDirectoryForCurrentUser
    ) -> String {
        if let path = environment[pathVariable], !path.isEmpty {
            return path
        }
        if let dataDirectory = environment[dataDirectoryVariable], !dataDirectory.isEmpty {
            return URL(fileURLWithPath: dataDirectory).appendingPathComponent(fileName).path
        }
        return homeDirectory
            .appendingPathComponent("Library/Application Support/Tabtender")
            .appendingPathComponent(fileName)
            .path
    }
}
