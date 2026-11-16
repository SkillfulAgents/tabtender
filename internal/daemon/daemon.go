// Package daemon wires the Tabtender components together and runs them.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/SkillfulAgents/tabtender/internal/api"
	"github.com/SkillfulAgents/tabtender/internal/auth"
	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/logging"
	"github.com/SkillfulAgents/tabtender/internal/power"
	"github.com/SkillfulAgents/tabtender/internal/runtime"
	"github.com/SkillfulAgents/tabtender/internal/runtime/chrome"
	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
	"github.com/SkillfulAgents/tabtender/internal/secrets"
	"github.com/SkillfulAgents/tabtender/internal/service"
	"github.com/SkillfulAgents/tabtender/internal/sessions"
	"github.com/SkillfulAgents/tabtender/internal/tunnel"
	"github.com/SkillfulAgents/tabtender/internal/tunnel/cloudflare"
	"github.com/SkillfulAgents/tabtender/internal/version"
)

const shutdownTimeout = 45 * time.Second

// Options control how the daemon runs.
type Options struct {
	// Launchd is set when started by launchd; it enables the
	// app.startOnLogin gate.
	Launchd bool
	// Stderr receives the human-readable log; nil disables it (as when
	// running as a Windows Service, which has no console).
	Stderr io.Writer
	// LookupEnv defaults to os.LookupEnv.
	LookupEnv config.LookupEnv
	// Runtimes and Tunnels default to the built-in adapters. They exist so
	// alternative adapters can be supplied without touching the core.
	Runtimes runtime.BrowserRuntimeFactory
	Tunnels  tunnel.TunnelFactory
}

// Daemon is a running Tabtender service.
type Daemon struct {
	opts   Options
	paths  config.Paths
	logger *slog.Logger
	env    config.LookupEnv

	store    *config.Store
	running  config.Config
	secrets  secrets.Store
	keys     *auth.KeyManager
	runtimes runtime.BrowserRuntimeFactory
	sessions *sessions.Manager
	tunnels  *tunnel.Manager
	power    *power.Inhibitor

	// applyMu serializes configuration changes.
	applyMu sync.Mutex
	applied config.Config
}

func (o *Options) defaults() {
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.Runtimes == nil {
		registry := runtime.NewRegistry()
		chrome.Register(registry)
		o.Runtimes = registry
	}
	if o.Tunnels == nil {
		registry := tunnel.NewRegistry()
		cloudflare.Register(registry)
		o.Tunnels = registry
	}
}

