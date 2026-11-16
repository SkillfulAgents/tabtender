import XCTest
@testable import TabtenderKit

final class ModelDecodingTests: XCTestCase {
    func testParsesGoTimestamps() throws {
        let whole = try XCTUnwrap(RFC3339.date(from: "2026-11-16T10:00:00Z"))
        XCTAssertEqual(whole.timeIntervalSince1970, 1_794_823_200)

        let nanos = try XCTUnwrap(RFC3339.date(from: "2026-11-16T10:00:00.123456789Z"))
        XCTAssertEqual(nanos.timeIntervalSince(whole), 0.123456789, accuracy: 1e-6)

        let short = try XCTUnwrap(RFC3339.date(from: "2026-11-16T10:00:00.5Z"))
        XCTAssertEqual(short.timeIntervalSince(whole), 0.5, accuracy: 1e-6)

        let offset = try XCTUnwrap(RFC3339.date(from: "2026-11-16T12:00:00.25+02:00"))
        XCTAssertEqual(offset.timeIntervalSince(whole), 0.25, accuracy: 1e-6)

        XCTAssertNil(RFC3339.date(from: "yesterday"))
        XCTAssertNil(RFC3339.date(from: "2026-11-16T10:00:00.Z"))
    }

    func testDecodesSessionWithOptionalFields() throws {
        let json = """
        {"id":"s1","status":"running","headless":true,"createdAt":"2026-11-16T10:00:00.1Z",
         "expiresAt":"2026-11-16T11:00:00.1Z","cdpUrl":"ws://127.0.0.1:3000/v1/sessions/s1/cdp","connectedClients":2}
        """
        let session = try JSONDecoder.management().decode(Session.self, from: Data(json.utf8))
        XCTAssertEqual(session.id, "s1")
        XCTAssertNil(session.profile)
        XCTAssertNil(session.endedAt)
        XCTAssertTrue(session.isActive)
        XCTAssertEqual(session.expiresAt.timeIntervalSince(session.createdAt), 3600, accuracy: 1e-6)
    }

    func testUnknownTunnelStateDoesNotFailDecoding() throws {
        let json = #"{"provider":"future","state":"hibernating","url":"","error":""}"#
        let status = try JSONDecoder.management().decode(TunnelStatus.self, from: Data(json.utf8))
        XCTAssertEqual(status.state, .unknown)
    }

    func testConfigurationRoundTripsAndToleratesNullExtraArgs() throws {
        let json = """
        {"server":{"host":"127.0.0.1","port":3000,"publicUrl":""},
         "sessions":{"defaultTTL":3600,"maxTTL":14400,"maxConcurrent":8},
         "browser":{"runtime":"chrome","executablePath":"auto","headless":false,"extraArgs":null},
         "tunnel":{"provider":"none","cloudflare":{"executablePath":"auto","hostname":""}},
         "app":{"startOnLogin":true,"preventSleep":true}}
        """
        var config = try JSONDecoder().decode(Configuration.self, from: Data(json.utf8))
        XCTAssertEqual(config.browser.extraArgs, [])

        config.tunnel.provider = TunnelProvider.cloudflareQuick.rawValue
        let reencoded = try JSONSerialization.jsonObject(with: JSONEncoder().encode(config)) as? [String: Any]
        XCTAssertEqual(Set(try XCTUnwrap(reencoded).keys), ["server", "sessions", "browser", "tunnel", "app"])
        XCTAssertEqual((reencoded?["tunnel"] as? [String: Any])?["provider"] as? String, "cloudflare-quick")
        XCTAssertEqual((reencoded?["browser"] as? [String: Any])?["extraArgs"] as? [String], [])
    }
}
