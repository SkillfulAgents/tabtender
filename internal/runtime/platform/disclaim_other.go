//go:build !darwin

package platform

import "errors"

// DisclaimSupported reports whether DisclaimExec is available. Process
// responsibility is a macOS concept.
const DisclaimSupported = false

// DisclaimExec is only available on macOS.
func DisclaimExec(string, []string, []string) error {
	return errors.New("disclaim-exec is only supported on macOS")
}
