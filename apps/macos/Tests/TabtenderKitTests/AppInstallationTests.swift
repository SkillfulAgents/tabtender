import XCTest
@testable import TabtenderKit

final class AppInstallationTests: XCTestCase {
    private var directory: URL!

    override func setUpWithError() throws {
        directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("tabtender-tests-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    }

    override func tearDown() {
        try? FileManager.default.removeItem(at: directory)
    }

    /// Creates an app bundle skeleton, optionally with the launch agent plist.
    private func makeBundle(withLaunchAgent: Bool) throws -> URL {
        let bundle = directory.appendingPathComponent("Tabtender.app")
        let launchAgent = bundle.appendingPathComponent(AppInstallation.launchAgentPath)
        try FileManager.default.createDirectory(
            at: launchAgent.deletingLastPathComponent(), withIntermediateDirectories: true
        )
        if withLaunchAgent {
            try Data().write(to: launchAgent)
        }
        return bundle
    }

    func testLaunchAgentLivesInTheBundlesLibrary() {
        XCTAssertEqual(
            AppInstallation.launchAgentPath,
            "Contents/Library/LaunchAgents/com.skillfulagents.tabtender.daemon.plist"
        )
    }

    func testBundleWithLaunchAgentIsRegistrable() throws {
        XCTAssertEqual(AppInstallation.inspect(bundleURL: try makeBundle(withLaunchAgent: true)), .registrable)
    }

    func testBundleWithoutLaunchAgentIsMissingIt() throws {
        XCTAssertEqual(AppInstallation.inspect(bundleURL: try makeBundle(withLaunchAgent: false)), .missingLaunchAgent)
        XCTAssertEqual(AppInstallation.inspect(bundleURL: directory), .missingLaunchAgent)
    }

    func testPermanentLocations() {
        for path in ["/Applications/Tabtender.app", "/Users/me/Applications/Tabtender.app", "/Users/me/Volumes/Tabtender.app"] {
            XCTAssertFalse(AppInstallation.isTemporaryLocation(path: path, onReadOnlyVolume: false), path)
        }
    }

    func testTemporaryLocations() {
        let translocated =
            "/private/var/folders/ab/cd/T/AppTranslocation/0A1B2C3D-0000-0000-0000-000000000000/d/Tabtender.app"
        XCTAssertTrue(AppInstallation.isTemporaryLocation(path: translocated, onReadOnlyVolume: false))
        XCTAssertTrue(AppInstallation.isTemporaryLocation(path: "/Volumes/Tabtender/Tabtender.app", onReadOnlyVolume: false))
        XCTAssertTrue(AppInstallation.isTemporaryLocation(path: "/Users/me/mnt/Tabtender.app", onReadOnlyVolume: true))
    }
}
