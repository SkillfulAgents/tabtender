// Package cloudflare implements Cloudflare Tunnel providers on top of the
// cloudflared executable: temporary Quick Tunnels and persistent named
// tunnels authenticated by a token.
package cloudflare

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
	"github.com/SkillfulAgents/tabtender/internal/tunnel"
)

// Provider kinds registered by this package.
const (
	KindQuick = config.TunnelCloudflareQuick
	KindNamed = config.TunnelCloudflareNamed
)

const (
	readyTimeout = 2 * time.Minute
	maxBackoff   = 30 * time.Second
)

var (
	quickURLPattern   = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)
	registeredPattern = regexp.MustCompile(`Registered tunnel connection`)
)

// Register adds the Cloudflare providers to a tunnel registry.
func Register(r *tunnel.Registry) {
	r.Register(KindQuick, func(cfg tunnel.TunnelConfig) (tunnel.TunnelProvider, error) {
		return newProvider(KindQuick, cfg), nil
	})
	r.Register(KindNamed, func(cfg tunnel.TunnelConfig) (tunnel.TunnelProvider, error) {
		if cfg.Hostname == "" {
			return nil, errors.New("cloudflare-named requires tunnel.cloudflare.hostname")
		}
		return newProvider(KindNamed, cfg), nil
	})
}

type provider struct {
	kind   string
	cfg    tunnel.TunnelConfig
	logger *slog.Logger

	mu      sync.Mutex
	state   tunnel.State
	url     string
	lastErr string
	cancel  context.CancelFunc
	done    chan struct{}
}

func newProvider(kind string, cfg tunnel.TunnelConfig) *provider {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &provider{kind: kind, cfg: cfg, logger: logger, state: tunnel.StateStopped}
}

func (p *provider) pidFile() string { return filepath.Join(p.cfg.StateDir, "cloudflared.pid") }

func (p *provider) command(ctx context.Context, binary string, localPort int) *exec.Cmd {
	args := []string{"tunnel", "--no-autoupdate", "--pidfile", p.pidFile()}
	env := platform.ChildEnv()
	if p.kind == KindQuick {
		host := p.cfg.LocalHost
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		args = append(args, "--url", "http://"+net.JoinHostPort(host, strconv.Itoa(localPort)))
	} else {
		args = append(args, "run")
		// The token travels in the environment so it never shows up in
		// process listings.
		env = append(env, "TUNNEL_TOKEN="+p.cfg.Token)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// Start launches cloudflared under supervision and waits for the tunnel to
// connect. After a successful start, the provider restarts cloudflared by
// itself whenever it exits.
func (p *provider) Start(ctx context.Context, localPort int) (string, error) {
	if p.kind == KindNamed && p.cfg.Token == "" {
		return "", errors.New("no Cloudflare tunnel token is configured")
	}
	p.setState(tunnel.StateStarting, "", "")
	binary, err := resolveBinary(ctx, p.cfg.ExecutablePath, p.cfg.BinDir)
	if err != nil {
		p.setState(tunnel.StateError, "", err.Error())
		return "", err
	}
	if err := os.MkdirAll(p.cfg.StateDir, 0o700); err != nil {
		return "", err
	}
	p.reapStale()

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ready := make(chan string, 1)
	p.mu.Lock()
	p.cancel, p.done = cancel, done
	p.mu.Unlock()
	go p.supervise(runCtx, done, binary, localPort, ready)

	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()
	var failure error
	select {
	case url := <-ready:
		return url, nil
	case <-ctx.Done():
		failure = ctx.Err()
	case <-timer.C:
		failure = fmt.Errorf("tunnel did not connect within %s", readyTimeout)
	}
	if last := p.Status().Error; last != "" {
		failure = fmt.Errorf("%w (last cloudflared error: %s)", failure, last)
	}
	_ = p.Stop(context.Background())
	p.setState(tunnel.StateError, "", failure.Error())
	return "", failure
}

// reapStale kills a cloudflared left behind by a previous daemon run. The
// process is only touched if its command line references our private PID
// file, which proves it was started by this Tabtender instance.
func (p *provider) reapStale() {
	data, err := os.ReadFile(p.pidFile())
	if err != nil {
		return
	}
	_ = os.Remove(p.pidFile())
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	args, alive := platform.Args(pid)
	if !alive {
		return
	}
	for _, arg := range args {
		if arg == p.pidFile() {
			p.logger.Info("terminating leftover cloudflared", "pid", pid)
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
			return
		}
	}
}

func (p *provider) supervise(ctx context.Context, done chan struct{}, binary string, localPort int, ready chan<- string) {
	defer close(done)
	backoff := time.Second
	for {
		connected := p.runOnce(ctx, binary, localPort, ready)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		p.logger.Warn("cloudflared exited; restarting", "provider", p.kind, "in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runOnce runs cloudflared until it exits and reports whether the tunnel
// connected during this run.
func (p *provider) runOnce(ctx context.Context, binary string, localPort int, ready chan<- string) bool {
	p.setState(tunnel.StateStarting, "", "")
	cmd := p.command(ctx, binary, localPort)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		p.setState(tunnel.StateError, "", "start cloudflared: "+err.Error())
		return false
	}
	_ = os.WriteFile(p.pidFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)

	connected := make(chan bool, 1)
	go func() { connected <- p.watchOutput(pr, ready) }()
	err := cmd.Wait()
	_ = pw.Close()
	wasConnected := <-connected
	_ = os.Remove(p.pidFile())
	if ctx.Err() == nil {
		msg := "cloudflared exited"
		if err != nil {
			msg += ": " + err.Error()
		}
		if last := p.Status().Error; last != "" {
			msg += " (" + last + ")"
		}
		p.setState(tunnel.StateError, "", msg)
	}
	return wasConnected
}

// watchOutput follows cloudflared's log, flipping the provider to running
// once the tunnel is connected (and, for quick tunnels, its URL is known).
func (p *provider) watchOutput(r io.Reader, ready chan<- string) bool {
	url := ""
	if p.kind == KindNamed {
		url = "https://" + p.cfg.Hostname
	}
	registered, connected := false, false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if p.cfg.Token != "" {
			line = strings.ReplaceAll(line, p.cfg.Token, "[redacted]")
		}
		p.logger.Debug("cloudflared", "line", line)
		if p.kind == KindQuick && url == "" {
			url = quickURLPattern.FindString(line)
		}
		if registeredPattern.MatchString(line) {
			registered = true
		}
		if strings.Contains(line, " ERR ") {
			p.noteError(line)
		}
		if !connected && registered && url != "" {
			connected = true
			p.setState(tunnel.StateRunning, url, "")
			select {
			case ready <- url:
			default:
			}
		}
	}
	// Drain whatever is left so cloudflared never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, r)
	return connected
}

func (p *provider) noteError(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != tunnel.StateRunning {
		if _, msg, ok := strings.Cut(line, " ERR "); ok {
			p.lastErr = strings.TrimSpace(msg)
		}
	}
}

func (p *provider) setState(state tunnel.State, url, lastErr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state, p.url, p.lastErr = state, url, lastErr
}

// Stop terminates cloudflared and its supervisor.
func (p *provider) Stop(ctx context.Context) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.setState(tunnel.StateStopped, "", "")
	return nil
}

// Status implements tunnel.TunnelProvider.
func (p *provider) Status() tunnel.TunnelStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return tunnel.TunnelStatus{Provider: p.kind, State: p.state, URL: p.url, Error: p.lastErr}
}
