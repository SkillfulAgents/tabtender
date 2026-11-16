// Package tunnel defines the tunnel provider abstraction and the manager
// that runs the configured provider. Providers expose the single Tabtender
// HTTP/WebSocket port to remote clients; the session and API layers only
// ever see the resulting public base URL.
package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// State is the lifecycle state of a tunnel.
type State string

// Tunnel states.
const (
	// StateDisabled means no tunnel is configured (local-only access).
	StateDisabled State = "disabled"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateError    State = "error"
	StateStopped  State = "stopped"
)

// TunnelStatus describes the tunnel for health reporting.
type TunnelStatus struct {
	Provider string `json:"provider"`
	State    State  `json:"state"`
	// URL is the externally reachable base URL while the tunnel is running.
	URL   string `json:"url"`
	Error string `json:"error"`
}

// TunnelConfig configures a provider. Fields that a provider does not need
// are ignored by it.
type TunnelConfig struct {
	// ExecutablePath locates the provider's helper binary ("auto" to manage it).
	ExecutablePath string
	// Hostname is the public hostname of a persistent tunnel.
	Hostname string
	// Token authenticates a persistent tunnel.
	Token string
	// LocalHost is the address the Tabtender server listens on.
	LocalHost string
	// BinDir is where managed helper binaries are installed.
	BinDir string
	// StateDir holds provider bookkeeping such as PID files.
	StateDir string
	Logger   *slog.Logger
}

// TunnelProvider exposes a local port through a tunnel.
type TunnelProvider interface {
	// Start brings the tunnel up and returns its external base URL (empty
	// for providers without one).
	Start(ctx context.Context, localPort int) (string, error)
	Stop(ctx context.Context) error
	Status() TunnelStatus
}

// TunnelFactory creates providers by kind.
type TunnelFactory interface {
	Create(kind string, cfg TunnelConfig) (TunnelProvider, error)
}

// Constructor builds a tunnel provider.
type Constructor func(cfg TunnelConfig) (TunnelProvider, error)

// Registry is a TunnelFactory backed by registered constructors.
type Registry struct {
	mu    sync.RWMutex
	ctors map[string]Constructor
}

// NewRegistry returns a registry containing only the "none" provider.
func NewRegistry() *Registry {
	r := &Registry{ctors: map[string]Constructor{}}
	r.Register(KindNone, func(TunnelConfig) (TunnelProvider, error) { return noneProvider{}, nil })
	return r
}

// Register adds a provider under kind, replacing any previous registration.
func (r *Registry) Register(kind string, ctor Constructor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctors[kind] = ctor
}

// Kinds lists the registered provider kinds.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	kinds := make([]string, 0, len(r.ctors))
	for k := range r.ctors {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// Create implements TunnelFactory.
func (r *Registry) Create(kind string, cfg TunnelConfig) (TunnelProvider, error) {
	r.mu.RLock()
	ctor, ok := r.ctors[kind]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown tunnel provider %q (available: %v)", kind, r.Kinds())
	}
	return ctor(cfg)
}

// KindNone is the provider kind for local-only access.
const KindNone = "none"

type noneProvider struct{}

func (noneProvider) Start(context.Context, int) (string, error) { return "", nil }
func (noneProvider) Stop(context.Context) error                 { return nil }
func (noneProvider) Status() TunnelStatus {
	return TunnelStatus{Provider: KindNone, State: StateDisabled}
}
