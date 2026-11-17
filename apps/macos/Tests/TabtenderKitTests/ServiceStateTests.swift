import XCTest
@testable import TabtenderKit

final class ServiceStateTests: XCTestCase {
    private func state(
        _ daemon: DaemonReachability,
        _ registration: ServiceRegistrationStatus,
        _ installation: AppInstallation = .registrable
    ) -> ServiceState {
        ServiceState(daemon: daemon, registration: registration, installation: installation)
    }

    func testANeverRegisteredAgentInTheBundleCanBeRegistered() {
        // SMAppService reports an agent that was never registered as not found.
        XCTAssertEqual(state(.notRunning, .notFound), .notRegistered)
        XCTAssertEqual(state(.notRunning, .notRegistered), .notRegistered)
        XCTAssertEqual(state(.notRunning, .notFound).firstLaunchStep, .register)
    }

    func testWithoutALaunchAgentTheServiceIsUnavailable() {
        XCTAssertEqual(state(.notRunning, .notFound, .missingLaunchAgent), .unavailable)
        XCTAssertEqual(state(.notRunning, .notRegistered, .missingLaunchAgent), .unavailable)
        XCTAssertEqual(ServiceState.unavailable.firstLaunchStep, .postpone)
    }

    func testATemporaryLocationNeedsRelocationBeforeRegistering() {
        XCTAssertEqual(state(.notRunning, .notFound, .temporaryLocation), .needsRelocation)
        XCTAssertEqual(state(.notRunning, .notRegistered, .temporaryLocation), .needsRelocation)
        XCTAssertEqual(ServiceState.needsRelocation.firstLaunchStep, .postpone)
    }

    func testAnExistingRegistrationIsReportedWhereverTheAppRunsFrom() {
        for installation in [AppInstallation.registrable, .temporaryLocation, .missingLaunchAgent] {
            XCTAssertEqual(state(.notRunning, .enabled, installation), .stopped)
            XCTAssertEqual(state(.notRunning, .requiresApproval, installation), .requiresApproval)
            XCTAssertEqual(state(.running, .notFound, installation), .running)
        }
    }

    func testAnUnresponsiveDaemonIsNotReportedAsStopped() {
        XCTAssertEqual(state(.unresponsive(reason: "busy"), .enabled), .unresponsive(reason: "busy"))
        XCTAssertEqual(state(.unresponsive(reason: "busy"), .notFound), .unresponsive(reason: "busy"))
    }

    func testFirstLaunchLeavesAnExistingRegistrationAlone() {
        for state in [ServiceState.running, .unresponsive(reason: "busy"), .stopped, .requiresApproval] {
            XCTAssertEqual(state.firstLaunchStep, .finish)
        }
    }

    func testOnlyAMissingSocketMeansTheDaemonIsNotRunning() {
        XCTAssertEqual(DaemonReachability(failure: ManagementError.notRunning), .notRunning)
        XCTAssertEqual(
            DaemonReachability(failure: ManagementError.timedOut),
            .unresponsive(reason: "The Tabtender service did not respond in time.")
        )
        XCTAssertEqual(
            DaemonReachability(failure: ManagementError.invalidResponse("GET /mgmt/v1/status: no pid")),
            .unresponsive(reason: "The Tabtender service sent an unexpected response: GET /mgmt/v1/status: no pid")
        )
        XCTAssertEqual(
            DaemonReachability(failure: ManagementError.transport("Connection reset by peer")),
            .unresponsive(reason: "Could not reach the Tabtender service: Connection reset by peer")
        )
    }
}
