package sessions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
)

// ProcessTable is the view of OS processes that reconciliation needs.
type ProcessTable interface {
	// Args returns a process command line, or ok=false if it is gone.
	Args(pid int) (args []string, ok bool)
	// List returns all inspectable processes.
	List() ([]platform.Process, error)
	// KillTree forcibly terminates a process and its descendants.
	KillTree(pid int) error
}

// OSProcesses is the ProcessTable backed by the operating system.
type OSProcesses struct{}

// Args implements ProcessTable.
func (OSProcesses) Args(pid int) ([]string, bool) { return platform.Args(pid) }

// List implements ProcessTable.
func (OSProcesses) List() ([]platform.Process, error) { return platform.List() }

// KillTree implements ProcessTable.
func (OSProcesses) KillTree(pid int) error { return platform.KillTree(pid) }

// ReconcileReport summarizes what startup reconciliation did.
type ReconcileReport struct {
	// RecordedSessions is the number of sessions left in the journal by the
	// previous daemon run.
	RecordedSessions int
	// BrowsersTerminated counts leftover browser processes that were
	// validated as ours and killed.
	BrowsersTerminated int
	// ForeignProcessesSkipped counts recorded PIDs now belonging to an
	// unrelated process; these are never touched.
	ForeignProcessesSkipped int
	// ProfilesRemoved counts ephemeral profile directories that were deleted.
	ProfilesRemoved int
}

// Reconcile restores a clean slate after a daemon restart. Sessions do not
// survive the daemon, so the outcome is always the same: every browser that
// provably belongs to this Tabtender instance is terminated, ephemeral
// profiles are deleted, named profiles are kept, and the journal is cleared.
//
// A process is only ever killed after ownership is validated: its command
// line must reference a profile directory inside this instance's private
// profiles directory. A recorded PID alone is never trusted, because PIDs are
// reused by the operating system.
//
// Reconcile must run before any session is created.
func (m *Manager) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var report ReconcileReport
	records, err := m.journal.Load()
	if err != nil {
		// A corrupt journal must not block startup: the process scan below
		// finds our browsers without it.
		m.logger.Warn("session journal unreadable; relying on process scan", "error", err)
	}
	report.RecordedSessions = len(records)

	killed := map[int]bool{}
	kill := func(pid int, why string) {
		if killed[pid] {
			return
		}
		killed[pid] = true
		if err := m.procs.KillTree(pid); err != nil {
			m.logger.Warn("failed to terminate leftover browser", "pid", pid, "error", err)
			return
		}
		report.BrowsersTerminated++
		m.logger.Info("terminated leftover browser", "pid", pid, "source", why)
	}

	for _, rec := range records {
		args, alive := m.procs.Args(rec.PID)
		switch {
		case !alive:
		case referencesDir(args, rec.ProfileDir):
			kill(rec.PID, "journal")
		default:
			report.ForeignProcessesSkipped++
			m.logger.Info("recorded pid now belongs to another process; leaving it alone",
				"session", rec.ID, "pid", rec.PID)
		}
	}

	// Catch browsers the journal does not know about (for example when the
	// daemon died between launching a browser and recording it).
	procs, err := m.procs.List()
	if err != nil {
		m.logger.Warn("process scan failed", "error", err)
	}
	for _, p := range procs {
		if p.PID != os.Getpid() && referencesDirUnder(p.Args, m.profilesDir) {
			kill(p.PID, "process scan")
		}
	}

	if len(killed) > 0 {
		m.waitGone(ctx, killed)
	}

	removed, err := m.removeEphemeralProfiles()
	report.ProfilesRemoved = removed
	if err != nil {
		return report, err
	}
	if err := m.journal.Save(nil); err != nil {
		return report, fmt.Errorf("clear session journal: %w", err)
	}
	return report, nil
}

func (m *Manager) waitGone(ctx context.Context, pids map[int]bool) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		remaining := false
		for pid := range pids {
			if _, alive := m.procs.Args(pid); alive {
				remaining = true
				break
			}
		}
		if !remaining {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (m *Manager) removeEphemeralProfiles() (int, error) {
	root := m.ephemeralRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list ephemeral profiles: %w", err)
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		if err := removeAllRetry(filepath.Join(root, e.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// removeAllRetry deletes a directory tree, retrying briefly: right after a
// browser is killed the OS can still hold files open (notably on Windows),
// and helper processes may write a few last files.
func removeAllRetry(dir string) error {
	var err error
	for attempt := 0; attempt < 40; attempt++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("remove %s: %w", dir, err)
}

// flagValue returns the value of a "--flag=value" style argument.
func flagValue(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", false
	}
	_, value, ok := strings.Cut(arg, "=")
	return value, ok && value != ""
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// referencesDir reports whether a command line carries dir as a flag value
// (as in --user-data-dir=<dir>).
func referencesDir(args []string, dir string) bool {
	for _, arg := range args {
		if value, ok := flagValue(arg); ok && samePath(value, dir) {
			return true
		}
	}
	return false
}

// referencesDirUnder reports whether a command line carries, as a flag value,
// a path strictly inside root.
func referencesDirUnder(args []string, root string) bool {
	root = filepath.Clean(root)
	for _, arg := range args {
		value, ok := flagValue(arg)
		if !ok || !filepath.IsAbs(value) {
			continue
		}
		for dir := filepath.Dir(filepath.Clean(value)); ; dir = filepath.Dir(dir) {
			if samePath(dir, root) {
				return true
			}
			if parent := filepath.Dir(dir); parent == dir {
				break
			}
		}
	}
	return false
}