// Run starts the daemon and blocks until ctx is canceled.
func Run(ctx context.Context, opts Options) error {
	opts.defaults()
	paths, err := config.ResolvePaths(opts.LookupEnv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	logger, logBuffer, logCloser, err := logging.Setup(paths.LogFile(), logLevel(opts.LookupEnv), opts.Stderr)
	if err != nil {
		return err
	}
	defer logCloser.Close() //nolint:errcheck // nothing to do at exit

	err = run(ctx, opts, paths, logger, logBuffer)
	if err != nil {
		logger.Error("tabtenderd stopped with an error", "error", err)
	}
	return err
}

func logLevel(lookup config.LookupEnv) slog.Level {
	level := slog.LevelInfo
	if raw, ok := lookup("TABTENDER_LOG_LEVEL"); ok {
		_ = level.UnmarshalText([]byte(raw))
	}
	return level
}

func run(ctx context.Context, opts Options, paths config.Paths, logger *slog.Logger, logBuffer *logging.Buffer) error {
	// One daemon per data directory: reconciliation terminates browsers
	// found there, so two instances must never share it.
	lock := flock.New(filepath.Join(paths.DataDir, "tabtenderd.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("acquire instance lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("another tabtenderd is already running with data directory %s", paths.DataDir)
	}
	defer lock.Unlock() //nolint:errcheck // released by the OS at exit anyway

	store, err := config.Load(paths.ConfigFile, opts.LookupEnv)
	if err != nil {
		return err
	}
	cfg := store.Snapshot().Effective

	if opts.Launchd && !cfg.App.StartOnLogin && !service.ManualStartRequested(paths.StateDir()) {
		logger.Info("app.startOnLogin is disabled and no manual start was requested; exiting")
		return nil
	}
	if err := platform.ContainDescendants(); err != nil {
		logger.Warn("could not contain child processes; relying on reconciliation", "error", err)
	}

	d := &Daemon{
		opts:     opts,
		paths:    paths,
		logger:   logger,
		env:      opts.LookupEnv,
		store:    store,
		running:  cfg,
		applied:  cfg,
		runtimes: opts.Runtimes,
		power:    power.NewInhibitor(cfg.App.PreventSleep),
	}
	defer d.power.Close()

	if err := d.setupSecrets(); err != nil {
		return err
	}

	// Bind before doing anything expensive so configuration mistakes (port
	// in use, bad address) surface immediately.
	addr := net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer listener.Close() //nolint:errcheck // closed by http.Server too
	listenAddr := listener.Addr().String()

	rt, err := d.newRuntime(cfg)
	if err != nil {
		return err
	}
	d.sessions = sessions.NewManager(sessions.Options{
		ProfilesDir:    paths.ProfilesDir(),
		Journal:        sessions.NewJournal(filepath.Join(paths.StateDir(), "sessions.json")),
		Runtime:        rt,
		Limits:         limitsFrom(cfg),
		Logger:         logger,
		OnActiveChange: func(active int) { d.power.SetActive(active > 0) },
	})
	report, err := d.sessions.Reconcile(ctx)
	if err != nil {
		logger.Warn("reconciliation finished with errors", "error", err)
	}
	logger.Info("reconciled previous state",
		"recordedSessions", report.RecordedSessions,
		"browsersTerminated", report.BrowsersTerminated,
		"foreignProcessesSkipped", report.ForeignProcessesSkipped,
		"ephemeralProfilesRemoved", report.ProfilesRemoved)

	_, portStr, _ := net.SplitHostPort(listenAddr)
	port, _ := strconv.Atoi(portStr)
	d.tunnels = tunnel.NewManager(opts.Tunnels, port, logger)
	if err := d.tunnels.Apply(cfg.Tunnel.Provider, d.tunnelConfig(cfg)); err != nil {
		return fmt.Errorf("configure tunnel: %w", err)
	}

	server := api.NewServer(api.Deps{
		Sessions:   d.sessions,
		Keys:       d.keys,
		Tunnel:     d.tunnels,
		Backend:    d,
		Logs:       logBuffer,
		Logger:     logger,
		Paths:      paths,
		Version:    version.Version,
		ListenAddr: listenAddr,
	})

	public := &http.Server{Handler: server.PublicHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	servers := []*http.Server{public}
	serveErr := make(chan error, 2)
	go func() { serveErr <- public.Serve(listener) }()

	if api.ManagementSupported {
		socket := paths.ManagementSocket()
		if v, ok := d.env(config.EnvMgmtSocket); ok && v != "" {
			socket = v
		}
		mgmtListener, err := api.ListenManagement(socket, logger)
		if err != nil {
			return err
		}
		defer os.Remove(socket) //nolint:errcheck // best-effort cleanup
		mgmt := &http.Server{Handler: server.ManagementHandler(), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, mgmt)
		go func() { serveErr <- mgmt.Serve(mgmtListener) }()
		logger.Info("management channel ready", "socket", socket)
	}

	logger.Info("tabtenderd started",
		"version", version.Version,
		"address", listenAddr,
		"config", paths.ConfigFile,
		"dataDir", paths.DataDir,
		"apiKeySource", d.keys.Source(),
		"browserRuntime", cfg.Browser.Runtime,
		"tunnel", cfg.Tunnel.Provider)
	if !isLoopback(cfg.Server.Host) {
		logger.Warn("the API is listening on a non-loopback address; make sure this is intended", "host", cfg.Server.Host)
	}

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("server failed: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Stop sessions first: this also closes every proxied CDP connection.
	d.sessions.Shutdown(shutdownCtx)
	for _, srv := range servers {
		httpCtx, httpCancel := context.WithTimeout(shutdownCtx, 5*time.Second)
		_ = srv.Shutdown(httpCtx)
		httpCancel()
	}
	d.tunnels.Stop()
	if opts.Launchd && ctx.Err() != nil {
		service.ClearManualStart(paths.StateDir())
	}
	logger.Info("tabtenderd stopped")
	return runErr
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (d *Daemon) setupSecrets() error {
	backend, _ := d.env(config.EnvSecretsBackend)
	store, err := secrets.Open(backend, d.paths.SecretsDir())
	if err != nil {
		return err
	}
	d.secrets = store
	// Administrator-supplied secrets dropped into the secrets directory
	// (the provisioning path on hosts without a management UI).
	if _, err := os.Stat(d.paths.SecretsDir()); err == nil {
		if err := secrets.ImportProvisioned(store, d.paths.SecretsDir(), d.logger, secrets.APIKey, secrets.TunnelToken); err != nil {
			return err
		}
	}
	envKey, _ := d.env(config.EnvAPIKey)
	keys, err := auth.NewKeyManager(store, envKey)
	if err != nil {
		return err
	}
	d.keys = keys
	if goruntime.GOOS == "windows" {
		// Windows has no management UI: a generated key can be collected
		// once from an administrators-only file.
		if err := keys.EnableInitialKeyFile(filepath.Join(d.paths.SecretsDir(), "initial-api-key.txt")); err != nil {
			return err
		}
	}
	return nil
}

func limitsFrom(cfg config.Config) sessions.Limits {
	return sessions.Limits{
		DefaultTTL:      time.Duration(cfg.Sessions.DefaultTTL) * time.Second,
		MaxTTL:          time.Duration(cfg.Sessions.MaxTTL) * time.Second,
		MaxConcurrent:   cfg.Sessions.MaxConcurrent,
		DefaultHeadless: cfg.Browser.Headless,
		// A Windows Service lives in the non-interactive session 0, where
		// browser windows cannot be shown.
		HeadedSupported: !service.IsWindowsService(),
	}
}

func (d *Daemon) newRuntime(cfg config.Config) (runtime.BrowserRuntime, error) {
	return d.runtimes.Create(cfg.Browser.Runtime, runtime.RuntimeConfig{
		ExecutablePath: cfg.Browser.ExecutablePath,
		ExtraArgs:      cfg.Browser.ExtraArgs,
		Logger:         d.logger,
	})
}

func (d *Daemon) tunnelToken() (token, source string) {
	if v, ok := d.env(config.EnvTunnelToken); ok && v != "" {
		return v, auth.SourceEnvironment
	}
	if v, err := d.secrets.Get(secrets.TunnelToken); err == nil && v != "" {
		return v, d.secrets.Backend()
	}
	return "", ""
}

func (d *Daemon) tunnelConfig(cfg config.Config) tunnel.TunnelConfig {
	token, _ := d.tunnelToken()
	return tunnel.TunnelConfig{
		ExecutablePath: cfg.Tunnel.Cloudflare.ExecutablePath,
		Hostname:       cfg.Tunnel.Cloudflare.Hostname,
		Token:          token,
		LocalHost:      cfg.Server.Host,
		BinDir:         d.paths.BinDir(),
		StateDir:       d.paths.StateDir(),
		Logger:         d.logger,
	}
}

// Config implements api.Backend.
func (d *Daemon) Config() config.Snapshot { return d.store.Snapshot() }

// RunningConfig implements api.Backend.
func (d *Daemon) RunningConfig() config.Config { return d.running }

// UpdateConfig implements api.Backend: it persists the new file layer and
// applies every setting that can change without a restart.
func (d *Daemon) UpdateConfig(file config.Config) (config.Snapshot, error) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()

	// Validate everything — values and adapter choices — before anything is
	// written to disk, and report all problems at once.
	candidate := file
	overrides := d.store.Snapshot().Overrides
	if o, ok := overrides["browser.runtime"]; ok {
		candidate.Browser.Runtime = o.Value
	}
	if o, ok := overrides["tunnel.provider"]; ok {
		candidate.Tunnel.Provider = o.Value
	}
	problems := []error{file.Validate()}
	if _, err := d.newRuntime(candidate); err != nil {
		problems = append(problems, err)
	}
	if _, err := d.opts.Tunnels.Create(candidate.Tunnel.Provider, d.tunnelConfig(candidate)); err != nil {
		problems = append(problems, err)
	}
	if err := errors.Join(problems...); err != nil {
		return config.Snapshot{}, err
	}

	snap, err := d.store.Save(file)
	if err != nil {
		return config.Snapshot{}, err
	}
	next, prev := snap.Effective, d.applied
	d.applied = next

	d.sessions.SetLimits(limitsFrom(next))
	d.power.SetEnabled(next.App.PreventSleep)

	if next.Browser.Runtime != prev.Browser.Runtime ||
		next.Browser.ExecutablePath != prev.Browser.ExecutablePath ||
		!slices.Equal(next.Browser.ExtraArgs, prev.Browser.ExtraArgs) {
		rt, err := d.newRuntime(next)
		if err != nil {
			return snap, err
		}
		d.sessions.SetRuntime(rt)
		d.logger.Info("browser runtime reconfigured", "runtime", next.Browser.Runtime)
	}
	if next.Tunnel != prev.Tunnel {
		if err := d.tunnels.Apply(next.Tunnel.Provider, d.tunnelConfig(next)); err != nil {
			return snap, fmt.Errorf("configuration saved, but the tunnel could not be reconfigured: %w", err)
		}
		d.logger.Info("tunnel reconfigured", "provider", next.Tunnel.Provider)
	}
	if d.opts.Launchd && !next.App.StartOnLogin {
		// The service is running now and should keep running (including
		// across crash restarts) until it is stopped.
		if err := service.RequestManualStart(d.paths.StateDir()); err != nil {
			d.logger.Warn("could not record manual start", "error", err)
		}
	}
	d.logger.Info("configuration updated")
	return snap, nil
}

// TunnelTokenSource implements api.Backend.
func (d *Daemon) TunnelTokenSource() string {
	_, source := d.tunnelToken()
	return source
}

// SetTunnelToken implements api.Backend.
func (d *Daemon) SetTunnelToken(token string) error {
	if v, ok := d.env(config.EnvTunnelToken); ok && v != "" {
		return fmt.Errorf("the tunnel token is set by %s: %w", config.EnvTunnelToken, config.ErrEnvironmentControlled)
	}
	var err error
	if token == "" {
		err = d.secrets.Delete(secrets.TunnelToken)
	} else {
		err = d.secrets.Set(secrets.TunnelToken, token)
	}
	if err != nil {
		return fmt.Errorf("store tunnel token: %w", err)
	}
	return d.RestartTunnel()
}

// RestartTunnel implements api.Backend.
func (d *Daemon) RestartTunnel() error {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	return d.tunnels.Restart(d.tunnelConfig(d.applied))
}
