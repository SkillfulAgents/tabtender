package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// Paths locates the files and directories tabtenderd works with.
type Paths struct {
	ConfigFile string
	DataDir    string
}

// ProfilesDir is the root of all browser profile directories.
func (p Paths) ProfilesDir() string { return filepath.Join(p.DataDir, "profiles") }

// StateDir holds daemon bookkeeping such as the session journal.
func (p Paths) StateDir() string { return filepath.Join(p.DataDir, "state") }

// LogFile is the daemon log file.
func (p Paths) LogFile() string { return filepath.Join(p.DataDir, "logs", "tabtenderd.log") }

// SecretsDir holds file-backed secrets on platforms without a keychain.
func (p Paths) SecretsDir() string { return filepath.Join(p.DataDir, "secrets") }

// BinDir holds managed helper executables such as cloudflared.
func (p Paths) BinDir() string { return filepath.Join(p.DataDir, "bin") }

// ManagementSocket is the default Unix socket for the local management API.
func (p Paths) ManagementSocket() string { return filepath.Join(p.DataDir, "tabtenderd.sock") }

// ResolvePaths applies TABTENDER_CONFIG_PATH and TABTENDER_DATA_DIR over the
// platform defaults:
//
//   - macOS:   ~/Library/Application Support/Tabtender
//   - Windows: %ProgramData%\Tabtender
//   - other:   $XDG_CONFIG_HOME/tabtender and $XDG_DATA_HOME/tabtender
func ResolvePaths(lookup LookupEnv) (Paths, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	configDir, dataDir, err := defaultDirs(lookup)
	if err != nil {
		return Paths{}, err
	}
	p := Paths{ConfigFile: filepath.Join(configDir, "config.yaml"), DataDir: dataDir}
	if v, ok := lookup(EnvConfigPath); ok && v != "" {
		p.ConfigFile = v
	}
	if v, ok := lookup(EnvDataDir); ok && v != "" {
		p.DataDir = v
	}
	if p.ConfigFile, err = filepath.Abs(p.ConfigFile); err != nil {
		return Paths{}, err
	}
	if p.DataDir, err = filepath.Abs(p.DataDir); err != nil {
		return Paths{}, err
	}
	return p, nil
}

func defaultDirs(lookup LookupEnv) (configDir, dataDir string, err error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		dir := filepath.Join(home, "Library", "Application Support", "Tabtender")
		return dir, dir, nil
	case "windows":
		base, ok := lookup("ProgramData")
		if !ok || base == "" {
			base = `C:\ProgramData`
		}
		dir := filepath.Join(base, "Tabtender")
		return dir, dir, nil
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		configBase, ok := lookup("XDG_CONFIG_HOME")
		if !ok || configBase == "" {
			configBase = filepath.Join(home, ".config")
		}
		dataBase, ok := lookup("XDG_DATA_HOME")
		if !ok || dataBase == "" {
			dataBase = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(configBase, "tabtender"), filepath.Join(dataBase, "tabtender"), nil
	}
}
