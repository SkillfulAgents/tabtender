//go:build !darwin

package platform

// RedirectStderr is only needed under launchd, which discards standard
// error; other service managers capture it or have their own failure log.
func RedirectStderr(string) error { return nil }
