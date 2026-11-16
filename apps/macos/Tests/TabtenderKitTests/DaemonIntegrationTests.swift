import XCTest
@testable import TabtenderKit

/// Drives a real `tabtenderd` through `ManagementClient`.
final class DaemonIntegrationTests: DaemonTestCase {
    private func assertAPIError<T>(
        status: Int, code: String,
        _ operation: () async throws -> T,
        file: StaticString = #filePath, line: UInt = #line
    ) async -> String? {
        do {
            _ = try await operation()
            XCTFail("expected a \(code) error", file: file, line: line)
        } catch let ManagementError.api(actualStatus, actualCode, message) {
            XCTAssertEqual(actualStatus, status, file: file, line: line)
            XCTAssertEqual(actualCode, code, file: file, line: line)
            return message
        } catch {
            XCTFail("unexpected error \(error)", file: file, line: line)
        }
        return nil
    }

    /// Calls the public TCP API with a bearer token and returns the HTTP status.
    private func publicAPIStatus(port: Int, apiKey: String) async throws -> Int {
        var request = URLRequest(url: URL(string: "http://127.0.0.1:\(port)/v1/health")!)
        request.setValue("Bearer \(apiKey)", forHTTPHeaderField: "Authorization")
        let (_, response) = try await URLSession(configuration: .ephemeral).data(for: request)
        return try XCTUnwrap(response as? HTTPURLResponse).statusCode
    }

    // MARK: Status

    func testStatusAndHealth() async throws {
        let daemon = try await launchDaemon()

        let status = try await daemon.client.status()
        XCTAssertEqual(status.pid, daemon.pid)
        XCTAssertFalse(status.version.isEmpty)
        XCTAssertEqual(status.localUrl, "http://127.0.0.1:\(daemon.port)")
        XCTAssertEqual(status.publicUrl, "")
        XCTAssertEqual(status.sessions.active, 0)
        XCTAssertEqual(status.sessions.max, 8)
        XCTAssertEqual(status.tunnel.state, .disabled)
        XCTAssertEqual(status.tunnelTokenSource, "")
        XCTAssertEqual(status.pendingRestart, [])
        XCTAssertEqual(
            URL(fileURLWithPath: status.dataDir).resolvingSymlinksInPath(),
            daemon.dataDirectory.resolvingSymlinksInPath()
        )
        XCTAssertLessThan(abs(status.startedAt.timeIntervalSinceNow), 60)

        let health = try await daemon.client.health()
        XCTAssertEqual(health.version, status.version)
        XCTAssertEqual(health.isHealthy, health.browser.available)
        XCTAssertEqual(health.sessions, status.sessions)
    }

    // MARK: Configuration

    func testConfigEditsPersistAcrossDaemonRestart() async throws {
        let dataDirectory = try makeDataDirectory()
        let first = try await launchDaemon(dataDirectory: dataDirectory)

        let original = try await first.client.config()
        XCTAssertEqual(original.file.sessions.maxConcurrent, 8)
        XCTAssertEqual(original.restartRequiredKeys, ["server.host", "server.port"])
        XCTAssertEqual(original.envOverrides["server.port"]?.variable, "TABTENDER_PORT")

        var edited = original.file
        edited.sessions.defaultTTL = 600
        edited.sessions.maxConcurrent = 5
        edited.browser.headless = true
        edited.app.preventSleep = false
        edited.server.host = "localhost"
        let saved = try await first.client.updateConfig(edited)
        XCTAssertEqual(saved.file, edited)
        XCTAssertEqual(saved.effective.sessions.maxConcurrent, 5)
        XCTAssertEqual(saved.pendingRestart, ["server.host"])

        // Live-applied settings take effect immediately; restart-required
        // ones are reported as pending.
        let status = try await first.client.status()
        XCTAssertEqual(status.sessions.max, 5)
        XCTAssertEqual(status.pendingRestart, ["server.host"])

        let reread = try await first.client.config()
        XCTAssertEqual(reread.file, edited)

        first.stop()
        let second = try await launchDaemon(dataDirectory: dataDirectory)
        let persisted = try await second.client.config()
        XCTAssertEqual(persisted.file, edited)
        XCTAssertEqual(persisted.effective.sessions.defaultTTL, 600)
        XCTAssertEqual(persisted.effective.server.host, "localhost")
        XCTAssertEqual(persisted.pendingRestart, [])
        let restarted = try await second.client.status()
        XCTAssertEqual(restarted.sessions.max, 5)
    }

