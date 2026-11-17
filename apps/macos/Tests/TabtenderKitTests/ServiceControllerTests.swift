import XCTest
@testable import TabtenderKit

/// Records every service operation in order, across both fakes.
private final class Recorder: @unchecked Sendable {
    private let lock = NSLock()
    private var entries: [String] = []

    func record(_ entry: String) {
        lock.lock()
        entries.append(entry)
        lock.unlock()
    }

    var log: [String] {
        lock.lock()
        defer { lock.unlock() }
        return entries
    }
}

private struct FakeRunner: CommandRunner {
    let recorder: Recorder
    /// Exit codes by executable name; anything else succeeds.
    var exitCodes: [String: Int32] = [:]
    var output = "fake output"

    func run(_ executable: URL, arguments: [String]) throws -> CommandResult {
        recorder.record(([executable.path] + arguments).joined(separator: " "))
        return CommandResult(exitCode: exitCodes[executable.lastPathComponent] ?? 0, output: output)
    }
}

private struct FakeRegistration: ServiceRegistration {
    struct Refused: Error {}

    let recorder: Recorder
    var status: ServiceRegistrationStatus = .notRegistered
    /// How many registrations fail before one succeeds.
    var registerFailures = 0

    func register() throws {
        recorder.record("register")
        if recorder.log.filter({ $0 == "register" }).count <= registerFailures {
            throw Refused()
        }
    }

    func unregister() throws { recorder.record("unregister") }
}

final class ServiceControllerTests: XCTestCase {
    private let recorder = Recorder()
    private let daemon = URL(fileURLWithPath: "/Applications/Tabtender.app/Contents/MacOS/tabtenderd")
    private let target = "gui/501/com.skillfulagents.tabtender.daemon"
    private let newDaemon = DaemonIdentity(version: "1.1.0", pid: 200)

    /// A daemon that comes up as soon as the service has been registered.
    private var startsOnceRegistered: @Sendable () async -> DaemonIdentity? {
        { [recorder, newDaemon] in recorder.log.contains("register") ? newDaemon : nil }
    }

    private func controller(
        exitCodes: [String: Int32] = [:],
        daemonURL: URL?? = nil,
        status: ServiceRegistrationStatus = .notRegistered,
        registerFailures: Int = 0,
        installation: AppInstallation = .registrable,
        output: String = "fake output",
        processExists: @escaping @Sendable (pid_t) -> Bool = { _ in false }
    ) -> ServiceController {
        ServiceController(
            registration: FakeRegistration(recorder: recorder, status: status, registerFailures: registerFailures),
            runner: FakeRunner(recorder: recorder, exitCodes: exitCodes, output: output),
            daemonURL: daemonURL ?? daemon,
            installation: installation,
            userID: 501,
            processExists: processExists
        )
    }

    func testIdentifiers() {
        XCTAssertEqual(ServiceController.label, "com.skillfulagents.tabtender.daemon")
        XCTAssertEqual(ServiceController.plistName, "com.skillfulagents.tabtender.daemon.plist")
        XCTAssertEqual(controller().serviceTarget, target)
    }

    func testStartRequestsStartThenKickstarts() throws {
        try controller().start()
        XCTAssertEqual(recorder.log, [
            "\(daemon.path) service request-start",
            "/bin/launchctl kickstart \(target)",
        ])
    }

    func testStartDoesNotKickstartWhenRequestStartFails() {
        XCTAssertThrowsError(try controller(exitCodes: ["tabtenderd": 1]).start()) { error in
            XCTAssertEqual(error as? ServiceError, .commandFailed(
                command: "tabtenderd service request-start", exitCode: 1, output: "fake output"
            ))
        }
        XCTAssertEqual(recorder.log, ["\(daemon.path) service request-start"])
    }

    func testStartFailsWithoutDaemon() {
        XCTAssertThrowsError(try controller(daemonURL: .some(nil)).start()) { error in
            XCTAssertEqual(error as? ServiceError, .daemonNotFound)
        }
        XCTAssertEqual(recorder.log, [])
    }

    func testStopSendsSIGTERMThroughLaunchctl() throws {
        try controller().stop()
        XCTAssertEqual(recorder.log, ["/bin/launchctl kill SIGTERM \(target)"])
    }

    func testLaunchctlFailureIsReported() {
        XCTAssertThrowsError(try controller(exitCodes: ["launchctl": 113]).stop()) { error in
            XCTAssertEqual(error as? ServiceError, .commandFailed(
                command: "launchctl kill SIGTERM \(target)", exitCode: 113, output: "fake output"
            ))
            XCTAssertEqual(
                error.localizedDescription,
                "launchctl kill SIGTERM \(target) failed with exit code 113: fake output"
            )
        }
    }

    func testRegisterRequestsStartBeforeRegistering() throws {
        try controller().register()
        XCTAssertEqual(recorder.log, ["\(daemon.path) service request-start", "register"])
    }

