package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/SkillfulAgents/tabtender/internal/auth"
	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/logging"
	"github.com/SkillfulAgents/tabtender/internal/runtime/runtimetest"
	"github.com/SkillfulAgents/tabtender/internal/secrets"
	"github.com/SkillfulAgents/tabtender/internal/sessions"
	"github.com/SkillfulAgents/tabtender/internal/tunnel"
)

// fakeBackend is an in-memory api.Backend.
type fakeBackend struct {
	mu       sync.Mutex
	store    *config.Store
	running  config.Config
	token    string
	restarts int
}

func (b *fakeBackend) Config() config.Snapshot      { return b.store.Snapshot() }
func (b *fakeBackend) RunningConfig() config.Config { return b.running }
func (b *fakeBackend) UpdateConfig(edit func(*config.Config) error) (config.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	file := b.store.Snapshot().File
	if err := edit(&file); err != nil {
		return config.Snapshot{}, err
	}
	return b.store.Save(file)
}
func (b *fakeBackend) TunnelTokenSource() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token == "" {
		return ""
	}
	return "memory"
}
func (b *fakeBackend) SetTunnelToken(token string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = token
	return nil
}
func (b *fakeBackend) RestartTunnel() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.restarts++
	return nil
}

// staticTunnel is a provider that is immediately "up" at a fixed URL.
type staticTunnel struct{ url string }

func (p staticTunnel) Start(context.Context, int) (string, error) { return p.url, nil }
func (p staticTunnel) Stop(context.Context) error                 { return nil }
func (p staticTunnel) Status() tunnel.TunnelStatus {
	return tunnel.TunnelStatus{State: tunnel.StateRunning, URL: p.url}
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// withoutToken strips the session token from a cdpUrl.
func withoutToken(cdpURL string) string {
	base, _, _ := strings.Cut(cdpURL, "?")
	return base
}

// tokenOf extracts the session token from a cdpUrl.
func tokenOf(t *testing.T, cdpURL string) string {
	t.Helper()
	_, token, ok := strings.Cut(cdpURL, "?token=")
	if !ok || token == "" {
		t.Fatalf("cdpUrl %q carries no token", cdpURL)
	}
	return token
}

type harness struct {
	t        *testing.T
	logText  *lockedBuffer
	server   *Server
	public   *httptest.Server
	mgmt     *httptest.Server
	rt       *runtimetest.Runtime
	keys     *auth.KeyManager
	backend  *fakeBackend
	tunnels  *tunnel.Manager
	sessions *sessions.Manager
}

func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	dir := t.TempDir()
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	store, err := config.Load(filepath.Join(dir, "config.yaml"), lookup)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Snapshot().Effective
	rt := runtimetest.New()
	manager := sessions.NewManager(sessions.Options{
		ProfilesDir: filepath.Join(dir, "profiles"),
		Journal:     sessions.NewJournal(filepath.Join(dir, "state", "sessions.json")),
		Runtime:     rt,
		Limits: sessions.Limits{
			DefaultTTL:      time.Duration(cfg.Sessions.DefaultTTL) * time.Second,
			MaxTTL:          time.Duration(cfg.Sessions.MaxTTL) * time.Second,
			MaxConcurrent:   cfg.Sessions.MaxConcurrent,
			HeadedSupported: true,
		},
		MonitorInterval: 10 * time.Millisecond,
	})
	keys, err := auth.NewKeyManager(secrets.NewMemory(), "")
	if err != nil {
		t.Fatal(err)
	}
	registry := tunnel.NewRegistry()
	registry.Register("static", func(cfg tunnel.TunnelConfig) (tunnel.TunnelProvider, error) {
		return staticTunnel{url: "https://" + cfg.Hostname}, nil
	})
	logText := &lockedBuffer{}
	logger, logs, _, err := logging.Setup("", slog.LevelDebug, logText)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:        t,
		logText:  logText,
		rt:       rt,
		keys:     keys,
		backend:  &fakeBackend{store: store, running: cfg},
		tunnels:  tunnel.NewManager(registry, 3000, nil),
		sessions: manager,
	}
	h.server = NewServer(Deps{
		Sessions:   manager,
		Keys:       keys,
		Tunnel:     h.tunnels,
		Backend:    h.backend,
		Logs:       logs,
		Logger:     logger,
		Paths:      config.Paths{ConfigFile: filepath.Join(dir, "config.yaml"), DataDir: dir},
		Version:    "test",
		ListenAddr: "127.0.0.1:3000",
	})
	h.public = httptest.NewServer(h.server.PublicHandler())
	h.mgmt = httptest.NewServer(h.server.ManagementHandler())
	t.Cleanup(func() {
		manager.Shutdown(context.Background())
		h.tunnels.Stop()
		h.public.Close()
		h.mgmt.Close()
	})
	return h
}

type response struct {
	status int
	header http.Header
	raw    []byte
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.raw, v); err != nil {
		t.Fatalf("decode %q: %v", r.raw, err)
	}
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	var body errorBody
	r.decode(t, &body)
	return body.Error.Code
}

func (h *harness) do(base, method, path, token, body string, headers ...string) response {
	h.t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	raw, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, raw: raw}
}

func (h *harness) api(method, path, body string, headers ...string) response {
	return h.do(h.public.URL, method, path, h.keys.Key(), body, headers...)
}

