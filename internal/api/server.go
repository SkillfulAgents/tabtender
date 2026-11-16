// Package api implements Tabtender's HTTP surface: the authenticated REST
// API, the CDP WebSocket proxy, and the local management API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/auth"
	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/logging"
	"github.com/SkillfulAgents/tabtender/internal/runtime"
	"github.com/SkillfulAgents/tabtender/internal/sessions"
	"github.com/SkillfulAgents/tabtender/internal/tunnel"
)

const maxRequestBody = 1 << 20

// Backend is what the API needs from the daemon beyond its direct
// dependencies: configuration state and the operations that mutate it.
type Backend interface {
	// Config returns the current configuration layers.
	Config() config.Snapshot
	// RunningConfig returns the effective configuration the process booted
	// with, used to tell which restart-required settings are pending.
	RunningConfig() config.Config
	// UpdateConfig validates, persists, and live-applies a new file layer.
	UpdateConfig(file config.Config) (config.Snapshot, error)
	// TunnelTokenSource reports where the Cloudflare tunnel token comes
	// from ("" when none is configured).
	TunnelTokenSource() string
	// SetTunnelToken stores a tunnel token ("" deletes it) and restarts the
	// tunnel.
	SetTunnelToken(token string) error
	// RestartTunnel restarts the active tunnel provider.
	RestartTunnel() error
}

// Deps are the collaborators of a Server.
type Deps struct {
	Sessions *sessions.Manager
	Keys     *auth.KeyManager
	Tunnel   *tunnel.Manager
	Backend  Backend
	Logs     *logging.Buffer
	Logger   *slog.Logger
	Paths    config.Paths
	Version  string
	// ListenAddr is the address of the public listener ("host:port").
	ListenAddr string
}

// Server builds the HTTP handlers.
type Server struct {
	Deps
	startedAt time.Time
}

// NewServer returns a Server.
func NewServer(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{Deps: deps, startedAt: time.Now()}
}

type contextKey int

const localChannelKey contextKey = iota

// viaManagement reports whether the request arrived on the local management
// channel rather than the network listener.
func viaManagement(r *http.Request) bool {
	v, _ := r.Context().Value(localChannelKey).(bool)
	return v
}

func (s *Server) registerSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /v1/sessions/{id}/cdp", s.handleCDP)
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
}

// PublicHandler serves the network-facing API. Every request, including CDP
// WebSocket handshakes, must carry a valid bearer token.
func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerSessionRoutes(mux)
	authenticate := auth.Middleware(s.Keys, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required: Authorization: Bearer <key>")
	})
	return s.logRequests("api", authenticate(mux))
}

// ManagementHandler serves the local management channel. Callers are
// authenticated by the transport (Unix socket peer credentials), so no
// bearer token is required.
func (s *Server) ManagementHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerSessionRoutes(mux)
	s.registerManagementRoutes(mux)
	return s.logRequests("mgmt", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), localChannelKey, true)))
	}))
}

// statusRecorder captures the response status for request logging while
// staying transparent to WebSocket upgrades.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests logs method, path, and outcome. Headers are never logged, so
// credentials cannot leak into the log.
func (s *Server) logRequests(channel string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelDebug
		if rec.status >= 500 {
			level = slog.LevelWarn
		}
		s.Logger.Log(r.Context(), level, "request", "channel", channel, "method", r.Method,
			"path", r.URL.Path, "status", rec.status, "duration", time.Since(start).Round(time.Millisecond).String())
	})
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON always sets Content-Length so that minimal HTTP clients (such as
// the macOS app's Unix-socket client) never need chunked decoding.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"code":"internal_error","message":"failed to encode response"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func (s *Server) writeSessionError(w http.ResponseWriter, err error) {
	var se *sessions.Error
	if !errors.As(err, &se) {
		s.Logger.Error("unexpected session error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	status := http.StatusInternalServerError
	switch se.Kind {
	case sessions.KindValidation:
		status = http.StatusBadRequest
	case sessions.KindCapacity:
		status = http.StatusTooManyRequests
	case sessions.KindProfileInUse, sessions.KindNotRunning:
		status = http.StatusConflict
	case sessions.KindNotFound:
		status = http.StatusNotFound
	case sessions.KindLaunchFailed:
		status = http.StatusBadGateway
	case sessions.KindShuttingDown:
		status = http.StatusServiceUnavailable
	}
	writeError(w, status, string(se.Kind), se.Message)
}

// decodeJSON strictly decodes an optional JSON object body into dst.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		return errors.New("request body is too large or unreadable")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid JSON body: " + strings.TrimPrefix(err.Error(), "json: "))
	}
	if dec.More() {
		return errors.New("invalid JSON body: unexpected data after the JSON object")
	}
	return nil
}

type createSessionRequest struct {
	TTLSeconds *int    `json:"ttlSeconds"`
	Headless   *bool   `json:"headless"`
	Profile    *string `json:"profile"`
}