    func testRegisterRefusesATemporaryLocation() {
        XCTAssertThrowsError(try controller(installation: .temporaryLocation).register()) { error in
            XCTAssertEqual(error as? ServiceError, .temporaryLocation)
            XCTAssertEqual(
                error.localizedDescription, "Move Tabtender to the Applications folder, then open it again."
            )
        }
        XCTAssertEqual(recorder.log, [])
    }

    func testRepairReplacesTheRegistration() async throws {
        let repaired = try await controller(status: .enabled)
            .repairRegistration(settleDelay: 0, daemon: startsOnceRegistered)
        XCTAssertTrue(repaired)
        XCTAssertEqual(recorder.log, [
            "\(daemon.path) service request-start",
            "unregister",
            "\(daemon.path) service request-start",
            "register",
        ])
    }

    func testRepairRegistersAgainUntilTheDaemonComesUp() async throws {
        // The daemon only answers after the second registration.
        let recorder = self.recorder
        let newDaemon = self.newDaemon
        let repaired = try await controller(status: .enabled).repairRegistration(startTimeout: 0.3, settleDelay: 0) {
            recorder.log.filter { $0 == "register" }.count >= 2 ? newDaemon : nil
        }
        XCTAssertTrue(repaired)
        XCTAssertEqual(recorder.log.filter { $0 == "register" }.count, 2)
        XCTAssertEqual(recorder.log.filter { $0 == "unregister" }.count, 2)
    }

    func testRepairKeepsTheRegistrationWhenItCannotPrepare() async {
        do {
            _ = try await controller(exitCodes: ["tabtenderd": 1], status: .enabled)
                .repairRegistration(settleDelay: 0, daemon: startsOnceRegistered)
            XCTFail("expected the start request to fail")
        } catch {
            XCTAssertEqual(error as? ServiceError, .commandFailed(
                command: "tabtenderd service request-start", exitCode: 1, output: "fake output"
            ))
        }
        XCTAssertEqual(recorder.log, ["\(daemon.path) service request-start"])
    }

    func testRepairKeepsTheRegistrationInATemporaryLocation() async {
        do {
            _ = try await controller(status: .enabled, installation: .temporaryLocation)
                .repairRegistration(settleDelay: 0, daemon: startsOnceRegistered)
            XCTFail("expected the repair to be refused")
        } catch {
            XCTAssertEqual(error as? ServiceError, .temporaryLocation)
        }
        XCTAssertEqual(recorder.log, [])
    }

    func testRepairRetriesAFailedRegistration() async throws {
        let repaired = try await controller(status: .enabled, registerFailures: 1)
            .repairRegistration(settleDelay: 0, daemon: startsOnceRegistered)
        XCTAssertTrue(repaired)
        XCTAssertEqual(recorder.log.filter { $0 == "register" }.count, 2)
    }

    func testRepairReportsARegistrationThatKeepsFailing() async {
        do {
            _ = try await controller(status: .enabled, registerFailures: 2)
                .repairRegistration(attempts: 2, settleDelay: 0, daemon: startsOnceRegistered)
            XCTFail("expected the registration error")
        } catch {
            XCTAssertTrue(error is FakeRegistration.Refused)
        }
        XCTAssertEqual(recorder.log.filter { $0 == "register" }.count, 2)
    }

    func testRepairDoesNotRetryWhileApprovalIsPending() async {
        do {
            _ = try await controller(status: .requiresApproval, registerFailures: 3)
                .repairRegistration(settleDelay: 0, daemon: startsOnceRegistered)
            XCTFail("expected the registration error")
        } catch {
            XCTAssertTrue(error is FakeRegistration.Refused)
        }
        XCTAssertEqual(recorder.log.filter { $0 == "register" }.count, 1)
    }

    func testRepairIgnoresTheDaemonItIsReplacing() async throws {
        // The old daemon keeps answering while it shuts down.
        let old = DaemonIdentity(version: "1.0.0", pid: 100)
        let repaired = try await controller(status: .enabled)
            .repairRegistration(attempts: 2, startTimeout: 0.1, settleDelay: 0) { old }
        XCTAssertFalse(repaired)
        XCTAssertEqual(recorder.log.filter { $0 == "register" }.count, 2)
    }

    func testRepairWaitsForTheOldDaemonToExitBeforeRegistering() async throws {
        let recorder = self.recorder
        let old = DaemonIdentity(version: "1.0.0", pid: 100)
        let newDaemon = self.newDaemon
        let controller = controller(status: .enabled, processExists: { pid in
            recorder.record("check \(pid)")
            return recorder.log.filter { $0 == "check 100" }.count < 3
        })
        let repaired = try await controller.repairRegistration(settleDelay: 0) {
            recorder.log.contains("register") ? newDaemon : old
        }
        XCTAssertTrue(repaired)
        XCTAssertEqual(recorder.log, [
            "\(daemon.path) service request-start",
            "unregister",
            "check 100", "check 100", "check 100",
            "\(daemon.path) service request-start",
            "register",
        ])
    }

    func testRegisterVerifiedIsASingleRegistrationWhenTheDaemonStarts() async throws {
        let started = try await controller(status: .enabled)
            .registerVerified(settleDelay: 0, daemon: startsOnceRegistered)
        XCTAssertTrue(started)
        XCTAssertEqual(recorder.log, ["\(daemon.path) service request-start", "register"])
    }

