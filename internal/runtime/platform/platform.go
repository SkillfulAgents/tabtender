// Package platform contains the OS-specific process handling used by browser
// runtimes and by session reconciliation.
package platform

import (
	"os"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// ChildEnv returns the environment for helper processes (browsers, tunnel
// clients): the daemon's own environment without any TABTENDER_* variable,
// so credentials such as TABTENDER_API_KEY never reach a child process.
func ChildEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), "TABTENDER_") {
			out = append(out, kv)
		}
	}
	return out
}

// Process is a snapshot of a running OS process.
type Process struct {
	PID  int
	Args []string
}

// Alive reports whether a process with the given PID exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	ok, err := process.PidExists(int32(pid))
	return err == nil && ok
}

// Args returns the command line of the process, or ok=false when the process
// does not exist or cannot be inspected.
func Args(pid int) (args []string, ok bool) {
	if pid <= 0 {
		return nil, false
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return nil, false
	}
	args, err = p.CmdlineSlice()
	if err != nil || len(args) == 0 {
		return nil, false
	}
	return args, true
}

// List returns every process whose command line can be inspected.
func List() ([]Process, error) {
	pids, err := process.Pids()
	if err != nil {
		return nil, err
	}
	procs := make([]Process, 0, len(pids))
	for _, pid := range pids {
		if args, ok := Args(int(pid)); ok {
			procs = append(procs, Process{PID: int(pid), Args: args})
		}
	}
	return procs, nil
}
