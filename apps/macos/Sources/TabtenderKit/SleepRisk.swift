import Foundation

/// The macOS settings that decide whether this Mac stays available to the
/// service: a sleeping Mac answers no requests, and logging the user out
/// stops their background services.
public struct PowerSettings: Equatable, Sendable {
    /// Minutes without activity before the Mac sleeps on the power adapter;
    /// zero means never.
    public var sleepOnPowerAdapter: Int
    /// The same on battery; `nil` on a Mac without one.
    public var sleepOnBattery: Int?
    /// Minutes without activity before the user is logged out; zero means
    /// never.
    public var automaticLogout: Int

    public init(sleepOnPowerAdapter: Int = 0, sleepOnBattery: Int? = nil, automaticLogout: Int = 0) {
        self.sleepOnPowerAdapter = sleepOnPowerAdapter
        self.sleepOnBattery = sleepOnBattery
        self.automaticLogout = automaticLogout
    }

    /// Reads the output of `pmset -g custom`: one block of `name value`
    /// lines per power source.
    public init(pmsetCustom output: String, automaticLogoutSeconds: Int = 0) {
        var sleepTimers: [String: Int] = [:]
        var source = ""
        for line in output.split(whereSeparator: \.isNewline) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasSuffix(":") {
                source = String(trimmed.dropLast())
                continue
            }
            let fields = trimmed.split(separator: " ", omittingEmptySubsequences: true)
            if fields.count == 2, fields[0] == "sleep", let minutes = Int(fields[1]) {
                sleepTimers[source] = minutes
            }
        }
        self.init(
            sleepOnPowerAdapter: sleepTimers["AC Power"] ?? 0,
            sleepOnBattery: sleepTimers["Battery Power"],
            // Rounded up: a delay under a minute is still a logout.
            automaticLogout: (max(automaticLogoutSeconds, 0) + 59) / 60
        )
    }

    public var hasBattery: Bool { sleepOnBattery != nil }
}

/// A macOS setting that will take the service offline, as far as this Mac's
/// settings show.
public enum SleepRisk: Equatable, Identifiable, Sendable {
    /// The Mac sleeps when idle on the power adapter.
    case sleepsOnPowerAdapter(minutes: Int)
    /// The Mac sleeps when idle on battery.
    case sleepsOnBattery(minutes: Int)
    /// macOS logs the user out when idle, which stops the service.
    case logsOutAutomatically(minutes: Int)

    public var id: String {
        switch self {
        case .sleepsOnPowerAdapter: return "sleepsOnPowerAdapter"
        case .sleepsOnBattery: return "sleepsOnBattery"
        case .logsOutAutomatically: return "logsOutAutomatically"
        }
    }

    /// The risks that remain given how the service itself prevents sleep.
    ///
    /// The service holds off idle sleep while sessions are active, or always;
    /// only in the latter case do the system's own sleep timers stop
    /// mattering. Nothing the service does prevents an automatic logout.
    public static func risks(settings: PowerSettings, prevention: Configuration.SleepPrevention) -> [SleepRisk] {
        var risks: [SleepRisk] = []
        if prevention != .always {
            if settings.sleepOnPowerAdapter > 0 {
                risks.append(.sleepsOnPowerAdapter(minutes: settings.sleepOnPowerAdapter))
            }
            if let minutes = settings.sleepOnBattery, minutes > 0 {
                risks.append(.sleepsOnBattery(minutes: minutes))
            }
        }
        if settings.automaticLogout > 0 {
            risks.append(.logsOutAutomatically(minutes: settings.automaticLogout))
        }
        return risks
    }

    /// What happens, in a sentence.
    public func summary(prevention: Configuration.SleepPrevention) -> String {
        let scope = prevention == .whileSessionsAreActive ? " between sessions" : ""
        switch self {
        case let .sleepsOnPowerAdapter(minutes):
            return "On the power adapter, this Mac goes to sleep after \(Self.duration(minutes)) "
                + "without activity\(scope). The service is unreachable while it sleeps."
        case let .sleepsOnBattery(minutes):
            return "On battery, this Mac goes to sleep after \(Self.duration(minutes)) "
                + "without activity\(scope). The service is unreachable while it sleeps."
        case let .logsOutAutomatically(minutes):
            return "macOS logs you out after \(Self.duration(minutes)) without activity, "
                + "which stops the service and ends its sessions."
        }
    }

