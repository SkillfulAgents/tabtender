package power

import (
	"os"
	"os/exec"
	"strconv"
)

// acquire prevents idle sleep by running caffeinate bound to our PID, so the
// assertion can never outlive the daemon.
func acquire() (release func(), err error) {
	cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}, nil
}
