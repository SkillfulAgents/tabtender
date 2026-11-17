package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/SkillfulAgents/tabtender/internal/runtime"
)

const (
	// keepaliveInterval keeps idle CDP connections alive through tunnels
	// and proxies that drop quiet WebSockets (Cloudflare does after 100s).
	keepaliveInterval = 30 * time.Second
	keepaliveTimeout  = 20 * time.Second
	closeHandshake    = 3 * time.Second
	connectTimeout    = 10 * time.Second
	// sessionEndGrace is how long a lost browser connection waits for the
	// session to be declared over. It must exceed the interval at which
	// the session manager notices a browser that exited.
	sessionEndGrace = 1500 * time.Millisecond
)

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// handleCDP bridges a client WebSocket to a CDP connection of its own on the
// session's browser.
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

	connectCtx, connectCancel := context.WithTimeout(ctx, connectTimeout)
	browser, err := attachment.Connect(connectCtx)
	connectCancel()
	if err != nil {
		s.Logger.Warn("failed to reach the browser", "session", id, "error", err)
		writeError(w, http.StatusBadGateway, "browser_unreachable", "the session's browser is not reachable")
		return
	}
	defer browser.Close() //nolint:errcheck // idempotent cleanup

	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin checks protect cookie-authenticated endpoints from
		// cross-site WebSocket hijacking. This endpoint is authorized by
		// a secret the caller must present explicitly (the API key
		// header or the session token), never by ambient browser
		// credentials, so any origin is acceptable.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept already wrote the error response.
	}
	defer client.CloseNow() //nolint:errcheck // idempotent cleanup

	// CDP messages (screenshots, traces) can be very large.
	client.SetReadLimit(-1)

	s.Logger.Debug("cdp client connected", "session", id)
	reason := relay(ctx, client, browser, attachment.Done)
	s.Logger.Debug("cdp client disconnected", "session", id, "reason", reason)
}

// relay pumps messages in both directions until one side closes or the
// session ends, then closes the other side.
func relay(ctx context.Context, client *websocket.Conn, browser runtime.CDPConn, sessionDone <-chan struct{}) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	clientErr := make(chan error, 1)
	browserErr := make(chan error, 1)
	go func() { clientErr <- pumpToBrowser(ctx, browser, client) }()
	go func() { browserErr <- pumpToClient(ctx, client, browser) }()
	go keepalive(ctx, client, cancel)

	select {
	case <-sessionDone:
		closeWith(client, websocket.StatusGoingAway, "session terminated")
		return "session terminated"
	case err := <-clientErr:
		if errors.Is(err, runtime.ErrInvalidMessage) {
			closeWith(client, websocket.StatusInvalidFramePayloadData, "not a CDP message")
			return "client sent an invalid message"
		}
		return "client closed: " + describe(err)
	case err := <-browserErr:
		// The browser going away usually means the session is ending; give
		// that signal a moment to win so clients get the precise reason.
		select {
		case <-sessionDone:
			closeWith(client, websocket.StatusGoingAway, "session terminated")
			return "session terminated"
		case <-time.After(sessionEndGrace):
		}
		closeWith(client, websocket.StatusGoingAway, "browser disconnected")
		return "browser closed: " + err.Error()
	case <-ctx.Done():
		closeWith(client, websocket.StatusGoingAway, "connection lost")
		return "keepalive failed"
	}
}

// pumpToBrowser forwards client frames. CDP is JSON, so text and binary
// frames are treated alike.
func pumpToBrowser(ctx context.Context, browser runtime.CDPConn, client *websocket.Conn) error {
	for {
		_, msg, err := client.Read(ctx)
		if err != nil {
			return err
		}
		if err := browser.Write(ctx, msg); err != nil {
			return err
		}
	}
}

func pumpToClient(ctx context.Context, client *websocket.Conn, browser runtime.CDPConn) error {
	for {
		msg, err := browser.Read(ctx)
		if err != nil {
			return err
		}
		if err := client.Write(ctx, websocket.MessageText, msg); err != nil {
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
