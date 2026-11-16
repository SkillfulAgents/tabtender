package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/SkillfulAgents/tabtender/internal/config"
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// sealer transforms secrets before they reach the disk.
type sealer struct {
	backend string
	ext     string
	seal    func([]byte) ([]byte, error)
	unseal  func([]byte) ([]byte, error)
}

// fileStore keeps one file per secret in a private directory.
type fileStore struct {
	dir    string
	sealer sealer
}

func identity(b []byte) ([]byte, error) { return b, nil }

func newFileStore(dir string) (Store, error) {
	return newSealedStore(dir, sealer{backend: BackendFile, ext: ".secret", seal: identity, unseal: identity})
}

func newSealedStore(dir string, s sealer) (Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create secrets directory: %w", err)
	}
	if err := restrictDir(dir); err != nil {
		return nil, fmt.Errorf("restrict secrets directory: %w", err)
	}
	return &fileStore{dir: dir, sealer: s}, nil
}

func (s *fileStore) path(name string) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	return filepath.Join(s.dir, name+s.sealer.ext), nil
}

func (s *fileStore) Get(name string) (string, error) {
	path, err := s.path(name)
	if err != nil {
		return "", err
	}
	sealed, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	plain, err := s.sealer.unseal(sealed)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %s: %w", name, err)
	}
	return string(plain), nil
}

func (s *fileStore) Set(name, value string) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	sealed, err := s.sealer.seal([]byte(value))
	if err != nil {
		return fmt.Errorf("encrypt secret %s: %w", name, err)
	}
	return config.WriteFileAtomic(path, sealed, 0o600)
}

func (s *fileStore) Delete(name string) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *fileStore) Backend() string { return s.sealer.backend }
