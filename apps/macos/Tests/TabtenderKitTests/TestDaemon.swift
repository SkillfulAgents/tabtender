import XCTest
import TabtenderKit

/// A real `tabtenderd` running in an isolated environment: its own data
/// directory, config file, TCP port, management socket, and file-backed
/// secrets (so tests never touch the Keychain).
final class TestDaemon {
    let dataDirectory: URL
    let socketPath: String
    let port: Int
    let client: ManagementClient

    private let process = Process()

    /// Launches the daemon and waits until it answers on its socket.
    init(binary: URL, dataDirectory: URL, environment extra: [String: String] = [:]) async throws {
        self.dataDirectory = dataDirectory
        // Kept short: sun_path holds at most 104 bytes.
        socketPath = "/tmp/tt-\(UUID().uuidString.prefix(8).lowercased()).sock"
        port = try Self.freePort()
        client = ManagementClient(socketPath: socketPath)

        let inherited = ProcessInfo.processInfo.environment
        var environment = [
            "TABTENDER_DATA_DIR": dataDirectory.path,
            "TABTENDER_CONFIG_PATH": dataDirectory.appendingPathComponent("config.yaml").path,
            "TABTENDER_PORT": String(port),
            "TABTENDER_MGMT_SOCKET": socketPath,
            "TABTENDER_SECRETS_BACKEND": "file",
        ]
        for name in ["PATH", "HOME", "TMPDIR", "TABTENDER_CHROME_PATH"] {
            environment[name] = inherited[name]
        }
        environment.merge(extra) { _, new in new }

        let logURL = dataDirectory.appendingPathComponent("daemon-output.log")
        FileManager.default.createFile(atPath: logURL.path, contents: nil)
        let log = try FileHandle(forWritingTo: logURL)
        log.seekToEndOfFile()

        process.executableURL = binary
        process.arguments = ["run"]
        process.environment = environment
        process.standardInput = FileHandle.nullDevice
        process.standardOutput = log
        process.standardError = log
        try process.run()

        do {
            try await waitUntilReady()
        } catch {
            stop()
            let output = (try? String(contentsOf: logURL, encoding: .utf8)) ?? ""
            throw StartupFailure(description: "tabtenderd did not become ready: \(error)\n\(output)")
        }
    }

    var pid: Int32 { process.processIdentifier }

    /// Stops the daemon with SIGTERM, escalating to SIGKILL if it lingers.
    func stop() {
        guard process.isRunning else { return }
        process.terminate()
        let deadline = Date().addingTimeInterval(20)
        while process.isRunning, Date() < deadline {
            Thread.sleep(forTimeInterval: 0.05)
        }
        if process.isRunning {
            kill(process.processIdentifier, SIGKILL)
            process.waitUntilExit()
        }
        unlink(socketPath)
    }

    private func waitUntilReady() async throws {
        let deadline = Date().addingTimeInterval(20)
        while true {
            do {
                _ = try await client.status()
                return
            } catch {
                guard process.isRunning, Date() < deadline else { throw error }
                try await Task.sleep(nanoseconds: 50_000_000)
            }
        }
    }

    /// Asks the kernel for a TCP port that is free right now.
    private static func freePort() throws -> Int {
        let descriptor = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(descriptor) }
        var address = sockaddr_in()
        address.sin_family = sa_family_t(AF_INET)
        address.sin_addr.s_addr = inet_addr("127.0.0.1")
        var length = socklen_t(MemoryLayout<sockaddr_in>.size)
        let bound = withUnsafeMutablePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(descriptor, $0, length) == 0 && getsockname(descriptor, $0, &length) == 0
            }
        }
        guard descriptor >= 0, bound else {
            throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
        }
        return Int(UInt16(bigEndian: address.sin_port))
    }

    struct StartupFailure: Error, CustomStringConvertible {
        let description: String
    }
}

/// Base class for tests that drive a real daemon. Tests are skipped unless
/// `TABTENDERD_PATH` points at a `tabtenderd` binary, and every daemon they
/// launch is stopped on teardown, whether the test passed or not.
class DaemonTestCase: XCTestCase {
    private var daemons: [TestDaemon] = []
    private var directories: [URL] = []

    override func tearDown() {
        daemons.forEach { $0.stop() }
        daemons = []
        directories.forEach { try? FileManager.default.removeItem(at: $0) }
        directories = []
        super.tearDown()
    }

    /// Creates a data directory that is removed after the test.
    func makeDataDirectory() throws -> URL {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("tabtender-tests-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        directories.append(directory)
        return directory
    }

    /// Launches a daemon, by default in a fresh data directory.
    func launchDaemon(dataDirectory: URL? = nil, environment: [String: String] = [:]) async throws -> TestDaemon {
        guard let path = ProcessInfo.processInfo.environment["TABTENDERD_PATH"], !path.isEmpty else {
            throw XCTSkip("set TABTENDERD_PATH to a tabtenderd binary to run the integration tests")
        }
        let daemon = try await TestDaemon(
            binary: URL(fileURLWithPath: path),
            dataDirectory: try dataDirectory ?? makeDataDirectory(),
            environment: environment
        )
        daemons.append(daemon)
        return daemon
    }
}