type sessionResponse struct {
	ID               string     `json:"id"`
	Status           string     `json:"status"`
	Headless         bool       `json:"headless"`
	Profile          string     `json:"profile,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	ExpiresAt        time.Time  `json:"expiresAt"`
	CDPURL           string     `json:"cdpUrl,omitempty"`
	ConnectedClients int        `json:"connectedClients"`
	EndedAt          *time.Time `json:"endedAt,omitempty"`
	EndReason        string     `json:"endReason,omitempty"`
}

func (s *Server) sessionResponse(r *http.Request, sess sessions.Session) sessionResponse {
	resp := sessionResponse{
		ID:               sess.ID,
		Status:           string(sess.Status),
		Headless:         sess.Headless,
		Profile:          sess.Profile,
		CreatedAt:        sess.CreatedAt,
		ExpiresAt:        sess.ExpiresAt,
		ConnectedClients: sess.Clients,
		EndReason:        string(sess.EndReason),
	}
	if sess.Status == sessions.StatusTerminated {
		ended := sess.EndedAt
		resp.EndedAt = &ended
	} else {
		resp.CDPURL = s.cdpBaseURL(r) + "/v1/sessions/" + sess.ID + "/cdp"
	}
	return resp
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body createSessionRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, string(sessions.KindValidation), err.Error())
		return
	}
	req := sessions.CreateRequest{TTLSeconds: body.TTLSeconds, Headless: body.Headless}
	if body.Profile != nil {
		if *body.Profile == "" {
			writeError(w, http.StatusBadRequest, string(sessions.KindValidation),
				"profile must not be empty; omit it for an ephemeral profile")
			return
		}
		req.Profile = *body.Profile
	}
	sess, err := s.Sessions.Create(r.Context(), req)
	if err != nil {
		s.writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.sessionResponse(r, sess))
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	list := s.Sessions.List()
	out := make([]sessionResponse, 0, len(list))
	for _, sess := range list {
		out = append(out, s.sessionResponse(r, sess))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.Sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, string(sessions.KindNotFound), "session "+id+" not found")
		return
	}
	writeJSON(w, http.StatusOK, s.sessionResponse(r, sess))
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Sessions.Terminate(r.Context(), id); err != nil {
		s.writeSessionError(w, err)
		return
	}
	sess, _ := s.Sessions.Get(id)
	writeJSON(w, http.StatusOK, s.sessionResponse(r, sess))
}

type healthResponse struct {
	Status        string              `json:"status"`
	Version       string              `json:"version"`
	UptimeSeconds int64               `json:"uptimeSeconds"`
	Sessions      capacityResponse    `json:"sessions"`
	Browser       browserResponse     `json:"browser"`
	Tunnel        tunnel.TunnelStatus `json:"tunnel"`
}

type capacityResponse struct {
	Active    int `json:"active"`
	Max       int `json:"max"`
	Available int `json:"available"`
}

type browserResponse struct {
	Runtime    string `json:"runtime"`
	Available  bool   `json:"available"`
	Executable string `json:"executable,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) capacity() capacityResponse {
	active, limit := s.Sessions.ActiveCount(), s.Sessions.Limits().MaxConcurrent
	return capacityResponse{Active: active, Max: limit, Available: max(limit-active, 0)}
}

// browserStatus reports whether the configured browser can be launched. The
// executable path is only revealed on the local management channel.
func (s *Server) browserStatus(includePath bool) browserResponse {
	resp := browserResponse{Runtime: s.Backend.Config().Effective.Browser.Runtime, Available: true}
	if resolver, ok := s.Sessions.Runtime().(runtime.ExecutableResolver); ok {
		path, err := resolver.Executable()
		if err != nil {
			resp.Available, resp.Error = false, err.Error()
		} else if includePath {
			resp.Executable = path
		}
	}
	return resp
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status:        "ok",
		Version:       s.Version,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Sessions:      s.capacity(),
		Browser:       s.browserStatus(viaManagement(r)),
		Tunnel:        s.Tunnel.Status(),
	}
	if !resp.Browser.Available {
		resp.Status = "degraded"
	}
	writeJSON(w, http.StatusOK, resp)
}

// localBaseURL is the service's own loopback-reachable HTTP base URL.
func (s *Server) localBaseURL() string {
	host, port, err := net.SplitHostPort(s.ListenAddr)
	if err != nil {
		return "http://" + s.ListenAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// publicBaseURL is the externally reachable HTTP base URL, or "".
func (s *Server) publicBaseURL() string {
	if pinned := s.Backend.Config().Effective.Server.PublicURL; pinned != "" {
		return pinned
	}
	return s.Tunnel.PublicURL()
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func toWebSocketScheme(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	return strings.TrimRight(u.String(), "/")
}

// cdpBaseURL picks the WebSocket base URL to hand back to the caller:
//
//  1. server.publicUrl, when pinned by the operator;
//  2. for callers addressing the service by a loopback name, the address
//     they used (a local client gets a local URL);
//  3. the tunnel's public URL while a tunnel is up;
//  4. otherwise the address the caller used.
//
// The browser's own CDP endpoint is never exposed.
func (s *Server) cdpBaseURL(r *http.Request) string {
	if pinned := s.Backend.Config().Effective.Server.PublicURL; pinned != "" {
		return toWebSocketScheme(pinned)
	}
	if viaManagement(r) {
		if public := s.Tunnel.PublicURL(); public != "" {
			return toWebSocketScheme(public)
		}
		return toWebSocketScheme(s.localBaseURL())
	}
	if !isLoopbackHost(r.Host) {
		if public := s.Tunnel.PublicURL(); public != "" {
			return toWebSocketScheme(public)
		}
	}
	scheme := "ws"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "wss"
	}
	return scheme + "://" + r.Host
}
