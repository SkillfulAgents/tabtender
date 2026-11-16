package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func env(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsApplyWhenFileIsMissing(t *testing.T) {
	store, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	snap := store.Snapshot()
	want := Default()
	want.normalize()
	if !reflect.DeepEqual(snap.Effective, want) {
		t.Fatalf("effective = %+v, want defaults %+v", snap.Effective, want)
	}
	if snap.Effective.Server.Host != "127.0.0.1" || snap.Effective.Server.Port != 3000 {
		t.Fatalf("server must default to loopback:3000, got %+v", snap.Effective.Server)
	}
	if got := snap.Effective.Sessions; got.DefaultTTL != 3600 || got.MaxTTL != 14400 || got.MaxConcurrent != 8 {
		t.Fatalf("unexpected session defaults: %+v", got)
	}
}

func TestSpecExampleFileParses(t *testing.T) {
	path := writeConfig(t, `
server:
  host: 127.0.0.1
  port: 3000

sessions:
  defaultTTL: 3600
  maxTTL: 14400
  maxConcurrent: 8

browser:
  runtime: chrome
  executablePath: auto
  headless: false

tunnel:
  provider: cloudflare-quick

app:
  startOnLogin: true
  preventSleep: true
`)
	store, err := Load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot().Effective.Tunnel.Provider; got != TunnelCloudflareQuick {
		t.Fatalf("tunnel.provider = %q", got)
	}
}

func TestPrecedenceEnvironmentOverFileOverDefaults(t *testing.T) {
	path := writeConfig(t, "server:\n  port: 4000\nsessions:\n  maxConcurrent: 2\n")
	store, err := Load(path, env(map[string]string{
		EnvPort:           "5000",
		EnvHost:           "0.0.0.0",
		EnvDefaultTTL:     "60",
		EnvMaxTTL:         "120",
		EnvMaxSessions:    "3",
		EnvBrowserRuntime: "chrome",
		EnvChromePath:     "/opt/chrome",
		EnvTunnelProvider: "cloudflare-quick",
	}))
	if err != nil {
		t.Fatal(err)
	}
	snap := store.Snapshot()
	if snap.File.Server.Port != 4000 || snap.Effective.Server.Port != 5000 {
		t.Fatalf("port: file=%d effective=%d, want 4000/5000", snap.File.Server.Port, snap.Effective.Server.Port)
	}
	if snap.File.Sessions.MaxConcurrent != 2 || snap.Effective.Sessions.MaxConcurrent != 3 {
		t.Fatalf("maxConcurrent: file=%d effective=%d", snap.File.Sessions.MaxConcurrent, snap.Effective.Sessions.MaxConcurrent)
	}
	// Untouched by file and env: the default survives.
	if !snap.Effective.App.StartOnLogin {
		t.Fatal("app.startOnLogin default lost")
	}
	e := snap.Effective
	if e.Server.Host != "0.0.0.0" || e.Sessions.DefaultTTL != 60 || e.Sessions.MaxTTL != 120 ||
		e.Browser.ExecutablePath != "/opt/chrome" || e.Tunnel.Provider != "cloudflare-quick" {
		t.Fatalf("environment overrides not applied: %+v", e)
	}
	wantKeys := []string{"server.host", "server.port", "sessions.defaultTTL", "sessions.maxTTL",
		"sessions.maxConcurrent", "browser.runtime", "browser.executablePath", "tunnel.provider"}
	for _, key := range wantKeys {
		if _, ok := snap.Overrides[key]; !ok {
			t.Errorf("override %s not reported", key)
		}
	}
	if got := snap.Overrides["server.port"]; got.Variable != EnvPort || got.Value != "5000" {
		t.Fatalf("unexpected override record: %+v", got)
	}
}

func TestInvalidInputsAreRejectedWithExplicitErrors(t *testing.T) {
	cases := []struct {
		name, file string
		env        map[string]string
		wantErr    string
	}{
		{"unknown key", "server:\n  prot: 1\n", nil, "prot"},
		{"malformed yaml", "server: [", nil, "parse config"},
		{"port range", "server:\n  port: 70000\n", nil, "server.port"},
		{"bad host", "server:\n  host: not a host\n", nil, "server.host"},
		{"default above max", "sessions:\n  defaultTTL: 100\n  maxTTL: 50\n", nil, "must not exceed"},
		{"zero concurrency", "sessions:\n  maxConcurrent: 0\n", nil, "sessions.maxConcurrent"},
		{"hostname with scheme", "tunnel:\n  cloudflare:\n    hostname: https://x.example\n", nil, "bare hostname"},
		{"bad public url", "server:\n  publicUrl: ftp://x\n", nil, "publicUrl"},
		{"non-numeric env", "", map[string]string{EnvPort: "abc"}, EnvPort},
		{"env breaks invariant", "", map[string]string{EnvMaxTTL: "10"}, "must not exceed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.file), env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateReportsAllProblemsAtOnce(t *testing.T) {
	cfg := Default()
	cfg.Server.Port = 0
	cfg.Sessions.MaxConcurrent = -1
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "server.port") || !strings.Contains(err.Error(), "maxConcurrent") {
		t.Fatalf("expected both problems, got %v", err)
	}
}

func TestSaveNeverPersistsEnvironmentValues(t *testing.T) {
	path := writeConfig(t, "server:\n  port: 4000\n")
	store, err := Load(path, env(map[string]string{EnvPort: "5000", EnvMaxSessions: "3"}))
	if err != nil {
		t.Fatal(err)
	}
	file := store.Snapshot().File
	file.Sessions.DefaultTTL = 900
	file.Sessions.MaxConcurrent = 6
	snap, err := store.Save(file)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Effective.Server.Port != 5000 || snap.Effective.Sessions.MaxConcurrent != 3 {
		t.Fatalf("environment must keep winning after save: %+v", snap.Effective)
	}
	if snap.Effective.Sessions.DefaultTTL != 900 {
		t.Fatalf("saved value not effective: %+v", snap.Effective.Sessions)
	}

	// Reload without the environment: only file-layer values are on disk.
	reloaded, err := Load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Snapshot().Effective
	if got.Server.Port != 4000 || got.Sessions.MaxConcurrent != 6 || got.Sessions.DefaultTTL != 900 {
		t.Fatalf("file content wrong after save: %+v", got)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "5000") {
		t.Fatalf("environment value leaked into the file:\n%s", raw)
	}
}

func TestSaveRejectsInvalidConfigAndKeepsFile(t *testing.T) {
	path := writeConfig(t, "server:\n  port: 4000\n")
	store, err := Load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	bad := store.Snapshot().File
	bad.Sessions.MaxTTL = 0
	if _, err := store.Save(bad); err == nil {
		t.Fatal("expected a validation error")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "server:\n  port: 4000\n" {
		t.Fatalf("file changed despite failed save:\n%s", raw)
	}
	if store.Snapshot().Effective.Sessions.MaxTTL != 14400 {
		t.Fatal("in-memory snapshot changed despite failed save")
	}
}

func TestWriteFileAtomicLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")
	for _, content := range []string{"one", "two"} {
		if err := WriteFileAtomic(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != "two" {
		t.Fatalf("content = %q", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("expected only the target file, found %d entries", len(entries))
	}
}

func TestPendingRestart(t *testing.T) {
	running := Default()
	next := running
	next.Sessions.MaxConcurrent = 2
	if got := PendingRestart(running, next); len(got) != 0 {
		t.Fatalf("live setting flagged as restart-required: %v", got)
	}
	next.Server.Port = 9
	next.Server.Host = "0.0.0.0"
	if got := PendingRestart(running, next); !reflect.DeepEqual(got, []string{"server.host", "server.port"}) {
		t.Fatalf("pending = %v", got)
	}
}

func TestResolvePathsHonorsEnvironment(t *testing.T) {
	dir := t.TempDir()
	paths, err := ResolvePaths(env(map[string]string{
		EnvConfigPath: filepath.Join(dir, "custom.yaml"),
		EnvDataDir:    filepath.Join(dir, "data"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigFile != filepath.Join(dir, "custom.yaml") || paths.DataDir != filepath.Join(dir, "data") {
		t.Fatalf("paths = %+v", paths)
	}
	defaults, err := ResolvePaths(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(defaults.ConfigFile) != "config.yaml" || !filepath.IsAbs(defaults.DataDir) {
		t.Fatalf("default paths = %+v", defaults)
	}
}
