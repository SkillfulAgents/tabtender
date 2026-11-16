//go:build !darwin && !windows

package chrome

import "os/exec"

// candidates lists Chrome locations in order of preference.
func candidates() []string {
	var paths []string
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			paths = append(paths, path)
		}
	}
	return paths
}
