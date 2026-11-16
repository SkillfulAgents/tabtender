// Package runtimetest provides a fake BrowserRuntime for tests. Its
// "browsers" are in-process WebSocket echo servers, which makes it possible
// to exercise the session manager and the CDP proxy without a real browser —
// and demonstrates that the core depends only on the runtime interfaces.
package runtimetest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/SkillfulAgents/tabtender/internal/runtime"
)

// Kind is the kind the fake registers under.
const Kind = "fake"

// firstPID is far above any real PID so fake PIDs never match a process.
const firstPID = 90_000_000

// Runtime is a scriptable in-memory BrowserRuntime.
type Runtime struct {
	mu        sync.Mutex
	instances []*Instance
	nextPID   int
	// LaunchErr, when set, makes Launch fail.
	LaunchErr error
	// Gate, when non-nil, blocks Launch until it is closed.
	Gate chan struct{}
	// CloseErr, when set, is returned by Close (after stopping the browser).
	CloseErr error
}

// New returns an empty fake runtime.
func New() *Runtime { return &Runtime{nextPID: firstPID} }

// Register adds the fake to a registry under Kind; every Create call returns
// the same runtime so tests can inspect it.
func (r *Runtime) Register(registry *runtime.Registry) {
	registry.Register(Kind, func(runtime.RuntimeConfig) (runtime.BrowserRuntime, error) { return r, nil })
}

// Instances returns every browser launched so far.
func (r *Runtime) Instances() []*Instance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Instance(nil), r.instances...)
}

// Running returns the number of browsers currently alive.
func (r *Runtime) Running() int {
	n := 0
	for _, inst := range r.Instances() {
		if inst.IsRunning() {
			n++
		}
	}
	return n
}

// Launch implements runtime.BrowserRuntime.
func (r *Runtime) Launch(ctx context.Context, opts runtime.BrowserOptions) (runtime.BrowserInstance, error) {
	r.mu.Lock()
	gate, launchErr := r.Gate, r.LaunchErr
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if launchErr != nil {
		return nil, launchErr
	}
	if err := os.MkdirAll(opts.ProfileDir, 0o700); err != nil {
		return nil, err
	}
	inst := &Instance{Options: opts}
	inst.running.Store(true)
	inst.server = httptest.NewServer(http.HandlerFunc(inst.serveCDP))

	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextPID++
	inst.pid = r.nextPID
	r.instances = append(r.instances, inst)
	return inst, nil
}

// Close implements runtime.BrowserRuntime.
func (r *Runtime) Close(_ context.Context, bi runtime.BrowserInstance) error {
	inst, ok := bi.(*Instance)
	if !ok {
		return errors.New("runtimetest: foreign instance")
	}
	inst.Crash()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.CloseErr
}

// Instance is a fake browser.
type Instance struct {
	// Options are the launch options the browser was started with.
	Options runtime.BrowserOptions

	pid     int
	server  *httptest.Server
	running atomic.Bool
	stop    sync.Once

	mu    sync.Mutex
	conns []*websocket.Conn
	// Handshakes records the headers of each CDP handshake the browser saw.
	handshakes []http.Header
}

// CDPEndpoint implements runtime.BrowserInstance.
func (i *Instance) CDPEndpoint() string {
	return "ws" + strings.TrimPrefix(i.server.URL, "http") + "/devtools/browser/fake"
}

// PID implements runtime.BrowserInstance.
func (i *Instance) PID() int { return i.pid }

// IsRunning implements runtime.BrowserInstance.
func (i *Instance) IsRunning() bool { return i.running.Load() }

// Crash makes the browser exit abruptly.
func (i *Instance) Crash() {
	i.stop.Do(func() {
		i.running.Store(false)
		i.mu.Lock()
		conns := i.conns
		i.conns = nil
		i.mu.Unlock()
		for _, c := range conns {
			_ = c.CloseNow()
		}
		i.server.CloseClientConnections()
		i.server.Close()
	})
}

// Handshakes returns the headers of every CDP handshake received.
func (i *Instance) Handshakes() []http.Header {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]http.Header(nil), i.handshakes...)
}

// CloseClients closes every CDP connection with the given status, as a
// browser does when it shuts a debugging session.
func (i *Instance) CloseClients(code websocket.StatusCode, reason string) {
	i.mu.Lock()
	conns := i.conns
	i.conns = nil
	i.mu.Unlock()
	for _, c := range conns {
		_ = c.Close(code, reason)
	}
}

// serveCDP echoes every message back with its original type.
func (i *Instance) serveCDP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/devtools/browser/fake" {
		http.NotFound(w, r)
		return
	}
	i.mu.Lock()
	i.handshakes = append(i.handshakes, r.Header.Clone())
	i.mu.Unlock()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(-1)
	i.mu.Lock()
	i.conns = append(i.conns, conn)
	i.mu.Unlock()
	defer conn.CloseNow() //nolint:errcheck // test helper
	ctx := context.Background()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if err := conn.Write(ctx, typ, data); err != nil {
			return
		}
	}
}
