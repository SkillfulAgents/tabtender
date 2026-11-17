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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/runtime"
	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
)

// Kind is the runtime kind this adapter registers under.
const Kind = "chrome"

const (
	startupTimeout = 45 * time.Second
	// gracePeriod is how long a browser gets to exit by itself after being
	// asked to, before its process tree is killed.
	gracePeriod         = 10 * time.Second
	closeRequestTimeout = 3 * time.Second
	killTimeout         = 10 * time.Second
	stderrDrainTimeout  = time.Second
	stderrTailSize      = 4 << 10
)

// staleLockFiles are Chrome's single-instance markers in a profile directory.
var staleLockFiles = []string{"SingletonLock", "SingletonCookie", "SingletonSocket"}

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

// launchArgs builds the browser command line. transport holds the flags that
// select the debugging pipe.
func launchArgs(opts runtime.BrowserOptions, transport, extra []string) []string {
	args := append([]string{}, transport...)
	args = append(args,
		"--user-data-dir="+opts.ProfileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-search-engine-choice-screen",
		"--hide-crash-restore-bubble",
		// Keep profile encryption self-contained instead of prompting for
		// the OS keychain on every fresh profile.
		"--password-store=basic",
		"--use-mock-keychain",
	)
	if opts.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, extra...)
	return append(args, "about:blank")
}

// checkExtraArgs rejects flags that would open a debugging endpoint next to
// the pipe: a debugging port is reachable by every local process, without
// any credential.
func checkExtraArgs(extra []string) error {
	for _, arg := range extra {
		if strings.HasPrefix(arg, "--remote-debugging-") || strings.HasPrefix(arg, "--remote-allow-origins") {
			return fmt.Errorf("browser.extraArgs must not configure remote debugging (%s)", arg)
		}
	}
	return nil
}

// Launch starts a browser with its own profile directory and waits until it
// answers on its debugging pipe.
func (r *Runtime) Launch(ctx context.Context, opts runtime.BrowserOptions) (runtime.BrowserInstance, error) {
	if opts.ProfileDir == "" {
		return nil, errors.New("chrome: profile directory is required")
	}
	if err := checkExtraArgs(r.cfg.ExtraArgs); err != nil {
		return nil, err
	}
	executable, err := r.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.ProfileDir, 0o700); err != nil {
		return nil, fmt.Errorf("create profile directory: %w", err)
	}
	// A browser that was killed leaves its profile lock behind, naming the
	// host it ran on; after a hostname change Chrome would refuse the
	// profile. The session manager guarantees exclusive use of the
	// directory, so stale locks are safe to drop.
	for _, name := range staleLockFiles {
		_ = os.Remove(filepath.Join(opts.ProfileDir, name))
	}

	// The browser reads commands from one pipe and writes messages to the
	// other.
	commandsOut, commandsIn, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create debugging pipe: %w", err)
	}
	messagesOut, messagesIn, err := os.Pipe()
	if err != nil {
		_ = commandsOut.Close()
		_ = commandsIn.Close()
		return nil, fmt.Errorf("create debugging pipe: %w", err)
	}
	closeAll := func(files ...*os.File) {
		for _, f := range files {
			_ = f.Close()
		}
	}

	// Standard error is collected through a pipe of our own rather than by
	// exec.Cmd: helpers that outlive the browser (its crash handler, its
	// updater) inherit it, and Cmd.Wait would not report the browser's exit
	// until the last of them is gone.
	stderrOut, stderrIn, err := os.Pipe()
	if err != nil {
		closeAll(commandsOut, commandsIn, messagesOut, messagesIn)
		return nil, fmt.Errorf("create output pipe: %w", err)
	}
	stderr := &tailBuffer{max: stderrTailSize}
	go func() { _, _ = io.Copy(stderr, stderrOut) }()
	cmd := exec.Command(executable)
	platform.Prepare(cmd)
	transport, err := platform.InheritPipe(cmd, commandsOut, messagesIn)
	if err != nil {
		closeAll(commandsOut, commandsIn, messagesOut, messagesIn, stderrOut, stderrIn)
		return nil, err
	}
	argv := append([]string{executable}, launchArgs(opts, transport, r.cfg.ExtraArgs)...)
	if len(r.cfg.Launcher) > 0 {
		argv = append(append([]string{}, r.cfg.Launcher...), argv...)
	}
	cmd.Path, cmd.Args = argv[0], argv
	cmd.Stderr = stderrIn
	cmd.Env = platform.ChildEnv()
	err = cmd.Start()
	// The browser holds its own copies of these ends now.
	closeAll(commandsOut, messagesIn, stderrIn)
	if err != nil {
		closeAll(commandsIn, messagesOut, stderrOut)
		return nil, fmt.Errorf("start browser: %w", err)
	}
	inst := &instance{
		pid:  cmd.Process.Pid,
		mux:  newMux(commandsIn, messagesOut),
		done: make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		inst.mux.Close()
		_ = messagesOut.Close()
		close(inst.done)
		// Let the last of the browser's output arrive, then stop listening
		// to whatever still holds the pipe.
		time.Sleep(stderrDrainTimeout)
		_ = stderrOut.Close()
	}()

	if err := inst.awaitReady(ctx); err != nil {
		_ = platform.KillTree(inst.pid)
		// Give the browser's dying words a moment to arrive.
		waitFor(context.Background(), inst.done, stderrDrainTimeout)
		time.Sleep(50 * time.Millisecond)
		if tail := stderr.String(); tail != "" {
			err = fmt.Errorf("%w; browser output: %s", err, tail)
		}
		return nil, err
	}
	r.logger.Debug("browser started", "pid", inst.pid, "headless", opts.Headless)
	return inst, nil
}

// awaitReady waits for the browser to answer its first command.
func (i *instance) awaitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	_, err := i.mux.call(ctx, "Browser.getVersion", nil)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("browser did not answer on its debugging pipe within %s", startupTimeout)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("browser launch canceled: %w", err)
	default:
		return fmt.Errorf("browser exited before it was ready: %w", err)
	}
}

// Close shuts the browser down: first politely through CDP so profile data is
// flushed, then by killing whatever is left of the process tree.
func (r *Runtime) Close(ctx context.Context, bi runtime.BrowserInstance) error {
	inst, ok := bi.(*instance)
	if !ok {
		return fmt.Errorf("chrome: unexpected instance type %T", bi)
	}
	if !inst.IsRunning() {
		// The process has been reaped: its ID may already belong to
		// something else and must not be signalled.
		return nil
	}
	closeCtx, cancel := context.WithTimeout(ctx, closeRequestTimeout)
	_, err := inst.mux.call(closeCtx, "Browser.close", nil)
	cancel()
	// The browser dropping the pipe as it exits is as good as an answer.
	if err == nil || errors.Is(err, errBrowserGone) || errors.Is(err, errConnClosed) {
		if waitFor(ctx, inst.done, gracePeriod) {
			return nil
		}
	} else {
		r.logger.Debug("graceful browser close failed", "pid", inst.pid, "error", err)
	}
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

type instance struct {
	pid  int
	mux  *mux
	done chan struct{}
}

// Connect implements runtime.BrowserInstance.
func (i *instance) Connect(ctx context.Context) (runtime.CDPConn, error) { return i.mux.Connect(ctx) }
func (i *instance) PID() int                                             { return i.pid }
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