func (h *harness) manage(method, path, body string) response {
	return h.do(h.mgmt.URL, method, path, "", body)
}

func (h *harness) createSession(body string) sessionResponse {
	h.t.Helper()
	resp := h.api(http.MethodPost, "/v1/sessions", body)
	if resp.status != http.StatusCreated {
		h.t.Fatalf("create session: %d %s", resp.status, resp.raw)
	}
	var s sessionResponse
	resp.decode(h.t, &s)
	return s
}

func (h *harness) dial(cdpURL, token string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, cdpURL, &websocket.DialOptions{HTTPHeader: header})
	if conn != nil {
		conn.SetReadLimit(-1)
	}
	return conn, resp, err
}

func (h *harness) mustDial(s sessionResponse) *websocket.Conn {
	h.t.Helper()
	conn, _, err := h.dial(s.CDPURL, h.keys.Key())
	if err != nil {
		h.t.Fatalf("dial %s: %v", s.CDPURL, err)
	}
	h.t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func TestEveryPublicEndpointRequiresAuthentication(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	endpoints := []struct{ method, path string }{
		{http.MethodPost, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions/" + s.ID},
		{http.MethodDelete, "/v1/sessions/" + s.ID},
		{http.MethodGet, "/v1/sessions/" + s.ID + "/cdp"},
		{http.MethodGet, "/v1/health"},
		{http.MethodGet, "/mgmt/v1/apikey"},
		{http.MethodGet, "/anything-else"},
	}
	for _, e := range endpoints {
		for _, token := range []string{"", "wrong-key"} {
			resp := h.do(h.public.URL, e.method, e.path, token, "")
			if resp.status != http.StatusUnauthorized || resp.errorCode(t) != "unauthorized" {
				t.Errorf("%s %s with token %q: status %d body %s", e.method, e.path, token, resp.status, resp.raw)
			}
		}
	}
	if got, _ := h.sessions.Get(s.ID); got.Status != sessions.StatusRunning {
		t.Fatal("an unauthenticated DELETE affected the session")
	}
	// WebSocket handshakes are covered by the same check.
	_, resp, err := h.dial(withoutToken(s.CDPURL), "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated CDP dial: err=%v resp=%v", err, resp)
	}
	if h.rt.Instances()[0].Connections() != 0 {
		t.Fatal("an unauthenticated client reached the browser")
	}
}

func TestManagementRoutesAreNotOnThePublicListener(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"/mgmt/v1/apikey", "/mgmt/v1/config", "/mgmt/v1/status", "/mgmt/v1/logs"} {
		if resp := h.api(http.MethodGet, path, ""); resp.status != http.StatusNotFound {
			t.Errorf("GET %s on the public listener: status %d", path, resp.status)
		}
	}
}

func TestSessionLifecycleOverREST(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createSession(`{"ttlSeconds": 1800, "headless": true, "profile": "research"}`)
	if created.Status != "running" || !created.Headless || created.Profile != "research" {
		t.Fatalf("created = %+v", created)
	}
	if ttl := created.ExpiresAt.Sub(created.CreatedAt); ttl < 1799*time.Second || ttl > 1802*time.Second {
		t.Fatalf("ttl = %s", ttl)
	}
	wantURL := "ws" + strings.TrimPrefix(h.public.URL, "http") + "/v1/sessions/" + created.ID + "/cdp?token=" + tokenOf(t, created.CDPURL)
	if created.CDPURL != wantURL {
		t.Fatalf("cdpUrl = %q, want %q", created.CDPURL, wantURL)
	}

	var got sessionResponse
	h.api(http.MethodGet, "/v1/sessions/"+created.ID, "").decode(t, &got)
	if got.ID != created.ID || got.CDPURL != wantURL {
		t.Fatalf("get = %+v", got)
	}

	h.createSession("")
	var list struct{ Sessions []sessionResponse }
	h.api(http.MethodGet, "/v1/sessions", "").decode(t, &list)
	if len(list.Sessions) != 2 || list.Sessions[0].ID != created.ID {
		t.Fatalf("list = %+v", list.Sessions)
	}

	deleted := h.api(http.MethodDelete, "/v1/sessions/"+created.ID, "")
	if deleted.status != http.StatusOK {
		t.Fatalf("delete: %d %s", deleted.status, deleted.raw)
	}
	var ended sessionResponse
	deleted.decode(t, &ended)
	if ended.Status != "terminated" || ended.EndReason != "deleted" || ended.EndedAt == nil || ended.CDPURL != "" {
		t.Fatalf("deleted = %+v", ended)
	}
	if resp := h.api(http.MethodDelete, "/v1/sessions/"+created.ID, ""); resp.status != http.StatusNotFound {
		t.Fatalf("second delete: %d", resp.status)
	}
	if resp := h.api(http.MethodGet, "/v1/sessions/sess_nope", ""); resp.status != http.StatusNotFound || resp.errorCode(t) != "session_not_found" {
		t.Fatalf("unknown session: %d %s", resp.status, resp.raw)
	}
	h.api(http.MethodGet, "/v1/sessions", "").decode(t, &list)
	if len(list.Sessions) != 1 {
		t.Fatalf("list after delete has %d sessions", len(list.Sessions))
	}
}

