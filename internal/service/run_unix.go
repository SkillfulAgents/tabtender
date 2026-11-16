//go:build !windows

package service

import (
	"context"
	"os/signal"
	"syscall"
)

// IsWindowsService reports whether the process runs under the Windows SCM.
func IsWindowsService() bool { return false }

// Run executes the daemon until it finishes or receives SIGINT/SIGTERM
// (launchd stops agents with SIGTERM).
func Run(run RunFunc) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}