    func testRegisterVerifiedRegistersAgainWhenTheFirstSpawnIsRejected() async throws {
        let recorder = self.recorder
        let newDaemon = self.newDaemon
        let started = try await controller(status: .enabled).registerVerified(startTimeout: 0.3, settleDelay: 0) {
            recorder.log.filter { $0 == "register" }.count >= 2 ? newDaemon : nil
        }
        XCTAssertTrue(started)
        XCTAssertEqual(recorder.log.filter { $0 == "unregister" }.count, 1)
    }

    func testRegisterVerifiedDoesNotRetryWhileApprovalIsPending() async throws {
        let started = try await controller(status: .requiresApproval)
            .registerVerified(startTimeout: 0.1, settleDelay: 0) { nil }
        XCTAssertFalse(started)
        XCTAssertEqual(recorder.log.filter { $0 == "register" }.count, 1)
    }

    func testRepairGivesUpAfterItsAttempts() async throws {
        let repaired = try await controller(status: .enabled)
            .repairRegistration(attempts: 2, startTimeout: 0.1, settleDelay: 0) { nil }
        XCTAssertFalse(repaired)
        XCTAssertEqual(recorder.log.filter { $0 == "register" }.count, 2)
    }

    func testRepairStopsAsSoonAsTheDaemonAnswers() async throws {
        let repaired = try await controller(status: .enabled)
            .repairRegistration(settleDelay: 0, daemon: startsOnceRegistered)
        XCTAssertTrue(repaired)
        XCTAssertEqual(recorder.log.filter { $0 == "register" }.count, 1)
    }

    func testDetectsFailingSpawnFromLaunchctl() {
        let failing = controller(output: "\tlast exit reason = OS_REASON_CODESIGNING\n\tjob state = spawn failed\n")
        XCTAssertTrue(failing.isSpawnFailing)
        XCTAssertEqual(recorder.log, ["/bin/launchctl print \(target)"])

        XCTAssertFalse(controller(output: "\tstate = running\n\tjob state = running\n").isSpawnFailing)
        // An unknown service (launchctl fails) is not a failing spawn.
        XCTAssertFalse(controller(exitCodes: ["launchctl": 113], output: "Could not find service").isSpawnFailing)
    }

    func testReadsBundledDaemonVersion() {
        XCTAssertEqual(controller(output: "1.2.3\n").bundledDaemonVersion, "1.2.3")
        XCTAssertEqual(recorder.log, ["\(daemon.path) version"])
        XCTAssertNil(controller(output: "\n").bundledDaemonVersion)
        XCTAssertNil(controller(exitCodes: ["tabtenderd": 1]).bundledDaemonVersion)
        XCTAssertNil(controller(daemonURL: .some(nil)).bundledDaemonVersion)
    }

    func testUnregisterOnlyUnregisters() throws {
        try controller().unregister()
        XCTAssertEqual(recorder.log, ["unregister"])
    }

    func testReportsRegistrationStatus() {
        XCTAssertEqual(controller(status: .requiresApproval).registrationStatus, .requiresApproval)
    }

    func testWaitForExitReturnsOncePidIsGone() async {
        let checks = Recorder()
        let controller = controller(processExists: { pid in
            checks.record("\(pid)")
            return checks.log.count < 3
        })
        let exited = await controller.waitForExit(of: 4242, timeout: 5)
        XCTAssertTrue(exited)
        XCTAssertEqual(checks.log, ["4242", "4242", "4242"])
    }

    func testWaitForExitTimesOut() async {
        let exited = await controller(processExists: { _ in true }).waitForExit(of: 4242, timeout: 0.3)
        XCTAssertFalse(exited)
    }

    func testProcessCommandRunnerCapturesOutputAndExitCode() throws {
        let result = try ProcessCommandRunner().run(
            URL(fileURLWithPath: "/bin/sh"), arguments: ["-c", "echo out; echo err >&2; exit 3"]
        )
        XCTAssertEqual(result.exitCode, 3)
        XCTAssertEqual(result.output, "out\nerr")
    }

    func testProcessCommandRunnerKillsACommandThatHangs() {
        let started = Date()
        XCTAssertThrowsError(
            try ProcessCommandRunner(timeout: 0.3).run(URL(fileURLWithPath: "/bin/sleep"), arguments: ["30"])
        ) { error in
            XCTAssertEqual(error as? ServiceError, .timedOut(command: "sleep 30"))
            XCTAssertEqual(error.localizedDescription, "sleep 30 did not finish in time.")
        }
        XCTAssertLessThan(Date().timeIntervalSince(started), 10)
    }

    func testProcessCommandRunnerReportsMissingExecutable() {
        XCTAssertThrowsError(try ProcessCommandRunner().run(URL(fileURLWithPath: "/nonexistent/tool"), arguments: [])) { error in
            guard case ServiceError.launchFailed(executable: "/nonexistent/tool", reason: _) = error else {
                return XCTFail("unexpected error \(error)")
            }
        }
    }
}
