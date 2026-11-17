// Package chrome implements the BrowserRuntime adapter for Google Chrome and
// other Chromium-based browsers that accept the same command-line flags.
package chrome

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/runtime"
	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
)

// Kind is the runtime kind this adapter registers under.
const Kind = "chrome"

const (
	// activePortFile is written by Chrome into the profile directory once
	// the DevTools server is listening: line 1 is the port, line 2 the
	// browser WebSocket path.
	activePortFile = "DevToolsActivePort"

	startupTimeout = 45 * time.Second
	// gracePeriod is how long a browser gets to exit by itself after being
	// asked to, before its process tree is killed.
	gracePeriod         = 10 * time.Second
	closeRequestTimeout = 3 * time.Second
	killTimeout         = 10 * time.Second
	pollInterval        = 25 * time.Millisecond
	stderrTailSize      = 4 << 10
)

// Runtime launches Chrome processes.
type Runtime struct {
	cfg    runtime.RuntimeConfig
	logger *slog.Logger
}

// New returns a Chrome runtime. A missing browser is not an error here; it is
// reported by Launch and Executable so the service can start regardless.
func New(cfg runtime.RuntimeConfig) (runtime.BrowserRuntime, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runtime{cfg: cfg, logger: logger}, nil
}

// Register adds the Chrome adapter to a runtime registry.
func Register(r *runtime.Registry) { r.Register(Kind, New) }

// Executable resolves the browser binary that Launch will use.
func (r *Runtime) Executable() (string, error) {
	path := r.cfg.ExecutablePath
	if path != "" && path != config.AutoPath {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return "", fmt.Errorf("browser executable %q not found", path)
		}
		return path, nil
	}
	for _, candidate := range candidates() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("no Chrome installation found; install Google Chrome or set browser.executablePath")
}

func launchArgs(opts runtime.BrowserOptions, extra []string) []string {
	args := []string{
		// Port 0 makes Chrome pick a free loopback port, which it reports
		// through the DevToolsActivePort file. Chrome only ever binds the
		// DevTools server to loopback.
		"--remote-debugging-port=0",
		"--user-data-dir=" + opts.ProfileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-search-engine-choice-screen",
		"--hide-crash-restore-bubble",
		// Keep profile encryption self-contained instead of prompting for
		// the OS keychain on every fresh profile.
		"--password-store=basic",
		"--use-mock-keychain",
	}
	if opts.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, extra...)
	return append(args, "about:blank")
}

// Launch starts a browser with its own profile directory and waits until its
// CDP endpoint is ready.
func (r *Runtime) Launch(ctx context.Context, opts runtime.BrowserOptions) (runtime.BrowserInstance, error) {
	if opts.ProfileDir == "" {
		return nil, errors.New("chrome: profile directory is required")
	}
	executable, err := r.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.ProfileDir, 0o700); err != nil {
		return nil, fmt.Errorf("create profile directory: %w", err)
	}
	portFile := filepath.Join(opts.ProfileDir, activePortFile)
	if err := os.Remove(portFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("clear stale %s: %w", activePortFile, err)
	}

	stderr := &tailBuffer{max: stderrTailSize}
	argv := append([]string{executable}, launchArgs(opts, r.cfg.ExtraArgs)...)
	if len(r.cfg.Launcher) > 0 {
		argv = append(append([]string{}, r.cfg.Launcher...), argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = stderr
	cmd.Env = platform.ChildEnv()
	platform.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start browser: %w", err)
	}
	inst := &instance{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(inst.done)
	}()

	endpoint, err := waitForEndpoint(ctx, portFile, inst.done)
	if err != nil {
		_ = platform.KillTree(inst.pid)
		if tail := stderr.String(); tail != "" {
			err = fmt.Errorf("%w; browser output: %s", err, tail)
		}
		return nil, err
	}
	inst.endpoint = endpoint
	r.logger.Debug("browser started", "pid", inst.pid, "headless", opts.Headless)
	return inst, nil
}

func waitForEndpoint(ctx context.Context, portFile string, exited <-chan struct{}) (string, error) {
	timeout := time.NewTimer(startupTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if endpoint, ok := readEndpoint(portFile); ok {
			return endpoint, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("browser launch canceled: %w", ctx.Err())
		case <-exited:
			return "", errors.New("browser exited before its CDP endpoint became available")
		case <-timeout.C:
			return "", fmt.Errorf("browser did not expose a CDP endpoint within %s", startupTimeout)
		case <-ticker.C:
		}
	}
}

func readEndpoint(portFile string) (string, bool) {
	data, err := os.ReadFile(portFile)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return "", false
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	path := strings.TrimSpace(lines[1])
	if err != nil || port <= 0 || !strings.HasPrefix(path, "/") {
		return "", false
	}
	return fmt.Sprintf("ws://127.0.0.1:%d%s", port, path), true
}

// Close shuts the browser down: first politely through CDP so profile data is
// flushed, then by killing whatever is left of the process tree.
func (r *Runtime) Close(ctx context.Context, bi runtime.BrowserInstance) error {
	inst, ok := bi.(*instance)
	if !ok {
		return fmt.Errorf("chrome: unexpected instance type %T", bi)
	}
	if inst.IsRunning() {
		if err := requestClose(ctx, inst.endpoint); err != nil {
			r.logger.Debug("graceful browser close failed", "pid", inst.pid, "error", err)
		} else {
			waitFor(ctx, inst.done, gracePeriod)
		}
	}
	// Always sweep the process group: helper processes can outlive the main
	// browser process.
	killErr := platform.KillTree(inst.pid)
	if !waitFor(ctx, inst.done, killTimeout) {
		return errors.Join(fmt.Errorf("browser process %d did not exit", inst.pid), killErr)
	}
	return nil
}

func waitFor(ctx context.Context, done <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// loopbackClient never uses a proxy: CDP endpoints are always on loopback.
var loopbackClient = &http.Client{Transport: &http.Transport{Proxy: nil}}

// requestClose asks the browser to shut itself down and waits until it has
// taken the request (it answers, or drops the connection as it exits), so
// that the shutdown is orderly and profile data gets flushed.
func requestClose(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, closeRequestTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: loopbackClient})
	if err != nil {
		return err
	}
	defer conn.CloseNow() //nolint:errcheck // best effort
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Browser.close"}`)); err != nil {
		return err
	}
	for {
		_, reply, err := conn.Read(ctx)
		if err != nil {
			// The browser closing the connection is the expected outcome.
			return nil
		}
		if bytes.Contains(reply, []byte(`"id":1`)) {
			return nil
		}
	}
}

type instance struct {
	pid      int
	endpoint string
	done     chan struct{}
}

func (i *instance) CDPEndpoint() string { return i.endpoint }
func (i *instance) PID() int            { return i.pid }
func (i *instance) IsRunning() bool {
	select {
	case <-i.done:
		return false
	default:
		return true
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.TrimSpace(t.buf))
}