func TestCreateSessionValidation(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct{ name, body, wantInMessage string }{
		{"ttl too large", `{"ttlSeconds": 14401}`, "exceeds the maximum"},
		{"ttl zero", `{"ttlSeconds": 0}`, "at least 1"},
		{"ttl wrong type", `{"ttlSeconds": "soon"}`, "invalid JSON"},
		{"ttl fractional", `{"ttlSeconds": 1.5}`, "invalid JSON"},
		{"unknown field", `{"ttl": 5}`, "unknown field"},
		{"malformed", `{"ttlSeconds": `, "invalid JSON"},
		{"trailing data", `{} {}`, "unexpected data"},
		{"bad profile", `{"profile": "../../etc"}`, "profile must be"},
		{"empty profile", `{"profile": ""}`, "must not be empty"},
		{"headless wrong type", `{"headless": "yes"}`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.api(http.MethodPost, "/v1/sessions", tc.body)
			var body errorBody
			resp.decode(t, &body)
			if resp.status != http.StatusBadRequest || body.Error.Code != "validation_error" ||
				!strings.Contains(body.Error.Message, tc.wantInMessage) {
				t.Fatalf("status %d, body %s", resp.status, resp.raw)
			}
		})
	}
	if len(h.rt.Instances()) != 0 {
		t.Fatal("invalid requests launched browsers")
	}
}

func TestCapacityAndConflictErrors(t *testing.T) {
	h := newHarness(t, map[string]string{config.EnvMaxSessions: "2"})
	h.createSession(`{"profile": "p"}`)
	if resp := h.api(http.MethodPost, "/v1/sessions", `{"profile": "p"}`); resp.status != http.StatusConflict || resp.errorCode(t) != "profile_in_use" {
		t.Fatalf("profile conflict: %d %s", resp.status, resp.raw)
	}
	h.createSession("")
	if resp := h.api(http.MethodPost, "/v1/sessions", ""); resp.status != http.StatusTooManyRequests || resp.errorCode(t) != "capacity_exceeded" {
		t.Fatalf("capacity: %d %s", resp.status, resp.raw)
	}
	var health healthResponse
	h.api(http.MethodGet, "/v1/health", "").decode(t, &health)
	if health.Sessions != (capacityResponse{Active: 2, Max: 2, Available: 0}) {
		t.Fatalf("health capacity = %+v", health.Sessions)
	}

	h.rt.LaunchErr = errors.New("chrome exploded")
	h.sessions.SetLimits(sessions.Limits{DefaultTTL: time.Hour, MaxTTL: time.Hour, MaxConcurrent: 3, HeadedSupported: true})
	if resp := h.api(http.MethodPost, "/v1/sessions", ""); resp.status != http.StatusBadGateway || resp.errorCode(t) != "browser_launch_failed" {
		t.Fatalf("launch failure: %d %s", resp.status, resp.raw)
	}
}

func TestHealth(t *testing.T) {
	h := newHarness(t, nil)
	h.createSession("")
	var health healthResponse
	resp := h.api(http.MethodGet, "/v1/health", "")
	resp.decode(t, &health)
	if resp.status != http.StatusOK || health.Status != "ok" || health.Version != "test" {
		t.Fatalf("health = %+v", health)
	}
	if health.Sessions != (capacityResponse{Active: 1, Max: 8, Available: 7}) {
		t.Fatalf("capacity = %+v", health.Sessions)
	}
	if health.Tunnel.Provider != "none" || health.Tunnel.State != tunnel.StateDisabled {
		t.Fatalf("tunnel = %+v", health.Tunnel)
	}
}

func TestCDPProxyRelaysMessages(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	conn := h.mustDial(s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Larger than any default read limit, as screenshots and traces are.
	large := []byte(`{"id":2,"result":{"data":"` + strings.Repeat("A", 8<<20) + `"}}`)
	messages := []struct {
		typ  websocket.MessageType
		data []byte
	}{
		{websocket.MessageText, []byte(`{"id":1,"method":"Browser.getVersion"}`)},
		{websocket.MessageText, []byte(`{"id":3,"params":{"text":"✓ unicode"}}`)},
		// CDP is JSON either way; some clients send it in binary frames.
		{websocket.MessageBinary, []byte(`{"id":4,"method":"Target.getTargets"}`)},
		{websocket.MessageText, large},
	}
	for i, m := range messages {
		if err := conn.Write(ctx, m.typ, m.data); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if typ != websocket.MessageText || !bytes.Equal(data, m.data) {
			t.Fatalf("message %d was altered in transit (type %v, %d bytes)", i, typ, len(data))
		}
	}
}

func TestCDPProxyRejectsMessagesTheBrowserCannotTake(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	conn := h.mustDial(s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0x00, 0xff}); err != nil {
		t.Fatal(err)
	}
	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInvalidFramePayloadData {
		t.Fatalf("close = %v, want invalid payload", err)
	}
	if got, _ := h.sessions.Get(s.ID); got.Status != sessions.StatusRunning {
		t.Fatal("a misbehaving client must not end the session")
	}
}

