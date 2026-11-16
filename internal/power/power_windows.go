package power

import (
	"runtime"

	"golang.org/x/sys/windows"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var setThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// acquire prevents idle sleep. The execution state belongs to a thread, so a
// dedicated OS thread holds it until release is called.
func acquire() (release func(), err error) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		_, _, _ = setThreadExecutionState.Call(uintptr(esContinuous | esSystemRequired))
		<-stop
		_, _, _ = setThreadExecutionState.Call(uintptr(esContinuous))
	}()
	return func() {
		close(stop)
		<-done
	}, nil
}
