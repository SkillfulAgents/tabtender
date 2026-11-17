//go:build !windows

package platform

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Prepare configures cmd so that the process it starts leads its own process
// group. Browsers spawn many helpers; a dedicated group lets KillTree stop
// all of them at once and shields them from signals aimed at the daemon.
func Prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillTree forcibly terminates the process and its descendants. The process
// must have been started via Prepare (so it leads a process group).
func KillTree(pid int) error {
	if pid <= 1 {
		return errors.New("refusing to kill invalid pid")
	}
	groupErr := syscall.Kill(-pid, syscall.SIGKILL)
	procErr := syscall.Kill(pid, syscall.SIGKILL)
	if groupErr == nil || procErr == nil || errors.Is(procErr, syscall.ESRCH) {
		return nil
	}
	return procErr
}

// ContainDescendants is a no-op on Unix, where orphaned browsers are reclaimed
// by reconciliation at the next daemon start.
func ContainDescendants() error { return nil }

// InheritPipe hands the child process the two pipe ends of a browser
// debugging pipe (commands are read from in, messages written to out) and
// returns the browser flags that select them. Chrome expects them as file
// descriptors 3 and 4.
func InheritPipe(cmd *exec.Cmd, in, out *os.File) ([]string, error) {
	cmd.ExtraFiles = []*os.File{in, out}
	return []string{"--remote-debugging-pipe"}, nil
}
