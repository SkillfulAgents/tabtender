package cloudflare

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
	"github.com/SkillfulAgents/tabtender/internal/tunnel"
)

// The test binary doubles as a fake cloudflared: when fakeModeEnv is set it
// imitates cloudflared's log output instead of running tests.
const (
	fakeModeEnv = "FAKE_CLOUDFLARED_MODE"
	fakeLogEnv  = "FAKE_CLOUDFLARED_LOG"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeModeEnv); mode != "" {
		fakeCloudflared(mode)
		return
	}
	os.Exit(m.Run())
}

func fakeCloudflared(mode string) {
	logPath := os.Getenv(fakeLogEnv)
	run := 1
	if data, err := os.ReadFile(logPath); err == nil {
		run = strings.Count(string(data), "\n") + 1
	}
	f, _ := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = fmt.Fprintf(f, "pid=%d token=%q args=%s\n", os.Getpid(), os.Getenv("TUNNEL_TOKEN"), strings.Join(os.Args[1:], " "))
	_ = f.Close()

	logf := func(level, msg string) {
		fmt.Fprintf(os.Stderr, "2026-01-01T00:00:00Z %s %s\n", level, msg)
	}
	announce := func() {
		logf("INF", "+--------------------------------------------------------+")
		logf("INF", fmt.Sprintf("|  https://fake-run-%d.trycloudflare.com                 |", run))
		logf("INF", "+--------------------------------------------------------+")
		logf("INF", "Registered tunnel connection connIndex=0 protocol=quic")
	}
	switch {
	case mode == "never-ready":
		logf("ERR", "failed to dial edge: connection refused")
	case mode == "crash-once" && run == 1:
		announce()
		time.Sleep(150 * time.Millisecond)
		os.Exit(1)
	default:
		announce()
	}
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	<-interrupted
}

type fakeRun struct {
	pid   int
	token string
	args  string
}

func readRuns(t *testing.T, logPath string) []fakeRun {
	t.Helper()
	data, _ := os.ReadFile(logPath)
	var runs []fakeRun
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r fakeRun
		var quoted string
		head, args, _ := strings.Cut(line, " args=")
		if _, err := fmt.Sscanf(head, "pid=%d token=%s", &r.pid, &quoted); err != nil {
			t.Fatalf("bad fake log line %q: %v", line, err)
		}
		r.token, _ = strconv.Unquote(quoted)
		r.args = args
		runs = append(runs, r)
	}
	return runs
}

type providerFixture struct {
	provider tunnel.TunnelProvider
	logPath  string
	stateDir string
}

func newFixture(t *testing.T, kind, mode string, mutate func(*tunnel.TunnelConfig)) *providerFixture {
	t.Helper()
	dir := t.TempDir()
	f := &providerFixture{logPath: filepath.Join(dir, "fake.log"), stateDir: filepath.Join(dir, "state")}
	t.Setenv(fakeModeEnv, mode)
	t.Setenv(fakeLogEnv, f.logPath)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := tunnel.TunnelConfig{ExecutablePath: self, LocalHost: "127.0.0.1", BinDir: filepath.Join(dir, "bin"), StateDir: f.stateDir}
	if mutate != nil {
		mutate(&cfg)
	}
	registry := tunnel.NewRegistry()
	Register(registry)
	f.provider, err = registry.Create(kind, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.provider.Stop(context.Background()) })
	return f
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestQuickTunnelLifecycle(t *testing.T) {
	f := newFixture(t, KindQuick, "ok", nil)
	if got := f.provider.Status().State; got != tunnel.StateStopped {
		t.Fatalf("initial state = %s", got)
	}
	url, err := f.provider.Start(context.Background(), 3210)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://fake-run-1.trycloudflare.com" {
		t.Fatalf("url = %q", url)
	}
	if got := f.provider.Status(); got.State != tunnel.StateRunning || got.URL != url || got.Provider != KindQuick {
		t.Fatalf("status = %+v", got)
	}
	runs := readRuns(t, f.logPath)
	if len(runs) != 1 {
		t.Fatalf("cloudflared ran %d times", len(runs))
	}
	if !strings.Contains(runs[0].args, "--url http://127.0.0.1:3210") || !strings.Contains(runs[0].args, "--no-autoupdate") {
		t.Fatalf("unexpected arguments: %s", runs[0].args)
	}
	if runs[0].token != "" {
		t.Fatal("quick tunnels must not receive a token")
	}

	if err := f.provider.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.provider.Status(); got.State != tunnel.StateStopped || got.URL != "" {
		t.Fatalf("status after stop = %+v", got)
	}
	eventually(t, "cloudflared to exit", func() bool { return !platform.Alive(runs[0].pid) })
	if err := f.provider.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestCloudflaredNeverSeesTabtenderCredentials(t *testing.T) {
	t.Setenv("TABTENDER_API_KEY", "must-not-leak")
	for _, kv := range platform.ChildEnv() {
		if strings.HasPrefix(kv, "TABTENDER_") {
			t.Fatalf("child environment contains %s", kv)
		}
	}
}