func TestEveryCDPClientGetsItsOwnBrowserConnection(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	h.mustDial(s)
	h.mustDial(s)
	if got := h.rt.Instances()[0].Connections(); got != 2 {
		t.Fatalf("the browser saw %d connections, want 2", got)
	}
}

func TestCDPClientsCanDisconnectAndReconnect(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	ctx := context.Background()

	first := h.mustDial(s)
	second := h.mustDial(s) // several clients may attach at once
	eventually(t, "both clients to be counted", func() bool {
		got, _ := h.sessions.Get(s.ID)
		return got.Clients == 2
	})
	if err := first.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatal(err)
	}
	_ = second.CloseNow() // abrupt disconnect
	eventually(t, "clients to be released", func() bool {
		got, _ := h.sessions.Get(s.ID)
		return got.Clients == 0
	})
	if got, _ := h.sessions.Get(s.ID); got.Status != sessions.StatusRunning {
		t.Fatalf("session ended when its clients disconnected: %+v", got)
	}
	if !h.rt.Instances()[0].IsRunning() {
		t.Fatal("browser stopped when its clients disconnected")
	}

	again := h.mustDial(s)
	if err := again.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := again.Read(ctx); err != nil || string(data) != "ping" {
		t.Fatalf("reconnected client could not talk to the browser: %q %v", data, err)
	}
}

func TestTerminatingASessionClosesItsClients(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	conn := h.mustDial(s)
	if resp := h.api(http.MethodDelete, "/v1/sessions/"+s.ID, ""); resp.status != http.StatusOK {
		t.Fatalf("delete: %d", resp.status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (err %v), want going away", got, err)
	}
	if _, resp, err := h.dial(s.CDPURL, h.keys.Key()); err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("dial after termination: err=%v resp=%v", err, resp)
	}
}

// dialStatus attempts a CDP handshake and returns the HTTP status it got
// (101 when the WebSocket was established).
func (h *harness) dialStatus(cdpURL, apiKey string) int {
	h.t.Helper()
	conn, resp, err := h.dial(cdpURL, apiKey)
	if err == nil {
		_ = conn.CloseNow()
		return http.StatusSwitchingProtocols
	}
	if resp == nil {
		h.t.Fatalf("dial %s: %v", cdpURL, err)
	}
	return resp.StatusCode
}

func TestSessionTokenConnectsWithoutAHeader(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	token := tokenOf(t, s.CDPURL)
	// 256 bits, base64url without padding.
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(token) {
		t.Fatalf("token %q is not 256 bits of base64url", token)
	}

	conn, _, err := h.dial(s.CDPURL, "")
	if err != nil {
		t.Fatalf("connecting with only the cdpUrl failed: %v", err)
	}
	defer conn.CloseNow() //nolint:errcheck // test
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.Read(ctx); err != nil || string(data) != "hello" {
		t.Fatalf("echo through a token-authenticated connection: %q %v", data, err)
	}
	// Header authentication keeps working, with or without the token.
	if got := h.dialStatus(withoutToken(s.CDPURL), h.keys.Key()); got != http.StatusSwitchingProtocols {
		t.Fatalf("header-authenticated dial: %d", got)
	}
}

func TestSessionTokenIsScopedToItsOwnSession(t *testing.T) {
	h := newHarness(t, nil)
	a, b := h.createSession(""), h.createSession("")
	tokenA, tokenB := tokenOf(t, a.CDPURL), tokenOf(t, b.CDPURL)
	if tokenA == tokenB {
		t.Fatal("two sessions share a token")
	}
	cases := map[string]string{
		"no credentials":           withoutToken(a.CDPURL),
		"empty token":              withoutToken(a.CDPURL) + "?token=",
		"another session's token":  withoutToken(a.CDPURL) + "?token=" + tokenB,
		"truncated token":          withoutToken(a.CDPURL) + "?token=" + tokenA[:len(tokenA)-1],
		"API key in the query":     withoutToken(a.CDPURL) + "?token=" + h.keys.Key(),
		"token on unknown session": strings.Replace(a.CDPURL, a.ID, "sess_unknown", 1),
	}
	for name, cdpURL := range cases {
		_, resp, err := h.dial(cdpURL, "")
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: err=%v resp=%v, want 401", name, err, resp)
			continue
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: missing WWW-Authenticate header", name)
		}
	}
	if h.rt.Instances()[0].Connections() != 0 {
		t.Fatal("an unauthorized client reached session A's browser")
	}
}

