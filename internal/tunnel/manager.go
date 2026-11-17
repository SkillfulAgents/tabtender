package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Manager runs the currently selected provider and lets it be swapped at
// runtime. It retries providers that fail to start (for example because the
// network is not up yet at login).
type Manager struct {
	factory   TunnelFactory
	localPort int
	logger    *slog.Logger
	// retryDelay is the pause after the first failed start attempt; it
	// doubles with every further failure, up to maxRetryDelay.
	retryDelay time.Duration

	// opMu serializes provider changes, so a provider can never be started
	// while another change — or the final stop — is tearing one down.
	opMu   sync.Mutex
	closed bool

	mu       sync.Mutex
	kind     string
	provider TunnelProvider
	startErr string
	starting bool
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewManager returns a Manager with the "none" provider active.
func NewManager(factory TunnelFactory, localPort int, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Manager{
		factory:    factory,
		localPort:  localPort,
		logger:     logger,
		retryDelay: 15 * time.Second,
		kind:       KindNone,
		provider:   noneProvider{},
	}
}

// maxRetryDelay caps the pause between start attempts. A provider that keeps
// failing (no network, a missing token, a download that never verifies) is
// retried indefinitely, but not in a tight loop.
const maxRetryDelay = 10 * time.Minute

// ErrClosed is returned when a provider is applied after Close.
var ErrClosed = errors.New("tunnel manager is closed")

// SetRetryDelay overrides the pause after the first failed start attempt.
func (m *Manager) SetRetryDelay(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryDelay = d
}

// Apply stops the current provider and starts the requested one in the
// background. It returns an error only if the provider cannot be created.
func (m *Manager) Apply(kind string, cfg TunnelConfig) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.closed {
		return ErrClosed
	}
	provider, err := m.factory.Create(kind, cfg)
	if err != nil {
		return err
	}
	m.stopCurrent()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	m.kind, m.provider, m.startErr, m.starting = kind, provider, "", true
	m.cancel, m.done = cancel, done
	retryDelay := m.retryDelay
	m.mu.Unlock()

	go m.run(ctx, done, kind, provider, retryDelay)
	return nil
}

func (m *Manager) run(ctx context.Context, done chan struct{}, kind string, provider TunnelProvider, retryDelay time.Duration) {
	defer close(done)
	for {
		url, err := provider.Start(ctx, m.localPort)
		m.mu.Lock()
		if m.provider == provider {
			m.starting = false
			m.startErr = ""
			if err != nil {
				m.startErr = err.Error()
			}
		}
		m.mu.Unlock()
		if err == nil {
			if url != "" {
				m.logger.Info("tunnel established", "provider", kind, "url", url)
			}
			<-ctx.Done()
			return
		}
		if ctx.Err() != nil {
			return
		}
		m.logger.Error("tunnel failed to start; will retry", "provider", kind, "error", err, "retryIn", retryDelay.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
		retryDelay = min(retryDelay*2, max(maxRetryDelay, retryDelay))
		m.mu.Lock()
		if m.provider == provider {
			m.starting = true
		}
		m.mu.Unlock()
	}
}

func (m *Manager) stopCurrent() {
	m.mu.Lock()
	provider, cancel, done := m.provider, m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	if err := provider.Stop(stopCtx); err != nil {
		m.logger.Warn("tunnel provider did not stop cleanly", "error", err)
	}
}

// Restart re-applies the current provider with a new configuration.
func (m *Manager) Restart(cfg TunnelConfig) error {
	m.mu.Lock()
	kind := m.kind
	m.mu.Unlock()
	return m.Apply(kind, cfg)
}

// Stop shuts the active provider down and reverts to local-only access.
func (m *Manager) Stop() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.stopLocked()
}

// Close stops the active provider for good: later Apply calls fail with
// ErrClosed.
func (m *Manager) Close() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.closed = true
	m.stopLocked()
}

func (m *Manager) stopLocked() {
	m.stopCurrent()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kind, m.provider, m.startErr, m.starting = KindNone, noneProvider{}, "", false
}

// Status reports the active provider's status.
func (m *Manager) Status() TunnelStatus {
	m.mu.Lock()
	provider, kind, startErr, starting := m.provider, m.kind, m.startErr, m.starting
	m.mu.Unlock()
	status := provider.Status()
	status.Provider = kind
	switch {
	case kind == KindNone:
	case starting && status.State != StateRunning:
		status.State = StateStarting
	case startErr != "":
		status.State, status.Error, status.URL = StateError, startErr, ""
	}
	return status
}

// PublicURL returns the external base URL, or "" when no tunnel is running.
func (m *Manager) PublicURL() string {
	if status := m.Status(); status.State == StateRunning {
		return status.URL
	}
	return ""
}
