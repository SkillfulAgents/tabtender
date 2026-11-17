package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"go.yaml.in/yaml/v3"
)

// Snapshot is a consistent view of the configuration layers.
type Snapshot struct {
	// Path is the configuration file location.
	Path string
	// File is the file layer merged over the defaults: what is (or would be)
	// persisted on disk.
	File Config
	// Effective is File with environment overrides applied.
	Effective Config
	// Overrides lists environment-controlled settings by dotted key.
	Overrides map[string]Override
}

// Store owns the configuration file and resolves the effective configuration.
type Store struct {
	path   string
	lookup LookupEnv

	mu       sync.RWMutex
	snapshot Snapshot
}

// Load reads the configuration file at path (a missing file means defaults),
// applies environment overrides, and validates the result.
func Load(path string, lookup LookupEnv) (*Store, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	file, err := readFile(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, lookup: lookup}
	snap, err := s.resolve(file)
	if err != nil {
		return nil, err
	}
	s.snapshot = snap
	return s, nil
}

func readFile(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

func (s *Store) resolve(file Config) (Snapshot, error) {
	file.normalize()
	if err := file.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("invalid configuration file: %w", err)
	}
	effective := file
	effective.Browser.ExtraArgs = append([]string{}, file.Browser.ExtraArgs...)
	overrides, err := applyEnv(&effective, s.lookup)
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid environment override: %w", err)
	}
	effective.normalize()
	if err := effective.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("invalid effective configuration: %w", err)
	}
	return Snapshot{Path: s.path, File: file, Effective: effective, Overrides: overrides}, nil
}

// Snapshot returns the current configuration layers.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := s.snapshot
	// Callers edit what they get; the stored layers must not change with it.
	snap.File, snap.Effective = snap.File.Clone(), snap.Effective.Clone()
	return snap
}

// Save validates file as the new file layer, writes it atomically, and
// returns the re-resolved snapshot. Environment overrides are never written:
// only the file layer is persisted.
func (s *Store) Save(file Config) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.resolve(file)
	if err != nil {
		return Snapshot{}, err
	}
	data, err := yaml.Marshal(snap.File)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode config: %w", err)
	}
	if err := WriteFileAtomic(s.path, data, 0o600); err != nil {
		return Snapshot{}, fmt.Errorf("write config: %w", err)
	}
	s.snapshot = snap
	return snap, nil
}

// WriteFileAtomic writes data to path via a temporary file and rename so
// readers never observe a partially written file.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
