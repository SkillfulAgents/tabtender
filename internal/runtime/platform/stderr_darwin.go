package platform

import (
	"os"
	"syscall"
)

// RedirectStderr points the process's standard error at the given file,
// replacing its contents. The Go runtime reports fatal errors (panics, and
// failures before logging is set up) on standard error, which launchd
// discards.
func RedirectStderr(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // the duplicate stays open
	return syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd()))
}
