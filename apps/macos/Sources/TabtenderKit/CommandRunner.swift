import Foundation

/// The outcome of a finished subprocess.
public struct CommandResult: Equatable, Sendable {
    public var exitCode: Int32
    /// Combined standard output and standard error.
    public var output: String

    public init(exitCode: Int32, output: String = "") {
        self.exitCode = exitCode
        self.output = output
    }
}

/// Runs helper executables to completion. Injectable so service control logic
/// can be tested without touching launchd.
public protocol CommandRunner: Sendable {
    func run(_ executable: URL, arguments: [String]) throws -> CommandResult
}

/// Runs commands as real subprocesses.
public struct ProcessCommandRunner: CommandRunner {
    /// How long a command may run before it is killed.
    public var timeout: TimeInterval

    public init(timeout: TimeInterval = 30) {
        self.timeout = timeout
    }

    public func run(_ executable: URL, arguments: [String]) throws -> CommandResult {
        let process = Process()
        let pipe = Pipe()
        let exited = DispatchSemaphore(value: 0)
        process.executableURL = executable
        process.arguments = arguments
        process.standardInput = FileHandle.nullDevice
        process.standardOutput = pipe
        process.standardError = pipe
        process.terminationHandler = { _ in exited.signal() }
        do {
            try process.run()
        } catch {
            throw ServiceError.launchFailed(executable: executable.path, reason: error.localizedDescription)
        }

        // Drained concurrently so a chatty command cannot fill the pipe and
        // stall before it exits.
        let output = OutputBuffer()
        let drained = DispatchSemaphore(value: 0)
        DispatchQueue.global(qos: .userInitiated).async {
            output.set(pipe.fileHandleForReading.readDataToEndOfFile())
            drained.signal()
        }

        let deadline = DispatchTime.now() + timeout
        guard exited.wait(timeout: deadline) == .success, drained.wait(timeout: deadline) == .success else {
            kill(process.processIdentifier, SIGKILL)
            throw ServiceError.timedOut(
                command: ([executable.lastPathComponent] + arguments).joined(separator: " ")
            )
        }
        return CommandResult(
            exitCode: process.terminationStatus,
            output: String(decoding: output.data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        )
    }
}

/// Hands a subprocess's output from the reading thread to the caller.
private final class OutputBuffer: @unchecked Sendable {
    private let lock = NSLock()
    private var storage = Data()

    var data: Data {
        lock.lock()
        defer { lock.unlock() }
        return storage
    }

    func set(_ data: Data) {
        lock.lock()
        storage = data
        lock.unlock()
    }
}
