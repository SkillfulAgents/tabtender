import XCTest
@testable import TabtenderKit

final class ManagementSocketTests: XCTestCase {
    private let home = URL(fileURLWithPath: "/Users/someone")

    func testDefaultsToApplicationSupport() {
        XCTAssertEqual(
            ManagementSocket.resolvePath(environment: [:], homeDirectory: home),
            "/Users/someone/Library/Application Support/Tabtender/tabtenderd.sock"
        )
    }

    func testExplicitSocketPathWins() {
        let environment = ["TABTENDER_MGMT_SOCKET": "/tmp/tt.sock", "TABTENDER_DATA_DIR": "/data"]
        XCTAssertEqual(ManagementSocket.resolvePath(environment: environment, homeDirectory: home), "/tmp/tt.sock")
    }

    func testFollowsDataDirectory() {
        XCTAssertEqual(
            ManagementSocket.resolvePath(environment: ["TABTENDER_DATA_DIR": "/data"], homeDirectory: home),
            "/data/tabtenderd.sock"
        )
    }

    func testEmptyVariablesAreIgnored() {
        let environment = ["TABTENDER_MGMT_SOCKET": "", "TABTENDER_DATA_DIR": ""]
        XCTAssertEqual(
            ManagementSocket.resolvePath(environment: environment, homeDirectory: home),
            "/Users/someone/Library/Application Support/Tabtender/tabtenderd.sock"
        )
    }

    func testDaemonLocatorPrefersEnvironmentThenSibling() {
        let executable = URL(fileURLWithPath: "/Applications/Tabtender.app/Contents/MacOS/Tabtender")
        XCTAssertEqual(
            DaemonLocator.locate(environment: [:], executableURL: executable)?.path,
            "/Applications/Tabtender.app/Contents/MacOS/tabtenderd"
        )
        XCTAssertEqual(
            DaemonLocator.locate(environment: ["TABTENDERD_PATH": "/opt/tabtenderd"], executableURL: executable)?.path,
            "/opt/tabtenderd"
        )
        XCTAssertNil(DaemonLocator.locate(environment: [:], executableURL: nil))
    }
}
