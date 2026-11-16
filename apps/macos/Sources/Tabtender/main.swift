import Foundation

// Service commands run headless for scripts and CI; anything else starts the
// menu bar app.
if let command = ServiceCommand(arguments: CommandLine.arguments) {
    Task.detached {
        exit(await command.run())
    }
    dispatchMain()
}

TabtenderApp.main()
