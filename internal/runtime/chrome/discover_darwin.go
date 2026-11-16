package chrome

import (
	"os"
	"path/filepath"
)

// candidates lists Chrome locations in order of preference.
func candidates() []string {
	apps := []string{
		"Google Chrome.app/Contents/MacOS/Google Chrome",
		"Google Chrome Beta.app/Contents/MacOS/Google Chrome Beta",
		"Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
		"Chromium.app/Contents/MacOS/Chromium",
	}
	roots := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "Applications"))
	}
	var paths []string
	for _, app := range apps {
		for _, root := range roots {
			paths = append(paths, filepath.Join(root, app))
		}
	}
	return paths
}
