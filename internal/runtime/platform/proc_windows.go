package platform

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Prepare configures cmd so the browser does not share the daemon's console
// control events.
func Prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// KillTree forcibly terminates the process and its descendants.
func KillTree(pid int) error {
	if pid <= 0 {
		return errors.New("refusing to kill invalid pid")
	}
	out, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil && Alive(pid) {
		return fmt.Errorf("taskkill: %w: %s", err, out)
	}
	return nil
}

// ContainDescendants places the daemon in a job object configured to kill
// every member when the daemon exits, so browsers can never outlive it —
// even if the daemon is terminated abruptly.
func ContainDescendants() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("configure job object: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("assign job object: %w", err)
	}
	// The handle is intentionally leaked: it must stay open for the lifetime
	// of the daemon and is closed by the OS at exit, which triggers the kill.
	return nil
}
