//go:build !windows

package secrets

import "os"

// restrictDir limits the directory to its owner.
func restrictDir(dir string) error { return os.Chmod(dir, 0o700) }
