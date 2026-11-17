import XCTest
@testable import TabtenderKit

final class ConfigurationChangesTests: XCTestCase {
    private func configuration() throws -> Configuration {
        let json = """
        {"server":{"host":"127.0.0.1","port":3000,"publicUrl":""},
         "sessions":{"defaultTTL":3600,"maxTTL":14400,"maxConcurrent":8},
         "browser":{"runtime":"chrome","executablePath":"auto","headless":false,"extraArgs":[]},
         "tunnel":{"provider":"none","cloudflare":{"executablePath":"auto","hostname":""}},
         "app":{"startOnLogin":true,"preventSleep":true}}
        """
        return try JSONDecoder().decode(Configuration.self, from: Data(json.utf8))
    }

    private func changes(_ edited: Configuration, from base: Configuration) throws -> String {
        String(decoding: try edited.changes(from: base), as: UTF8.self)
    }

    func testAnUneditedConfigurationHasNoChanges() throws {
        let base = try configuration()
        XCTAssertEqual(try changes(base, from: base), "{}")
    }

    func testChangesHoldOnlyTheEditedSettings() throws {
        let base = try configuration()
        var edited = base
        edited.server.port = 4000
        edited.app.startOnLogin = false
        edited.tunnel.cloudflare.hostname = "browser.example.com"
        edited.browser.extraArgs = ["--lang=en"]
        XCTAssertEqual(
            try changes(edited, from: base),
            #"{"app":{"startOnLogin":false},"browser":{"extraArgs":["--lang=en"]},"#
                + #""server":{"port":4000},"tunnel":{"cloudflare":{"hostname":"browser.example.com"}}}"#
        )
    }

    func testRebasingKeepsEditsAndAdoptsWhatChangedUnderneath() throws {
        let base = try configuration()
        var edited = base
        edited.sessions.maxConcurrent = 4
        var newBase = base
        newBase.sessions.defaultTTL = 600
        newBase.browser.headless = true

        var expected = newBase
        expected.sessions.maxConcurrent = 4
        XCTAssertEqual(try edited.rebased(from: base, onto: newBase), expected)
    }

    func testRebasingAnEditThatWasSavedElsewhereLeavesNothingUnsaved() throws {
        let base = try configuration()
        var edited = base
        edited.server.port = 4000
        XCTAssertEqual(try edited.rebased(from: base, onto: edited), edited)
    }
}
