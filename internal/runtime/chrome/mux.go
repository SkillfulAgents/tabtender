package chrome

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/SkillfulAgents/tabtender/internal/runtime"
)

const (
	// maxQueuedBytes bounds what the browser may have produced for a client
	// that is not reading. Beyond it the client is disconnected, so one
	// stalled client can neither exhaust memory nor hold up the others.
	maxQueuedBytes = 256 << 20
	// sessionIDProbeLimit bounds the size of command results inspected for
	// a newly created CDP session. Such results are tiny.
	sessionIDProbeLimit = 1 << 10

	methodAttachToBrowser = "Target.attachToBrowserTarget"
	methodDetach          = "Target.detachFromTarget"
	eventAttached         = "Target.attachedToTarget"
	eventDetached         = "Target.detachedFromTarget"
)

var (
	errBrowserGone  = errors.New("the browser closed its debugging pipe")
	errSlowConsumer = errors.New("client fell too far behind the browser")
	errConnClosed   = errors.New("connection closed")
)

// mux shares a browser's debugging pipe between independent CDP clients.
//
// Chrome's debugging pipe is a single CDP connection, unreachable by anything
// but the process holding it — unlike a debugging port, which any local
// process can connect to. To still give every client a connection of its
// own, the mux opens one browser-level CDP session per client
// (Target.attachToBrowserTarget) and translates between the two views: what
// the client sends without a session ID runs in its browser session, and
// what the browser emits for that session reaches the client without one.
// Sessions the client attaches from there (pages, workers) are already
// addressed by ID and pass through untouched. Detaching the browser session
// when a client leaves makes Chrome drop that client's state, as closing a
// connection would.
type mux struct {
	in      io.WriteCloser // browser's command input
	writeMu sync.Mutex
	// queueLimit is maxQueuedBytes, lowered by tests.
	queueLimit int

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rootReply
	// routes maps a CDP session ID to the client it belongs to: each
	// client's browser session and every session attached beneath it.
	routes map[string]*muxConn
	err    error
	done   chan struct{}
}

type rootReply struct {
	result json.RawMessage
	err    error
}

