package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	// keepaliveInterval keeps idle CDP connections alive through tunnels
	// and proxies that drop quiet WebSockets (Cloudflare does after 100s).
	keepaliveInterval = 30 * time.Second
	keepaliveTimeout  = 20 * time.Second
	closeHandshake    = 3 * time.Second
	copyBufferSize    = 32 << 10
)

// upstreamClient reaches browser CDP endpoints, which are always on
// loopback, so proxy settings from the environment must not apply.
var upstreamClient = &http.Client{Transport: &http.Transport{Proxy: nil}}

var copyBuffers = sync.Pool{New: func() any {
	buf := make([]byte, copyBufferSize)
	return &buf
}}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// handleCDP bridges a client WebSocket to the session's browser. Messages
// are relayed verbatim, frame type included, and are never parsed.
func (s *Server) handleCDP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isWebSocketUpgrade(r) {
		w.Header().Set("Upgrade", "websocket")
		writeError(w, http.StatusUpgradeRequired, "upgrade_required",
			"this endpoint only accepts WebSocket connections")
		return
	}
	attachment, err := s.Sessions.Attach(id)
	if err != nil {
		s.writeSessionError(w, err)
		return
	}
	defer attachment.Release()

	// The proxy outlives the HTTP request context (the connection is
	// hijacked) and ends when either peer or the session goes away.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	upstream, _, err := websocket.Dial(dialCtx, attachment.Endpoint, &websocket.DialOptions{
		HTTPClient:      upstreamClient,
		CompressionMode: websocket.CompressionDisabled,
	})
	dialCancel()
	if err != nil {
		s.Logger.Warn("failed to reach browser CDP endpoint", "session", id, "error", err)
		writeError(w, http.StatusBadGateway, "browser_unreachable", "the session's browser is not reachable")
		return
	}
	defer upstream.CloseNow() //nolint:errcheck // idempotent cleanup

	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin checks protect cookie-authenticated endpoints from
		// cross-site WebSocket hijacking. This endpoint authenticates
		// with a bearer token that browsers cannot attach to WebSocket
		// handshakes, so any origin is acceptable.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept already wrote the error response.
	}
	defer client.CloseNow() //nolint:errcheck // idempotent cleanup

	// CDP messages (screenshots, traces) can be very large.
	client.SetReadLimit(-1)
	upstream.SetReadLimit(-1)

	s.Logger.Debug("cdp client connected", "session", id)
	reason := relay(ctx, client, upstream, attachment.Done)
	s.Logger.Debug("cdp client disconnected", "session", id, "reason", reason)
}

// relay pumps messages in both directions until one side closes or the
// session ends, then propagates the close to the other side.
func relay(ctx context.Context, client, upstream *websocket.Conn, sessionDone <-chan struct{}) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	clientErr := make(chan error, 1)
	upstreamErr := make(chan error, 1)
	go func() { clientErr <- pump(ctx, upstream, client) }()
	go func() { upstreamErr <- pump(ctx, client, upstream) }()
	go keepalive(ctx, client, cancel)

	select {
	case <-sessionDone:
		closeWith(client, websocket.StatusGoingAway, "session terminated")
		return "session terminated"
	case err := <-clientErr:
		code, reason := closeInfo(err)
		closeWith(upstream, code, reason)
		return "client closed: " + describe(err)
	case err := <-upstreamErr:
		// The browser going away usually means the session is ending; give
		// that signal a moment to win so clients get the precise reason.
		select {
		case <-sessionDone:
			closeWith(client, websocket.StatusGoingAway, "session terminated")
			return "session terminated"
		case <-time.After(250 * time.Millisecond):
		}
		code, reason := closeInfo(err)
		closeWith(client, code, reason)
		return "browser closed: " + describe(err)
	case <-ctx.Done():
		closeWith(client, websocket.StatusGoingAway, "connection lost")
		return "keepalive failed"
	}
}

// pump copies whole messages from src to dst, streaming their payloads.
func pump(ctx context.Context, dst, src *websocket.Conn) error {
	bufp := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(bufp)
	for {
		typ, reader, err := src.Reader(ctx)
		if err != nil {
			return err
		}
		writer, err := dst.Writer(ctx, typ)
		if err != nil {
			return err
		}
		if _, err := io.CopyBuffer(writer, reader, *bufp); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
	}
}

func keepalive(ctx context.Context, client *websocket.Conn, fail context.CancelFunc) {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, keepaliveTimeout)
			err := client.Ping(pingCtx)
			cancel()
			if err != nil {
				fail()
				return
			}
		}
	}
}

// closeInfo maps a pump error to the close code to forward to the peer.
func closeInfo(err error) (websocket.StatusCode, string) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.StatusNoStatusRcvd, websocket.StatusAbnormalClosure, websocket.StatusTLSHandshake:
			// Reserved codes that must not be sent on the wire.
			return websocket.StatusNormalClosure, ""
		}
		return ce.Code, ce.Reason
	}
	return websocket.StatusGoingAway, "peer disconnected"
}

func describe(err error) string {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code.String()
	}
	if errors.Is(err, io.EOF) {
		return "connection closed"
	}
	return "connection error"
}

// closeWith performs a bounded close handshake.
func closeWith(conn *websocket.Conn, code websocket.StatusCode, reason string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = conn.Close(code, reason)
	}()
	select {
	case <-done:
	case <-time.After(closeHandshake):
		_ = conn.CloseNow()
	}
}