    func testInvalidConfigIsRejectedWithTypedError() async throws {
        let daemon = try await launchDaemon()
        let original = try await daemon.client.config()

        var invalid = original.file
        invalid.sessions.defaultTTL = 0
        invalid.tunnel.provider = "carrier-pigeon"
        let message = await assertAPIError(status: 400, code: "validation_error") {
            try await daemon.client.updateConfig(invalid)
        }
        XCTAssertTrue(message?.contains("sessions.defaultTTL") ?? false, message ?? "")
        XCTAssertTrue(message?.contains("carrier-pigeon") ?? false, message ?? "")

        let unchanged = try await daemon.client.config()
        XCTAssertEqual(unchanged.file, original.file)
    }

    func testEnvironmentOverridesAreReportedAndWin() async throws {
        let daemon = try await launchDaemon(environment: ["TABTENDER_MAX_SESSIONS": "3"])

        let original = try await daemon.client.config()
        XCTAssertEqual(
            original.envOverrides["sessions.maxConcurrent"],
            EnvOverride(variable: "TABTENDER_MAX_SESSIONS", value: "3")
        )
        XCTAssertEqual(original.effective.sessions.maxConcurrent, 3)
        XCTAssertEqual(original.file.sessions.maxConcurrent, 8)

        var edited = original.file
        edited.sessions.maxConcurrent = 6
        let saved = try await daemon.client.updateConfig(edited)
        XCTAssertEqual(saved.file.sessions.maxConcurrent, 6)
        XCTAssertEqual(saved.effective.sessions.maxConcurrent, 3)
        XCTAssertEqual(saved.envOverrides["sessions.maxConcurrent"]?.variable, "TABTENDER_MAX_SESSIONS")
        let status = try await daemon.client.status()
        XCTAssertEqual(status.sessions.max, 3)
    }

    // MARK: Secrets

    func testAPIKeyRotationInvalidatesOldKey() async throws {
        let daemon = try await launchDaemon()

        let original = try await daemon.client.apiKey()
        XCTAssertFalse(original.apiKey.isEmpty)
        XCTAssertTrue(original.rotatable)
        let fetchedAgain = try await daemon.client.apiKey()
        XCTAssertEqual(fetchedAgain, original)
        let before = try await publicAPIStatus(port: daemon.port, apiKey: original.apiKey)
        XCTAssertEqual(before, 200)

        let rotated = try await daemon.client.rotateAPIKey()
        XCTAssertNotEqual(rotated.apiKey, original.apiKey)
        XCTAssertFalse(rotated.apiKey.isEmpty)

        let oldKeyStatus = try await publicAPIStatus(port: daemon.port, apiKey: original.apiKey)
        XCTAssertEqual(oldKeyStatus, 401)
        let newKeyStatus = try await publicAPIStatus(port: daemon.port, apiKey: rotated.apiKey)
        XCTAssertEqual(newKeyStatus, 200)
    }

    func testEnvironmentPinnedAPIKeyCannotBeRotated() async throws {
        let pinnedKey = "pinned-test-key-0123456789abcdef0123456789"
        let daemon = try await launchDaemon(environment: ["TABTENDER_API_KEY": pinnedKey])

        let info = try await daemon.client.apiKey()
        XCTAssertEqual(info, APIKeyInfo(apiKey: pinnedKey, source: SecretSource.environment, rotatable: false))
        _ = await assertAPIError(status: 409, code: "environment_controlled") {
            try await daemon.client.rotateAPIKey()
        }
    }

