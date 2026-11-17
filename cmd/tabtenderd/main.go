// Command tabtenderd is the Tabtender browser session server.
//
// Usage:
//
//	tabtenderd [run] [--launchd]      run the service (default)
//	tabtenderd version                print the version
//	tabtenderd service request-start  allow the next launchd start (macOS app)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/daemon"
	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
	"github.com/SkillfulAgents/tabtender/internal/service"
	"github.com/SkillfulAgents/tabtender/internal/version"
)

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tabtenderd:", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	command := "run"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}
	switch command {
	case "run":
		return runCommand(args)
	case "version":
		fmt.Println(version.Version)
		return nil
	case "service":
		return serviceCommand(args)
	case daemon.DisclaimExecCommand:
		// Internal: used by the daemon itself to start browsers on macOS.
		if len(args) == 0 {
			return fmt.Errorf("usage: tabtenderd %s <program> [arguments...]", daemon.DisclaimExecCommand)
		}
		return platform.DisclaimExec(args[0], args, os.Environ())
	default:
		return fmt.Errorf("unknown command %q (expected run, version, or service)", command)
	}
}

func runCommand(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	launchd := flags.Bool("launchd", false, "running under launchd: honor app.startOnLogin")
	if err := flags.Parse(args); err != nil {
		return err
	}
	// A Windows Service has no console to write to; the log file is the
	// only sink there.
	var stderr io.Writer = os.Stderr
	if service.IsWindowsService() {
		stderr = nil
	}
	if *launchd {
		// launchd discards standard error. Keep what the runtime prints
		// when the daemon dies (the log file has everything else).
		stderr = nil
		if paths, err := config.ResolvePaths(nil); err == nil {
			logDir := filepath.Dir(paths.LogFile())
			if os.MkdirAll(logDir, 0o700) == nil {
				_ = platform.RedirectStderr(filepath.Join(logDir, "tabtenderd.crash.log"))
			}
		}
	}
	return service.Run(func(ctx context.Context) error {
		return daemon.Run(ctx, daemon.Options{Launchd: *launchd, Stderr: stderr})
	})
}

func serviceCommand(args []string) error {
	if len(args) != 1 || args[0] != "request-start" {
		return fmt.Errorf("usage: tabtenderd service request-start")
	}
	paths, err := config.ResolvePaths(nil)
	if err != nil {
		return err
	}
	return service.RequestManualStart(paths.StateDir())
}
