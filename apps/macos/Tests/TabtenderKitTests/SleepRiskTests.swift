import XCTest
@testable import TabtenderKit

final class SleepRiskTests: XCTestCase {
    private static let laptop = """
    Battery Power:
     Sleep On Power Button 1
     standby              1
     displaysleep         2
     sleep                1
     disksleep            10
    AC Power:
     Sleep On Power Button 1
     displaysleep         10
     sleep                15
     disksleep            10
    """

    private static let desktop = """
    AC Power:
     Sleep On Power Button 1
     autorestartatconnect 0
     displaysleep         5
     sleep                0
     disksleep            10
    """

    func testReadsTheSleepTimerOfEachPowerSource() {
        XCTAssertEqual(
            PowerSettings(pmsetCustom: Self.laptop),
            PowerSettings(sleepOnPowerAdapter: 15, sleepOnBattery: 1)
        )
        let desktop = PowerSettings(pmsetCustom: Self.desktop, automaticLogoutSeconds: 3600)
        XCTAssertEqual(desktop, PowerSettings(sleepOnPowerAdapter: 0, sleepOnBattery: nil, automaticLogout: 60))
        XCTAssertFalse(desktop.hasBattery)
        XCTAssertEqual(PowerSettings(pmsetCustom: "", automaticLogoutSeconds: 30).automaticLogout, 1)
    }

    func testSystemSleepTimersOnlyMatterUntilTheServiceKeepsTheMacAwake() {
        let settings = PowerSettings(sleepOnPowerAdapter: 15, sleepOnBattery: 1)
        let idle: [SleepRisk] = [.sleepsOnPowerAdapter(minutes: 15), .sleepsOnBattery(minutes: 1)]
        XCTAssertEqual(SleepRisk.risks(settings: settings, prevention: .never), idle)
        XCTAssertEqual(SleepRisk.risks(settings: settings, prevention: .whileSessionsAreActive), idle)
        XCTAssertEqual(SleepRisk.risks(settings: settings, prevention: .always), [])
    }

    func testAMacThatNeverSleepsHasNoRisks() {
        let settings = PowerSettings(pmsetCustom: Self.desktop)
        XCTAssertEqual(SleepRisk.risks(settings: settings, prevention: .never), [])
    }

    func testAutomaticLogoutIsARiskWhateverTheServiceDoes() {
        let settings = PowerSettings(automaticLogout: 60)
        XCTAssertEqual(
            SleepRisk.risks(settings: settings, prevention: .always),
            [.logsOutAutomatically(minutes: 60)]
        )
    }

    func testEachRiskPointsAtTheSettingThatRemovesIt() {
        let adapter = SleepRisk.sleepsOnPowerAdapter(minutes: 15)
        XCTAssertEqual(adapter.settingsPane(hasBattery: true, macOSMajorVersion: 15), .batteryOptions)
        XCTAssertEqual(adapter.settingsPane(hasBattery: false, macOSMajorVersion: 15), .energySaver)
        XCTAssertEqual(adapter.settingsPane(hasBattery: false, macOSMajorVersion: 26), .energy)
        XCTAssertNil(SleepRisk.sleepsOnBattery(minutes: 1).settingsPane(hasBattery: true, macOSMajorVersion: 26))
        XCTAssertEqual(
            SleepRisk.logsOutAutomatically(minutes: 5).settingsPane(hasBattery: true, macOSMajorVersion: 26),
            .privacyAdvanced
        )
        XCTAssertEqual(SystemSettingsPane.energy.buttonTitle, "Open Energy Settings…")
        XCTAssertEqual(
            SystemSettingsPane.batteryOptions.url.absoluteString,
            "x-apple.systempreferences:com.apple.Battery-Settings.extension?options"
        )
    }

    func testSummariesSayWhenTheMacSleeps() {
        XCTAssertEqual(
            SleepRisk.sleepsOnPowerAdapter(minutes: 1).summary(prevention: .whileSessionsAreActive),
            "On the power adapter, this Mac goes to sleep after 1 minute without activity between sessions. "
                + "The service is unreachable while it sleeps."
        )
        XCTAssertTrue(
            SleepRisk.logsOutAutomatically(minutes: 120).summary(prevention: .always).contains("after 2 hours")
        )
        XCTAssertFalse(
            SleepRisk.sleepsOnBattery(minutes: 10).summary(prevention: .never).contains("between sessions")
        )
    }

    func testReaderCombinesPmsetAndTheLogoutPreference() throws {
        let preferences = FileManager.default.temporaryDirectory
            .appendingPathComponent("tabtender-\(UUID().uuidString).plist")
        defer { try? FileManager.default.removeItem(at: preferences) }
        XCTAssertTrue(NSDictionary(dictionary: ["com.apple.autologout.AutoLogOutDelay": 600]).write(to: preferences, atomically: true))

        let reader = PowerSettingsReader(
            runner: StubRunner(result: CommandResult(exitCode: 0, output: Self.laptop)),
            globalPreferences: preferences
        )
        XCTAssertEqual(
            reader.read(),
            PowerSettings(sleepOnPowerAdapter: 15, sleepOnBattery: 1, automaticLogout: 10)
        )

        let failing = PowerSettingsReader(runner: StubRunner(result: CommandResult(exitCode: 1)))
        XCTAssertNil(failing.read())
    }

    func testReaderWorksAgainstTheRealSystem() {
        XCTAssertNotNil(PowerSettingsReader().read())
    }

    func testOnlyAServiceBoundBeyondLoopbackServesOtherMachines() throws {
        let json = """
        {"server":{"host":"127.0.0.1","port":3000,"publicUrl":""},
         "sessions":{"defaultTTL":3600,"maxTTL":14400,"maxConcurrent":8},
         "browser":{"runtime":"chrome","executablePath":"auto","headless":false,"extraArgs":[]},
         "tunnel":{"provider":"none","cloudflare":{"executablePath":"auto","hostname":""}},
         "app":{"startOnLogin":true,"preventSleep":true,"keepAwake":false}}
        """
        var config = try JSONDecoder().decode(Configuration.self, from: Data(json.utf8))
        XCTAssertFalse(config.servesOtherMachines)
        config.server.host = "0.0.0.0"
        XCTAssertTrue(config.servesOtherMachines)
        config.server.host = "localhost"
        config.tunnel.provider = TunnelProvider.cloudflareQuick.rawValue
        XCTAssertTrue(config.servesOtherMachines)
    }
}

private struct StubRunner: CommandRunner {
    let result: CommandResult

    func run(_ executable: URL, arguments: [String]) throws -> CommandResult {
        result
    }
}