func TestNamedTunnelPassesTokenOutOfBand(t *testing.T) {
	f := newFixture(t, KindNamed, "ok", func(cfg *tunnel.TunnelConfig) {
		cfg.Hostname = "browser.example.com"
		cfg.Token = "super-secret-token"
	})
	url, err := f.provider.Start(context.Background(), 3000)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://browser.example.com" {
		t.Fatalf("url = %q, want the configured hostname", url)
	}
	run := readRuns(t, f.logPath)[0]
	if run.token != "super-secret-token" {
		t.Fatalf("token not delivered through the environment: %q", run.token)
	}
	if strings.Contains(run.args, "super-secret-token") {
		t.Fatal("the token must never appear on the command line")
	}
	if !strings.HasSuffix(run.args, " run") {
		t.Fatalf("unexpected arguments: %s", run.args)
	}
}

func TestNamedTunnelRequiresHostnameAndToken(t *testing.T) {
	registry := tunnel.NewRegistry()
	Register(registry)
	if _, err := registry.Create(KindNamed, tunnel.TunnelConfig{}); err == nil {
		t.Fatal("expected an error without a hostname")
	}
	f := newFixture(t, KindNamed, "ok", func(cfg *tunnel.TunnelConfig) { cfg.Hostname = "browser.example.com" })
	if _, err := f.provider.Start(context.Background(), 3000); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want a missing-token error", err)
	}
	if len(readRuns(t, f.logPath)) != 0 {
		t.Fatal("cloudflared must not be started without a token")
	}
}

func TestProviderRestartsCloudflaredAfterCrash(t *testing.T) {
	f := newFixture(t, KindQuick, "crash-once", nil)
	url, err := f.provider.Start(context.Background(), 3000)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://fake-run-1.trycloudflare.com" {
		t.Fatalf("first url = %q", url)
	}
	eventually(t, "the tunnel to recover with a new URL", func() bool {
		status := f.provider.Status()
		return status.State == tunnel.StateRunning && status.URL == "https://fake-run-2.trycloudflare.com"
	})
	if runs := readRuns(t, f.logPath); len(runs) != 2 {
		t.Fatalf("cloudflared ran %d times, want 2", len(runs))
	}
}