    /// How to remove the risk.
    public func remedy(hasBattery: Bool) -> String {
        switch self {
        case .sleepsOnPowerAdapter:
            let setting = hasBattery
                ? "“Prevent automatic sleeping on power adapter when the display is off”"
                : "“Prevent automatic sleeping when the display is off”"
            return "Set “Keep this Mac awake” to Always, or turn on \(setting) in System Settings."
        case .sleepsOnBattery:
            return "Set “Keep this Mac awake” to Always. macOS has no setting of its own for this."
        case .logsOutAutomatically:
            return "Turn off “Log out automatically after inactivity” in System Settings."
        }
    }

    /// The System Settings pane holding the setting, if there is one.
    public func settingsPane(hasBattery: Bool, macOSMajorVersion: Int) -> SystemSettingsPane? {
        switch self {
        case .sleepsOnPowerAdapter:
            if hasBattery {
                return .batteryOptions
            }
            // Desktop Macs had a pane of their own before macOS 26.
            return macOSMajorVersion >= 26 ? .energy : .energySaver
        case .sleepsOnBattery:
            return nil
        case .logsOutAutomatically:
            return .privacyAdvanced
        }
    }

    private static func duration(_ minutes: Int) -> String {
        if minutes >= 60, minutes % 60 == 0 {
            return minutes == 60 ? "1 hour" : "\(minutes / 60) hours"
        }
        return minutes == 1 ? "1 minute" : "\(minutes) minutes"
    }
}

/// A place in System Settings the app can open.
public enum SystemSettingsPane: Equatable, Sendable {
    /// The Options sheet of the Battery pane on a Mac with a battery.
    case batteryOptions
    /// The Energy pane of a desktop Mac, which shares the Battery pane's
    /// identifier.
    case energy
    /// The Energy Saver pane of a desktop Mac before macOS 26.
    case energySaver
    /// The Advanced sheet of Privacy & Security.
    case privacyAdvanced

    public var url: URL {
        switch self {
        case .batteryOptions:
            return URL(string: "x-apple.systempreferences:com.apple.Battery-Settings.extension?options")!
        case .energy:
            return URL(string: "x-apple.systempreferences:com.apple.Battery-Settings.extension")!
        case .energySaver:
            return URL(string: "x-apple.systempreferences:com.apple.Energy-Saver-Settings.extension")!
        case .privacyAdvanced:
            return URL(string: "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension?Advanced")!
        }
    }

    /// The label of a button that opens the pane.
    public var buttonTitle: String {
        switch self {
        case .batteryOptions: return "Open Battery Settings…"
        case .energy, .energySaver: return "Open Energy Settings…"
        case .privacyAdvanced: return "Open Privacy & Security…"
        }
    }
}

/// Reads `PowerSettings` from the system.
public struct PowerSettingsReader: Sendable {
    private let runner: CommandRunner
    private let globalPreferences: URL

    public init(
        runner: CommandRunner = ProcessCommandRunner(timeout: 10),
        globalPreferences: URL = URL(fileURLWithPath: "/Library/Preferences/.GlobalPreferences.plist")
    ) {
        self.runner = runner
        self.globalPreferences = globalPreferences
    }

    /// `nil` when the settings cannot be read; nothing is reported then.
    public func read() -> PowerSettings? {
        let pmset = URL(fileURLWithPath: "/usr/bin/pmset")
        guard let result = try? runner.run(pmset, arguments: ["-g", "custom"]), result.exitCode == 0 else {
            return nil
        }
        let preferences = NSDictionary(contentsOf: globalPreferences)
        let logoutDelay = (preferences?["com.apple.autologout.AutoLogOutDelay"] as? NSNumber)?.intValue ?? 0
        return PowerSettings(pmsetCustom: result.output, automaticLogoutSeconds: logoutDelay)
    }
}

extension Configuration {
    /// Whether other machines are meant to reach the service, which is when
    /// this Mac going to sleep matters most.
    public var servesOtherMachines: Bool {
        if tunnel.provider != TunnelProvider.none.rawValue {
            return true
        }
        let host = server.host.lowercased()
        return !(host == "localhost" || host == "::1" || host.hasPrefix("127."))
    }
}
