// Package runtime defines the browser runtime abstraction. The session
// manager and API layer depend only on these interfaces; concrete adapters
// (such as the Chrome adapter) register themselves with a Registry.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// BrowserOptions describes one browser launch.
type BrowserOptions struct {
	// Headless launches the browser without a visible window.
	Headless bool
	// ProfileDir is the browser profile (user data) directory. It is unique
	// per running browser.
	ProfileDir string
}

// RuntimeConfig configures a runtime adapter.
type RuntimeConfig struct {
	// ExecutablePath is the browser binary, or "auto" for discovery.
	ExecutablePath string
	// ExtraArgs are appended to the adapter's own launch flags.
	ExtraArgs []string
	// Launcher, when set, is a command prefix that starts the browser: the
	// adapter runs Launcher followed by the browser executable and its
	// arguments. The launcher must replace itself with the browser (exec),
	// keeping the process ID.
	Launcher []string
	// Logger receives adapter diagnostics; nil discards them.
	Logger *slog.Logger
}

// BrowserRuntime launches and stops browser processes.
type BrowserRuntime interface {
	Launch(ctx context.Context, opts BrowserOptions) (BrowserInstance, error)
	Close(ctx context.Context, instance BrowserInstance) error
}

// BrowserInstance is one running browser process.
type BrowserInstance interface {
	// Connect opens a CDP connection to the browser. Connections are
	// independent of each other: each sees the browser as a client that
	// connected on its own would.
	Connect(ctx context.Context) (CDPConn, error)
	PID() int
	IsRunning() bool
}

// ErrInvalidMessage is returned by CDPConn.Write for a message that is not a
// CDP message (a JSON object).
var ErrInvalidMessage = errors.New("not a valid CDP message")

// CDPConn is a browser-level CDP connection. Read and Write may be used
// concurrently with each other.
type CDPConn interface {
	// Read returns the next message from the browser.
	Read(ctx context.Context) ([]byte, error)
	// Write sends one message to the browser.
	Write(ctx context.Context, msg []byte) error
	// Close ends the connection. The browser keeps running.
	Close() error
}

// BrowserRuntimeFactory creates runtimes by kind.
type BrowserRuntimeFactory interface {
	Create(kind string, cfg RuntimeConfig) (BrowserRuntime, error)
}

// ExecutableResolver is optionally implemented by runtimes that can report
// which browser binary they will launch, for health reporting.
type ExecutableResolver interface {
	Executable() (string, error)
}

// Constructor builds a runtime adapter.
type Constructor func(cfg RuntimeConfig) (BrowserRuntime, error)

// Registry is a BrowserRuntimeFactory backed by registered constructors.
type Registry struct {
	mu    sync.RWMutex
	ctors map[string]Constructor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{ctors: map[string]Constructor{}} }

// Register adds an adapter under kind, replacing any previous registration.
func (r *Registry) Register(kind string, ctor Constructor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctors[kind] = ctor
}

// Kinds lists the registered adapter kinds.
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

// Create implements BrowserRuntimeFactory.
func (r *Registry) Create(kind string, cfg RuntimeConfig) (BrowserRuntime, error) {
	r.mu.RLock()
	ctor, ok := r.ctors[kind]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown browser runtime %q (available: %v)", kind, r.Kinds())
	}
	return ctor(cfg)
}