func TestStartFailsWhenTunnelNeverConnects(t *testing.T) {
	f := newFixture(t, KindQuick, "never-ready", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err := f.provider.Start(ctx, 3000)
	if err == nil || !strings.Contains(err.Error(), "failed to dial edge") {
		t.Fatalf("err = %v, want the last cloudflared error", err)
	}
	if got := f.provider.Status(); got.State != tunnel.StateError || got.URL != "" {
		t.Fatalf("status = %+v", got)
	}
	runs := readRuns(t, f.logPath)
	eventually(t, "cloudflared to be cleaned up", func() bool { return !platform.Alive(runs[0].pid) })
}

func TestMissingExecutable(t *testing.T) {
	f := newFixture(t, KindQuick, "ok", func(cfg *tunnel.TunnelConfig) {
		cfg.ExecutablePath = filepath.Join(t.TempDir(), "no-such-cloudflared")
	})
	if _, err := f.provider.Start(context.Background(), 3000); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
	if got := f.provider.Status().State; got != tunnel.StateError {
		t.Fatalf("state = %s", got)
	}
}

func TestStaleCloudflaredIsReapedOnlyWhenOwned(t *testing.T) {
	f := newFixture(t, KindQuick, "ok", nil)
	pidFile := filepath.Join(f.stateDir, "cloudflared.pid")
	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()

	// A leftover from a previous daemon run: its command line references
	// our PID file.
	stale := exec.Command(self, "tunnel", "--no-autoupdate", "--pidfile", pidFile, "--url", "http://127.0.0.1:1")
	stale.Env = append(os.Environ(), fakeLogEnv+"="+filepath.Join(t.TempDir(), "stale.log"))
	if err := stale.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = stale.Wait()
		close(exited)
	}()
	defer stale.Process.Kill() //nolint:errcheck // test cleanup
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(stale.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Start(context.Background(), 3000); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the stale cloudflared was not terminated")
	}
	_ = f.provider.Stop(context.Background())

	// A PID file pointing at an unrelated process (this test) is ignored.
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Start(context.Background(), 3000); err != nil {
		t.Fatal(err)
	}
	// Still alive to assert it: the foreign process was left alone.
}

func tgz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestDownloadVerifiesChecksumAndInstalls(t *testing.T) {
	binary := []byte("#!/bin/sh\necho fake cloudflared\n")
	archive := tgz(t, "cloudflared", binary)
	files := map[string][]byte{"/raw": binary, "/archive.tgz": archive}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	sum := func(b []byte) string {
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}
	ctx := context.Background()
	dir := t.TempDir()

	t.Run("raw binary", func(t *testing.T) {
		dest := filepath.Join(dir, "bin", "cloudflared-raw")
		if err := download(ctx, server.URL+"/raw", asset{"cloudflared-linux-amd64", sum(binary)}, dest); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(dest)
		if !bytes.Equal(got, binary) {
			t.Fatal("installed binary differs from the download")
		}
	})
	t.Run("tgz archive", func(t *testing.T) {
		dest := filepath.Join(dir, "bin", "cloudflared-tgz")
		if err := download(ctx, server.URL+"/archive.tgz", asset{"cloudflared-darwin-arm64.tgz", sum(archive)}, dest); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(dest)
		if !bytes.Equal(got, binary) {
			t.Fatal("extracted binary differs")
		}
	})
	t.Run("checksum mismatch", func(t *testing.T) {
		dest := filepath.Join(dir, "bin", "cloudflared-bad")
		err := download(ctx, server.URL+"/raw", asset{"cloudflared-linux-amd64", sum([]byte("something else"))}, dest)
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(dest); statErr == nil {
			t.Fatal("a tampered download was installed")
		}
	})
	t.Run("http error", func(t *testing.T) {
		if err := download(ctx, server.URL+"/missing", asset{"x", sum(nil)}, filepath.Join(dir, "bin", "x")); err == nil {
			t.Fatal("expected an error")
		}
	})
	entries, _ := os.ReadDir(filepath.Join(dir, "bin"))
	if len(entries) != 2 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestEveryReleasePlatformHasAPinnedAsset(t *testing.T) {
	for _, platform := range []string{"darwin/arm64", "darwin/amd64", "windows/amd64", "windows/arm64"} {
		a, ok := assets[platform]
		if !ok || len(a.sha256) != 64 || !strings.HasPrefix(a.name, "cloudflared-") {
			t.Errorf("missing or malformed asset for %s: %+v", platform, a)
		}
	}
}

func TestSupersededDownloadsAreRemoved(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "cloudflared-current")
	keep := []string{current, filepath.Join(dir, "unrelated.txt")}
	stale := []string{
		filepath.Join(dir, "cloudflared-2020.1.0"),
		filepath.Join(dir, downloadPrefix+"123"),
		filepath.Join(dir, downloadPrefix+"123.bin"),
	}
	for _, path := range append(append([]string{}, keep...), stale...) {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removeSuperseded(dir, current)
	for _, path := range keep {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed", filepath.Base(path))
		}
	}
	for _, path := range stale {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s was left behind", filepath.Base(path))
		}
	}
}
