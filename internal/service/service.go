// Package service integrates tabtenderd with the operating system's service
// managers: launchd on macOS and the Service Control Manager on Windows.
package service

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/host"
)

// Name is the service name registered with the Windows SCM.
const Name = "tabtenderd"

// RunFunc is the daemon body. It must return when ctx is canceled.
type RunFunc func(ctx context.Context) error

const manualStartFile = "manual-start"

// The manual-start marker implements "run now, but not at login" on macOS.
// launchd always launches a registered agent at login; when app.startOnLogin
// is off, the daemon exits immediately unless the user explicitly started it
// during the current boot. The marker records that request, stamped with the
// boot time so it cannot leak into the next boot.

// RequestManualStart records that the user asked for the service to run.
func RequestManualStart(stateDir string) error {
	boot, err := host.BootTime()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, manualStartFile), []byte(strconv.FormatUint(boot, 10)), 0o600)
}

// ManualStartRequested reports whether a manual start was requested during
// the current boot.
func ManualStartRequested(stateDir string) bool {
	data, err := os.ReadFile(filepath.Join(stateDir, manualStartFile))
	if err != nil {
		return false
	}
	boot, err := host.BootTime()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == strconv.FormatUint(boot, 10)
}

// ClearManualStart forgets the manual start request. It is called on orderly
// shutdown (stop, logout, restart) so the next login honors app.startOnLogin.
func ClearManualStart(stateDir string) {
	_ = os.Remove(filepath.Join(stateDir, manualStartFile))
}
