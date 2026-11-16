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
    public init() {}

    public func run(_ executable: URL, arguments: [String]) throws -> CommandResult {
        let process = Process()
        let pipe = Pipe()
        process.executableURL = executable
        process.arguments = arguments
        process.standardInput = FileHandle.nullDevice
        process.standardOutput = pipe
        process.standardError = pipe
        do {
            try process.run()
        } catch {
            throw ServiceError.launchFailed(executable: executable.path, reason: error.localizedDescription)
        }
        let output = pipe.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        return CommandResult(
            exitCode: process.terminationStatus,
            output: String(decoding: output, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        )
    }
}