// envelope is the part of a CDP message needed for routing.
type envelope struct {
	ID        *int64          `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type sessionRef struct {
	SessionID string `json:"sessionId"`
}

// newMux starts relaying. in and out are the browser's ends of the pipe as
// seen from here: commands are written to in, messages read from out.
func newMux(in io.WriteCloser, out io.Reader) *mux {
	m := &mux{
		in:         in,
		queueLimit: maxQueuedBytes,
		pending:    map[int64]chan rootReply{},
		routes:     map[string]*muxConn{},
		done:       make(chan struct{}),
	}
	go m.readLoop(out)
	return m
}

// Done is closed when the browser's pipe has closed.
func (m *mux) Done() <-chan struct{} { return m.done }

func (m *mux) readLoop(out io.Reader) {
	reader := bufio.NewReaderSize(out, 64<<10)
	var err error
	for {
		var msg []byte
		// Messages are NUL-terminated JSON documents.
		if msg, err = reader.ReadBytes(0); err != nil {
			break
		}
		m.dispatch(msg[:len(msg)-1])
	}
	m.shutdown(fmt.Errorf("%w: %v", errBrowserGone, err))
}

func (m *mux) shutdown(err error) {
	m.mu.Lock()
	if m.err != nil {
		m.mu.Unlock()
		return
	}
	m.err = err
	pending, routes := m.pending, m.routes
	m.pending, m.routes = map[int64]chan rootReply{}, map[string]*muxConn{}
	close(m.done)
	m.mu.Unlock()

	_ = m.in.Close()
	for _, ch := range pending {
		ch <- rootReply{err: err}
	}
	for _, c := range routes {
		c.fail(err)
	}
}

// Close stops the mux and disconnects every client.
func (m *mux) Close() { m.shutdown(errConnClosed) }

func (m *mux) dispatch(msg []byte) {
	var env envelope
	if err := json.Unmarshal(msg, &env); err != nil {
		return
	}
	if env.SessionID == "" {
		m.dispatchRoot(env)
		return
	}
	m.mu.Lock()
	c := m.routes[env.SessionID]
	if c != nil {
		m.trackSessionsLocked(c, env)
	}
	m.mu.Unlock()
	if c == nil {
		return // A session of a client that has already left.
	}
	if env.SessionID == c.sessionID {
		// The client believes it is talking to the browser directly.
		stripped, err := withoutSessionID(msg)
		if err != nil {
			return
		}
		msg = stripped
	}
	c.deliver(msg)
}

func (m *mux) dispatchRoot(env envelope) {
	if env.ID == nil {
		return // Nothing subscribes to events on the root connection.
	}
	m.mu.Lock()
	ch := m.pending[*env.ID]
	delete(m.pending, *env.ID)
	m.mu.Unlock()
	if ch == nil {
		return
	}
	if env.Error != nil {
		ch <- rootReply{err: fmt.Errorf("browser: %s", env.Error.Message)}
		return
	}
	ch <- rootReply{result: env.Result}
}

// trackSessionsLocked keeps routes current as the client's sessions come and
// go, so that their messages find the client.
func (m *mux) trackSessionsLocked(c *muxConn, env envelope) {
	switch {
	case env.Method == eventAttached:
		var ref sessionRef
		if json.Unmarshal(env.Params, &ref) == nil && ref.SessionID != "" {
			m.routes[ref.SessionID] = c
		}
	case env.Method == eventDetached:
		var ref sessionRef
		if json.Unmarshal(env.Params, &ref) == nil && ref.SessionID != c.sessionID {
			delete(m.routes, ref.SessionID)
		}
	case env.ID != nil && len(env.Result) > 0 && len(env.Result) <= sessionIDProbeLimit:
		// Target.attachToTarget and Target.attachToBrowserTarget answer
		// with the new session's ID.
		var ref sessionRef
		if json.Unmarshal(env.Result, &ref) == nil && ref.SessionID != "" {
			m.routes[ref.SessionID] = c
		}
	}
}

// call runs a command on the root connection.
func (m *mux) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan rootReply, 1)
	m.mu.Lock()
	if m.err != nil {
		m.mu.Unlock()
		return nil, m.err
	}
	m.nextID++
	id := m.nextID
	m.pending[id] = ch
	m.mu.Unlock()

	msg, err := json.Marshal(struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{id, method, params})
	if err == nil {
		err = m.write(msg)
	}
	if err != nil {
		m.forget(id)
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		m.forget(id)
		return nil, ctx.Err()
	}
}

func (m *mux) forget(id int64) {
	m.mu.Lock()
	delete(m.pending, id)
	m.mu.Unlock()
}

func (m *mux) write(msg []byte) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	buf := make([]byte, 0, len(msg)+1)
	buf = append(append(buf, msg...), 0)
	if _, err := m.in.Write(buf); err != nil {
		return fmt.Errorf("%w: %v", errBrowserGone, err)
	}
	return nil
}

// Connect opens a CDP connection for one client.
func (m *mux) Connect(ctx context.Context) (runtime.CDPConn, error) {
	result, err := m.call(ctx, methodAttachToBrowser, nil)
	if err != nil {
		return nil, err
	}
	var ref sessionRef
	if err := json.Unmarshal(result, &ref); err != nil || ref.SessionID == "" {
		return nil, errors.New("browser did not open a session")
	}
	c := &muxConn{mux: m, sessionID: ref.SessionID, wake: make(chan struct{}, 1)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	m.routes[c.sessionID] = c
	return c, nil
}

// release forgets a client and tells the browser to drop its session, which
// also detaches everything the client had attached through it.
func (m *mux) release(c *muxConn) {
	m.mu.Lock()
	for id, owner := range m.routes {
		if owner == c {
			delete(m.routes, id)
		}
	}
	gone := m.err != nil
	m.nextID++
	id := m.nextID
	m.mu.Unlock()
	if gone {
		return
	}
	// The answer is of no interest; an unmatched reply is discarded.
	msg, err := json.Marshal(struct {
		ID     int64      `json:"id"`
		Method string     `json:"method"`
		Params sessionRef `json:"params"`
	}{id, methodDetach, sessionRef{c.sessionID}})
	if err == nil {
		_ = m.write(msg)
	}
}

// muxConn is one client's view of the browser.
type muxConn struct {
	mux       *mux
	sessionID string

	mu     sync.Mutex
	queue  [][]byte
	queued int
	err    error
	wake   chan struct{}
	once   sync.Once
}

func (c *muxConn) deliver(msg []byte) {
	c.mu.Lock()
	if c.err == nil {
		if c.queued+len(msg) > c.mux.queueLimit {
			c.err = errSlowConsumer
			c.queue, c.queued = nil, 0
		} else {
			c.queue = append(c.queue, msg)
			c.queued += len(msg)
		}
	}
	c.mu.Unlock()
	c.signal()
}

func (c *muxConn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.signal()
}

func (c *muxConn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Read returns the next message from the browser. Messages already queued
// are delivered before a failure is reported.
func (c *muxConn) Read(ctx context.Context) ([]byte, error) {
	for {
		c.mu.Lock()
		if len(c.queue) > 0 {
			msg := c.queue[0]
			c.queue[0] = nil
			c.queue = c.queue[1:]
			c.queued -= len(msg)
			c.mu.Unlock()
			return msg, nil
		}
		err := c.err
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		select {
		case <-c.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Write sends one CDP message to the browser.
func (c *muxConn) Write(_ context.Context, msg []byte) error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return err
	}
	scoped, err := withSessionID(msg, c.sessionID)
	if err != nil {
		return err
	}
	return c.mux.write(scoped)
}

// Close disconnects the client. The browser keeps running.
func (c *muxConn) Close() error {
	c.once.Do(func() {
		c.fail(errConnClosed)
		c.mux.release(c)
	})
	return nil
}

// withSessionID addresses a client message to the client's browser session,
// unless it already names a session.
func withSessionID(msg []byte, sessionID string) ([]byte, error) {
	// NUL frames messages on the pipe and never occurs in valid JSON.
	if bytes.IndexByte(msg, 0) >= 0 {
		return nil, runtime.ErrInvalidMessage
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(msg, &fields); err != nil {
		return nil, runtime.ErrInvalidMessage
	}
	if raw, ok := fields["sessionId"]; ok {
		var existing string
		if json.Unmarshal(raw, &existing) != nil {
			return nil, runtime.ErrInvalidMessage
		}
		if existing != "" {
			return msg, nil
		}
	}
	id, err := json.Marshal(sessionID)
	if err != nil {
		return nil, err
	}
	fields["sessionId"] = id
	return json.Marshal(fields)
}

func withoutSessionID(msg []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(msg, &fields); err != nil {
		return nil, err
	}
	delete(fields, "sessionId")
	return json.Marshal(fields)
}
