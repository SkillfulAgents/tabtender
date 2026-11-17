// Package runtimetest provides a fake BrowserRuntime for tests. Its
// "browsers" echo every CDP message back, which makes it possible
// to exercise the session manager and the CDP proxy without a real browser —
// and demonstrates that the core depends only on the runtime interfaces.
package runtimetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"

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
	running atomic.Bool
	stop    sync.Once

	mu          sync.Mutex
	conns       []*conn
	connections int
	connectErr  error
}

// Connect implements runtime.BrowserInstance.
func (i *Instance) Connect(context.Context) (runtime.CDPConn, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.connectErr != nil {
		return nil, i.connectErr
	}
	if !i.running.Load() {
		return nil, errors.New("runtimetest: browser is not running")
	}
	c := &conn{messages: make(chan []byte, 64), closed: make(chan struct{})}
	i.conns = append(i.conns, c)
	i.connections++
	return c, nil
}

// PID implements runtime.BrowserInstance.
func (i *Instance) PID() int { return i.pid }

// IsRunning implements runtime.BrowserInstance.
func (i *Instance) IsRunning() bool { return i.running.Load() }

// Crash makes the browser exit abruptly.
func (i *Instance) Crash() {
	i.stop.Do(func() {
		i.running.Store(false)
		i.DropClients()
	})
}

// FailConnect makes every later Connect fail with err.
func (i *Instance) FailConnect(err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.connectErr = err
}

// Connections returns how many CDP connections were ever opened.
func (i *Instance) Connections() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.connections
}

// DropClients ends every CDP connection from the browser's side.
func (i *Instance) DropClients() {
	i.mu.Lock()
	conns := i.conns
	i.conns = nil
	i.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// conn echoes every message back.
type conn struct {
	messages chan []byte
	closed   chan struct{}
	once     sync.Once
}

func (c *conn) Read(ctx context.Context) ([]byte, error) {
	select {
	case msg := <-c.messages:
		return msg, nil
	case <-c.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *conn) Write(ctx context.Context, msg []byte) error {
	// Like a real debugging pipe, which frames messages with NUL.
	if bytes.IndexByte(msg, 0) >= 0 {
		return runtime.ErrInvalidMessage
	}
	select {
	case c.messages <- append([]byte(nil), msg...):
		return nil
	case <-c.closed:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *conn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
