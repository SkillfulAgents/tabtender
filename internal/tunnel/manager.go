package tunnel

import (
	"context"
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
	// RetryDelay is the pause between failed start attempts.
	retryDelay time.Duration

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

// SetRetryDelay overrides the pause between failed start attempts.
func (m *Manager) SetRetryDelay(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryDelay = d
}

// Apply stops the current provider and starts the requested one in the
// background. It returns an error only if the provider cannot be created.
func (m *Manager) Apply(kind string, cfg TunnelConfig) error {
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
