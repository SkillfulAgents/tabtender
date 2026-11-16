//go:build !darwin && !windows

package secrets

const defaultBackend = BackendFile

func newPlatformStore(dir string) (Store, error) { return newFileStore(dir) }