    func testTunnelTokenIsStoredAndRemoved() async throws {
        let daemon = try await launchDaemon()

        let source = try await daemon.client.setTunnelToken("test-token")
        XCTAssertFalse(source.isEmpty)
        let stored = try await daemon.client.status()
        XCTAssertEqual(stored.tunnelTokenSource, source)

        let removed = try await daemon.client.setTunnelToken("")
        XCTAssertEqual(removed, "")

        let tunnel = try await daemon.client.restartTunnel()
        XCTAssertEqual(tunnel.state, .disabled)
    }

    // MARK: Sessions

    func testSessionLifecycle() async throws {
        let daemon = try await launchDaemon()
        let health = try await daemon.client.health()
        try XCTSkipUnless(health.browser.available, "no browser available: \(health.browser.error ?? "")")

        let created = try await daemon.client.createSession(CreateSessionRequest(ttlSeconds: 120, headless: true))
        XCTAssertFalse(created.id.isEmpty)
        XCTAssertTrue(created.isActive)
        XCTAssertTrue(created.headless)
        XCTAssertNil(created.profile)
        XCTAssertEqual(created.connectedClients, 0)
        // The TTL starts once the browser is ready, so the launch time is on top of it.
        let lifetime = created.expiresAt.timeIntervalSince(created.createdAt)
        XCTAssertTrue((120...210).contains(lifetime), "lifetime \(lifetime)")
        XCTAssertEqual(created.cdpUrl, "ws://127.0.0.1:\(daemon.port)/v1/sessions/\(created.id)/cdp")

        let listed = try await daemon.client.sessions()
        XCTAssertEqual(listed.filter(\.isActive).map(\.id), [created.id])
        let busy = try await daemon.client.status()
        XCTAssertEqual(busy.sessions.active, 1)

        let terminated = try await daemon.client.terminateSession(id: created.id)
        XCTAssertFalse(terminated.isActive)
        XCTAssertEqual(terminated.endReason, "deleted")
        XCTAssertNotNil(terminated.endedAt)

        let remaining = try await daemon.client.sessions()
        XCTAssertEqual(remaining.filter(\.isActive), [])
        let idle = try await daemon.client.status()
        XCTAssertEqual(idle.sessions.active, 0)

        _ = await assertAPIError(status: 404, code: "session_not_found") {
            try await daemon.client.terminateSession(id: "does-not-exist")
        }
    }

    // MARK: Logs

    func testLogsCanBeFilteredAndLimited() async throws {
        let daemon = try await launchDaemon()
        // Rotating the key writes a known INFO entry.
        _ = try await daemon.client.rotateAPIKey()

        let entries = try await daemon.client.logs()
        XCTAssertTrue(entries.contains { $0.message == "API key rotated" && $0.level == "INFO" })
        XCTAssertEqual(entries.map(\.time), entries.map(\.time).sorted())

        let limited = try await daemon.client.logs(limit: 1)
        XCTAssertEqual(limited.count, 1)
        XCTAssertEqual(limited.last, entries.last)

        let errors = try await daemon.client.logs(minimumLevel: .error)
        XCTAssertTrue(errors.allSatisfy { $0.level == "ERROR" })
        XCTAssertFalse(errors.contains { $0.message == "API key rotated" })
    }

    // MARK: Daemon down

    func testClientReportsNotRunningAfterDaemonStops() async throws {
        let daemon = try await launchDaemon()
        _ = try await daemon.client.status()
        daemon.stop()

        let started = Date()
        do {
            _ = try await daemon.client.status()
            XCTFail("expected notRunning")
        } catch {
            XCTAssertEqual(error as? ManagementError, .notRunning)
        }
        XCTAssertLessThan(Date().timeIntervalSince(started), 1)
    }
}
