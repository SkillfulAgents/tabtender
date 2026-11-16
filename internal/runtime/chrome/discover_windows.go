package chrome

import (
	"os"
	"path/filepath"
)

// candidates lists Chrome locations in order of preference.
func candidates() []string {
	var paths []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
		if root := os.Getenv(env); root != "" {
			paths = append(paths, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
		}
	}
	return paths
}
