package platform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
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
	if err == nil {
		return nil
	}
	// taskkill reports failure when any process of the tree could not be
	// terminated, which routinely happens for helpers that exit on their own
	// while the tree is being torn down. What matters is that the root is
	// gone; give the OS a moment to finish tearing it down.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !Alive(pid) {
			return nil
		}
	}
	return fmt.Errorf("taskkill: %w: %s", err, out)
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

// InheritPipe hands the child process the two pipe ends of a browser
// debugging pipe (commands are read from in, messages written to out) and
// returns the browser flags that select them. Windows has no numbered file
// descriptors to inherit, so the handles are made inheritable and named on
// the command line; an inherited handle keeps its value in the child.
// Prepare must have been called on cmd first.
func InheritPipe(cmd *exec.Cmd, in, out *os.File) ([]string, error) {
	handles := []syscall.Handle{syscall.Handle(in.Fd()), syscall.Handle(out.Fd())}
	for _, h := range handles {
		if err := windows.SetHandleInformation(windows.Handle(h), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return nil, fmt.Errorf("make pipe handle inheritable: %w", err)
		}
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, handles...)
	return []string{
		"--remote-debugging-pipe",
		fmt.Sprintf("--remote-debugging-io-pipes=%d,%d", handles[0], handles[1]),
	}, nil
}
