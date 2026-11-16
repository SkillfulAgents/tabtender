// Package secrets stores credentials outside of ordinary configuration files
// using the most appropriate facility of each operating system:
//
//   - macOS:   the user's login Keychain
//   - Windows: DPAPI-encrypted files in an ACL-restricted directory
//   - other:   owner-only (0600) files
package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Well-known secret names.
const (
	APIKey      = "api-key"
	TunnelToken = "cloudflare-tunnel-token"
)

// Backend identifiers accepted by Open.
const (
	BackendAuto     = ""
	BackendFile     = "file"
	BackendKeychain = "keychain"
	BackendDPAPI    = "dpapi"
)

// ErrNotFound is returned when a secret does not exist.
var ErrNotFound = errors.New("secret not found")

// Store persists named secrets.
type Store interface {
	// Get returns the secret or ErrNotFound.
	Get(name string) (string, error)
	// Set creates or replaces the secret.
	Set(name, value string) error
	// Delete removes the secret; deleting a missing secret is not an error.
	Delete(name string) error
	// Backend names the storage facility for display purposes.
	Backend() string
}

// Open returns the secret store for the requested backend. BackendAuto picks
// the platform default; dir is used by file-backed stores.
func Open(backend, dir string) (Store, error) {
	if backend == BackendAuto {
		backend = defaultBackend
	}
	switch backend {
	case BackendFile:
		return newFileStore(dir)
	case BackendKeychain, BackendDPAPI:
		if backend != defaultBackend {
			return nil, fmt.Errorf("secrets backend %q is not available on this platform", backend)
		}
		return newPlatformStore(dir)
	default:
		return nil, fmt.Errorf("unknown secrets backend %q", backend)
	}
}

// ImportProvisioned moves administrator-supplied secrets into the store. For
// each name, a plaintext file "<dir>/<name>.provision" is read, stored, and
// deleted. This is how keys are supplied on hosts without a management UI.
func ImportProvisioned(store Store, dir string, logger *slog.Logger, names ...string) error {
	for _, name := range names {
		path := filepath.Join(dir, name+".provision")
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read provisioned secret %s: %w", name, err)
		}
		value := strings.TrimSpace(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
		if value == "" {
			return fmt.Errorf("provisioned secret %s is empty", name)
		}
		if err := store.Set(name, value); err != nil {
			return fmt.Errorf("store provisioned secret %s: %w", name, err)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove provisioned secret file %s: %w", path, err)
		}
		logger.Info("imported provisioned secret", "name", name)
	}
	return nil
}