func TestSessionTokenIsRejectedEverywhereElse(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	query := "?token=" + tokenOf(t, s.CDPURL)
	endpoints := []struct{ method, path string }{
		{http.MethodPost, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions/" + s.ID},
		{http.MethodDelete, "/v1/sessions/" + s.ID},
		{http.MethodGet, "/v1/health"},
		{http.MethodGet, "/mgmt/v1/apikey"},
		// Only GET on the CDP endpoint is token-authorized.
		{http.MethodDelete, "/v1/sessions/" + s.ID + "/cdp"},
		{http.MethodPost, "/v1/sessions/" + s.ID + "/cdp"},
	}
	for _, e := range endpoints {
		resp := h.do(h.public.URL, e.method, e.path+query, "", "")
		if resp.status != http.StatusUnauthorized || resp.errorCode(t) != "unauthorized" {
			t.Errorf("%s %s with the session token: %d %s", e.method, e.path, resp.status, resp.raw)
		}
		// The token in a bearer header is not an API key either.
		resp = h.do(h.public.URL, e.method, e.path, tokenOf(t, s.CDPURL), "")
		if resp.status != http.StatusUnauthorized {
			t.Errorf("%s %s with the session token as bearer: %d", e.method, e.path, resp.status)
		}
	}
	if got, _ := h.sessions.Get(s.ID); got.Status != sessions.StatusRunning {
		t.Fatal("the session token was able to affect the session")
	}
	// The API key is never accepted in the query string.
	if resp := h.do(h.public.URL, http.MethodGet, "/v1/health?token="+h.keys.Key(), "", ""); resp.status != http.StatusUnauthorized {
		t.Fatalf("API key in the query string was accepted: %d", resp.status)
	}
}

func TestSessionTokenIsStableAcrossResponses(t *testing.T) {
	h := newHarness(t, nil)
	created := h.createSession("")
	h.createSession("")

	var got sessionResponse
	h.api(http.MethodGet, "/v1/sessions/"+created.ID, "").decode(t, &got)
	var list struct{ Sessions []sessionResponse }
	h.api(http.MethodGet, "/v1/sessions", "").decode(t, &list)
	if len(list.Sessions) != 2 || list.Sessions[0].ID != created.ID {
		t.Fatalf("list = %+v", list.Sessions)
	}
	for name, cdpURL := range map[string]string{"get": got.CDPURL, "list": list.Sessions[0].CDPURL} {
		if cdpURL != created.CDPURL {
			t.Errorf("%s returned %q, create returned %q", name, cdpURL, created.CDPURL)
		}
	}
	if tokenOf(t, list.Sessions[1].CDPURL) == tokenOf(t, created.CDPURL) {
		t.Fatal("sessions share a token")
	}
	// Reading the session again, much later, still yields the working URL.
	if h.dialStatus(got.CDPURL, "") != http.StatusSwitchingProtocols {
		t.Fatal("the URL from a later read does not connect")
	}
}

func TestSessionTokenDiesWithTheSession(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	other := h.createSession("")
	if h.dialStatus(s.CDPURL, "") != http.StatusSwitchingProtocols {
		t.Fatal("token did not work while the session was running")
	}
	deleted := h.api(http.MethodDelete, "/v1/sessions/"+s.ID, "")
	if strings.Contains(string(deleted.raw), "token") || strings.Contains(string(deleted.raw), "cdpUrl") {
		t.Fatalf("a terminated session still exposes its URL: %s", deleted.raw)
	}
	if strings.Contains(string(h.api(http.MethodGet, "/v1/sessions/"+s.ID, "").raw), tokenOf(t, s.CDPURL)) {
		t.Fatal("the token is still returned for a terminated session")
	}

	// The holder of the old URL is told the session is over, exactly as an
	// API-key holder is; nobody can connect.
	if got := h.dialStatus(s.CDPURL, ""); got != http.StatusConflict {
		t.Fatalf("old URL after delete: %d, want 409", got)
	}
	if got := h.dialStatus(withoutToken(s.CDPURL), h.keys.Key()); got != http.StatusConflict {
		t.Fatalf("API key on the ended session: %d, want 409", got)
	}
	// A stranger learns nothing about the ended session.
	if got := h.dialStatus(withoutToken(s.CDPURL)+"?token="+tokenOf(t, other.CDPURL), ""); got != http.StatusUnauthorized {
		t.Fatalf("wrong token on the ended session: %d, want 401", got)
	}
	// And the dead token is no good for a live session.
	if got := h.dialStatus(withoutToken(other.CDPURL)+"?token="+tokenOf(t, s.CDPURL), ""); got != http.StatusUnauthorized {
		t.Fatalf("dead token on a live session: %d, want 401", got)
	}
}

func TestAPIKeyRotationLeavesSessionTokensWorking(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	conn := h.mustDial(s) // an established connection is not disturbed either
	oldKey := h.keys.Key()
	if resp := h.manage(http.MethodPost, "/mgmt/v1/apikey/rotate", ""); resp.status != http.StatusOK {
		t.Fatalf("rotate: %d", resp.status)
	}
	if h.dialStatus(s.CDPURL, "") != http.StatusSwitchingProtocols {
		t.Fatal("the session token stopped working after API key rotation")
	}
	var got sessionResponse
	h.api(http.MethodGet, "/v1/sessions/"+s.ID, "").decode(t, &got)
	if got.CDPURL != s.CDPURL {
		t.Fatal("API key rotation changed the session's cdpUrl")
	}
	if got := h.dialStatus(withoutToken(s.CDPURL), oldKey); got != http.StatusUnauthorized {
		t.Fatalf("the old API key still opens CDP connections: %d", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte("still here")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.Read(ctx); err != nil || string(data) != "still here" {
		t.Fatalf("existing connection after rotation: %q %v", data, err)
	}
}

func TestSessionTokensAreNeverLogged(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession(`{"profile": "logged"}`)
	token := tokenOf(t, s.CDPURL)

	// Exercise every path that sees the token: success, failures, reads.
	h.mustDial(s)
	h.dialStatus(withoutToken(s.CDPURL)+"?token="+token+"x", "")
	h.do(h.public.URL, http.MethodGet, "/v1/health?token="+token, "", "")
	h.api(http.MethodGet, "/v1/sessions", "")
	h.manage(http.MethodGet, "/v1/sessions/"+s.ID, "")
	h.api(http.MethodDelete, "/v1/sessions/"+s.ID, "")
	h.dialStatus(s.CDPURL, "")

	if text := h.logText.String(); strings.Contains(text, token) {
		t.Fatalf("the session token appears in the log:\n%s", text)
	} else if !strings.Contains(text, "/v1/sessions/"+s.ID+"/cdp") {
		t.Fatal("expected the CDP requests to be logged (without their query)")
	}
	// The management API's log buffer is clean too.
	if logs := h.manage(http.MethodGet, "/mgmt/v1/logs?level=debug&limit=1000", ""); strings.Contains(string(logs.raw), token) {
		t.Fatalf("the session token appears in the management log buffer: %s", logs.raw)
	}
	if strings.Contains(h.logText.String(), h.keys.Key()) {
		t.Fatal("the API key appears in the log")
	}
}

func TestLosingTheBrowserConnectionClosesTheClient(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	conn := h.mustDial(s)
	inst := h.rt.Instances()[0]
	eventually(t, "the browser to see the client", func() bool { return inst.Connections() == 1 })
	inst.DropClients()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusGoingAway || ce.Reason != "browser disconnected" {
		t.Fatalf("close = %v, want going away: browser disconnected", err)
	}
	if got, _ := h.sessions.Get(s.ID); got.Status != sessions.StatusRunning {
		t.Fatal("a closed CDP connection must not end the session")
	}
}

func TestUnreachableBrowserIsReported(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	h.rt.Instances()[0].FailConnect(errors.New("no pipe"))
	if got := h.dialStatus(s.CDPURL, ""); got != http.StatusBadGateway {
		t.Fatalf("dial = %d, want 502", got)
	}
}

func TestCDPEndpointErrors(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	resp := h.api(http.MethodGet, "/v1/sessions/"+s.ID+"/cdp", "")
	if resp.status != http.StatusUpgradeRequired || resp.errorCode(t) != "upgrade_required" {
		t.Fatalf("plain GET: %d %s", resp.status, resp.raw)
	}
	_, dialResp, err := h.dial(strings.Replace(s.CDPURL, s.ID, "sess_unknown", 1), h.keys.Key())
	if err == nil || dialResp == nil || dialResp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: err=%v resp=%v", err, dialResp)
	}
	// The raw browser endpoint never leaks through the API.
	if strings.Contains(string(h.api(http.MethodGet, "/v1/sessions/"+s.ID, "").raw), "devtools") {
		t.Fatal("session JSON exposes the browser's own CDP endpoint")
	}
}

func TestCDPURLSelection(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	get := func(headers ...string) string {
		var got sessionResponse
		h.api(http.MethodGet, "/v1/sessions/"+s.ID, "", headers...).decode(t, &got)
		return strings.TrimSuffix(withoutToken(got.CDPURL), "/v1/sessions/"+s.ID+"/cdp")
	}
	viaMgmt := func() string {
		var got sessionResponse
		h.manage(http.MethodGet, "/v1/sessions/"+s.ID, "").decode(t, &got)
		if tokenOf(t, got.CDPURL) != tokenOf(t, s.CDPURL) {
			t.Errorf("the management channel reports a different token")
		}
		return strings.TrimSuffix(withoutToken(got.CDPURL), "/v1/sessions/"+s.ID+"/cdp")
	}

	// No tunnel: echo the address the caller used.
	if got := get("Host", "localhost:3000"); got != "ws://localhost:3000" {
		t.Errorf("local caller: %q", got)
	}
	if got := get("Host", "browser.internal:8443", "X-Forwarded-Proto", "https"); got != "wss://browser.internal:8443" {
		t.Errorf("behind a TLS proxy: %q", got)
	}
	if got := viaMgmt(); got != "ws://127.0.0.1:3000" {
		t.Errorf("management channel without tunnel: %q", got)
	}

	// Tunnel up: remote callers get the public wss:// URL.
	if err := h.tunnels.Apply("static", tunnel.TunnelConfig{Hostname: "abc.trycloudflare.com"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "tunnel to be running", func() bool { return h.tunnels.PublicURL() != "" })
	if got := get("Host", "abc.trycloudflare.com"); got != "wss://abc.trycloudflare.com" {
		t.Errorf("tunnel caller: %q", got)
	}
	if got := get("Host", "127.0.0.1:3000"); got != "ws://127.0.0.1:3000" {
		t.Errorf("local caller while tunnel is up: %q", got)
	}
	if got := viaMgmt(); got != "wss://abc.trycloudflare.com" {
		t.Errorf("management channel with tunnel: %q", got)
	}

	// An operator-pinned public URL always wins.
	pin := func(file *config.Config) error {
		file.Server.PublicURL = "https://browsers.example.com/"
		return nil
	}
	if _, err := h.backend.UpdateConfig(pin); err != nil {
		t.Fatal(err)
	}
	if got := get("Host", "127.0.0.1:3000"); got != "wss://browsers.example.com" {
		t.Errorf("pinned public URL: %q", got)
	}
}

func TestRejectedConfigUpdateLeavesNoTrace(t *testing.T) {
	h := newHarness(t, nil)
	put := func(body string) int {
		return h.manage(http.MethodPut, "/mgmt/v1/config", body).status
	}
	extraArgs := func() []string {
		snap := h.backend.Config()
		if !slices.Equal(snap.File.Browser.ExtraArgs, snap.Effective.Browser.ExtraArgs) {
			t.Fatalf("file %v and effective %v disagree", snap.File.Browser.ExtraArgs, snap.Effective.Browser.ExtraArgs)
		}
		return snap.File.Browser.ExtraArgs
	}
	if got := put(`{"browser":{"extraArgs":["--a","--b"]}}`); got != http.StatusOK {
		t.Fatalf("status %d", got)
	}
	// Valid JSON, invalid configuration: nothing of it may stick.
	if got := put(`{"browser":{"extraArgs":["--x"]},"sessions":{"maxConcurrent":0}}`); got != http.StatusBadRequest {
		t.Fatalf("status %d", got)
	}
	if got := extraArgs(); !slices.Equal(got, []string{"--a", "--b"}) {
		t.Fatalf("a rejected update changed the configuration: %v", got)
	}
	if got := put(`{"sessions":{"maxConcurrent":3}}`); got != http.StatusOK {
		t.Fatalf("status %d", got)
	}
	if got := extraArgs(); !slices.Equal(got, []string{"--a", "--b"}) {
		t.Fatalf("a rejected update leaked into a later one: %v", got)
	}
	// Editing a snapshot never reaches the stored configuration either.
	h.backend.Config().File.Browser.ExtraArgs[0] = "--tampered"
	if got := extraArgs(); got[0] != "--a" {
		t.Fatalf("a snapshot shares memory with the store: %v", got)
	}
}

func TestSessionTokenIgnoresEncodedPaths(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	token := tokenOf(t, s.CDPURL)
	for _, path := range []string{
		"/v1/sessions/" + s.ID + "%2Fcdp",
		"/v1/sessions%2F" + s.ID + "/cdp",
		"/v1%2Fsessions/" + s.ID + "/cdp",
		"/v1/sessions/" + s.ID + "/%63dp",
	} {
		if got := h.do(h.public.URL, http.MethodGet, path+"?token="+token, "", "").status; got != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", path, got)
		}
	}
	// A "?" encoded into the path would otherwise carry the token into the log.
	h.do(h.public.URL, http.MethodGet, "/v1/sessions/"+s.ID+"/cdp%3Ftoken="+token, "", "")
	if strings.Contains(h.logText.String(), token) {
		t.Fatalf("the session token appears in the log:\n%s", h.logText.String())
	}
}

func TestManagementConfigRoundTrip(t *testing.T) {
	h := newHarness(t, map[string]string{config.EnvMaxSessions: "4"})
	var view configResponse
	h.manage(http.MethodGet, "/mgmt/v1/config", "").decode(t, &view)
	if view.File.Sessions.MaxConcurrent != 8 || view.Effective.Sessions.MaxConcurrent != 4 {
		t.Fatalf("file/effective maxConcurrent = %d/%d", view.File.Sessions.MaxConcurrent, view.Effective.Sessions.MaxConcurrent)
	}
	if o := view.EnvOverrides["sessions.maxConcurrent"]; o.Variable != config.EnvMaxSessions || o.Value != "4" {
		t.Fatalf("envOverrides = %+v", view.EnvOverrides)
	}
	if len(view.RestartRequiredKeys) == 0 || len(view.PendingRestart) != 0 {
		t.Fatalf("restart info = %v / %v", view.RestartRequiredKeys, view.PendingRestart)
	}

	// A partial document only changes what it mentions.
	resp := h.manage(http.MethodPut, "/mgmt/v1/config",
		`{"sessions": {"defaultTTL": 600, "maxTTL": 14400, "maxConcurrent": 6}, "server": {"host": "127.0.0.1", "port": 4444}}`)
	if resp.status != http.StatusOK {
		t.Fatalf("PUT config: %d %s", resp.status, resp.raw)
	}
	resp.decode(t, &view)
	if view.File.Sessions.DefaultTTL != 600 || view.File.Sessions.MaxConcurrent != 6 {
		t.Fatalf("file layer not updated: %+v", view.File.Sessions)
	}
	if view.Effective.Sessions.MaxConcurrent != 4 {
		t.Fatal("the environment override must stay in effect")
	}
	if !view.File.App.StartOnLogin || view.File.Browser.Runtime != "chrome" {
		t.Fatalf("untouched sections were reset: %+v", view.File)
	}
	if len(view.PendingRestart) != 1 || view.PendingRestart[0] != "server.port" {
		t.Fatalf("pendingRestart = %v", view.PendingRestart)
	}

	invalid := h.manage(http.MethodPut, "/mgmt/v1/config", `{"sessions": {"defaultTTL": 0, "maxTTL": 10, "maxConcurrent": 1}}`)
	if invalid.status != http.StatusBadRequest || invalid.errorCode(t) != "validation_error" {
		t.Fatalf("invalid config: %d %s", invalid.status, invalid.raw)
	}
	unknown := h.manage(http.MethodPut, "/mgmt/v1/config", `{"sesions": {}}`)
	if unknown.status != http.StatusBadRequest {
		t.Fatalf("unknown key accepted: %d", unknown.status)
	}
}

func TestManagementAPIKey(t *testing.T) {
	h := newHarness(t, nil)
	var key apiKeyResponse
	h.manage(http.MethodGet, "/mgmt/v1/apikey", "").decode(t, &key)
	old := h.keys.Key()
	if key.APIKey != old || !key.Rotatable || key.Source != "memory" {
		t.Fatalf("apikey = %+v", key)
	}
	h.manage(http.MethodPost, "/mgmt/v1/apikey/rotate", "").decode(t, &key)
	if key.APIKey == old || key.APIKey != h.keys.Key() {
		t.Fatal("rotation did not return the new key")
	}
	if resp := h.do(h.public.URL, http.MethodGet, "/v1/health", old, ""); resp.status != http.StatusUnauthorized {
		t.Fatalf("old key still accepted: %d", resp.status)
	}
	if resp := h.do(h.public.URL, http.MethodGet, "/v1/health", key.APIKey, ""); resp.status != http.StatusOK {
		t.Fatalf("new key rejected: %d", resp.status)
	}
}

func TestManagementStatusProfilesLogsAndTunnel(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession(`{"profile": "alpha"}`)

	var status statusResponse
	h.manage(http.MethodGet, "/mgmt/v1/status", "").decode(t, &status)
	if status.Version != "test" || status.LocalURL != "http://127.0.0.1:3000" || status.PublicURL != "" ||
		status.Sessions.Active != 1 || status.PID == 0 || status.ConfigPath == "" {
		t.Fatalf("status = %+v", status)
	}

	var profiles struct{ Profiles []profileResponse }
	h.manage(http.MethodGet, "/mgmt/v1/profiles", "").decode(t, &profiles)
	if len(profiles.Profiles) != 1 || profiles.Profiles[0] != (profileResponse{Name: "alpha", InUse: true, SessionID: s.ID}) {
		t.Fatalf("profiles = %+v", profiles.Profiles)
	}
	if resp := h.manage(http.MethodDelete, "/mgmt/v1/profiles/alpha", ""); resp.status != http.StatusConflict {
		t.Fatalf("deleting an in-use profile: %d", resp.status)
	}
	// Sessions are manageable over the management channel without a key.
	if resp := h.manage(http.MethodDelete, "/v1/sessions/"+s.ID, ""); resp.status != http.StatusOK {
		t.Fatalf("terminate via management: %d", resp.status)
	}
	if resp := h.manage(http.MethodDelete, "/mgmt/v1/profiles/alpha", ""); resp.status != http.StatusNoContent {
		t.Fatalf("delete profile: %d %s", resp.status, resp.raw)
	}
	if resp := h.manage(http.MethodDelete, "/mgmt/v1/profiles/alpha", ""); resp.status != http.StatusNotFound || resp.errorCode(t) != "profile_not_found" {
		t.Fatalf("delete missing profile: %d %s", resp.status, resp.raw)
	}
	if resp := h.manage(http.MethodDelete, "/mgmt/v1/profiles/bad.name", ""); resp.status != http.StatusBadRequest {
		t.Fatalf("delete profile with an invalid name: %d", resp.status)
	}

	h.server.Logger.Warn("something odd", "detail", "x")
	var logs struct{ Entries []logging.Entry }
	h.manage(http.MethodGet, "/mgmt/v1/logs?level=warn&limit=5", "").decode(t, &logs)
	if len(logs.Entries) != 1 || logs.Entries[0].Message != "something odd" {
		t.Fatalf("logs = %+v", logs.Entries)
	}
	if resp := h.manage(http.MethodGet, "/mgmt/v1/logs?limit=zero", ""); resp.status != http.StatusBadRequest {
		t.Fatalf("bad limit: %d", resp.status)
	}

	if resp := h.manage(http.MethodPut, "/mgmt/v1/tunnel/token", `{"token": "secret-token"}`); resp.status != http.StatusOK ||
		strings.Contains(string(resp.raw), "secret-token") {
		t.Fatalf("set tunnel token: %d %s", resp.status, resp.raw)
	}
	h.manage(http.MethodGet, "/mgmt/v1/status", "").decode(t, &status)
	if status.TunnelToken != "memory" {
		t.Fatalf("tunnelTokenSource = %q", status.TunnelToken)
	}
	if resp := h.manage(http.MethodPost, "/mgmt/v1/tunnel/restart", ""); resp.status != http.StatusOK || h.backend.restarts != 1 {
		t.Fatalf("tunnel restart: %d (restarts=%d)", resp.status, h.backend.restarts)
	}
}

func TestResponsesCarryContentLength(t *testing.T) {
	// The macOS app's minimal Unix-socket HTTP client relies on this.
	h := newHarness(t, nil)
	for i := 0; i < 40; i++ {
		h.server.Logger.Info(strings.Repeat("long log line ", 20))
	}
	resp := h.manage(http.MethodGet, "/mgmt/v1/logs", "")
	if resp.header.Get("Content-Length") == "" || len(resp.raw) < 4096 {
		t.Fatalf("Content-Length=%q for a %d byte body", resp.header.Get("Content-Length"), len(resp.raw))
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
