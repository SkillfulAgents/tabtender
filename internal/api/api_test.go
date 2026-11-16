package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
func (b *fakeBackend) UpdateConfig(file config.Config) (config.Snapshot, error) {
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

type harness struct {
	t        *testing.T
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
	logger, logs, _, err := logging.Setup("", slog.LevelInfo, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:        t,
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
	_, resp, err := h.dial(s.CDPURL, "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated CDP dial: err=%v resp=%v", err, resp)
	}
	if len(h.rt.Instances()[0].Handshakes()) != 0 {
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
	wantURL := "ws" + strings.TrimPrefix(h.public.URL, "http") + "/v1/sessions/" + created.ID + "/cdp"
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

func TestCDPProxyRelaysTextAndBinaryFramesVerbatim(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	conn := h.mustDial(s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	large := make([]byte, 8<<20) // larger than any default read limit
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	messages := []struct {
		typ  websocket.MessageType
		data []byte
	}{
		{websocket.MessageText, []byte(`{"id":1,"method":"Browser.getVersion"}`)},
		{websocket.MessageBinary, []byte{0x00, 0xff, 0x10, 0x80}},
		{websocket.MessageText, []byte("not json at all — the proxy must not care ✓")},
		{websocket.MessageBinary, large},
		{websocket.MessageText, []byte("")},
	}
	for i, m := range messages {
		if err := conn.Write(ctx, m.typ, m.data); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if typ != m.typ || !bytes.Equal(data, m.data) {
			t.Fatalf("message %d was altered in transit (type %v, %d bytes)", i, typ, len(data))
		}
	}
}

func TestCDPProxyNeverForwardsCredentials(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	h.mustDial(s)
	handshakes := h.rt.Instances()[0].Handshakes()
	if len(handshakes) != 1 {
		t.Fatalf("browser saw %d handshakes", len(handshakes))
	}
	if got := handshakes[0].Get("Authorization"); got != "" {
		t.Fatalf("the API key was forwarded to the browser: %q", got)
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

func TestBrowserInitiatedCloseIsPropagated(t *testing.T) {
	h := newHarness(t, nil)
	s := h.createSession("")
	conn := h.mustDial(s)
	inst := h.rt.Instances()[0]
	eventually(t, "the browser to see the client", func() bool { return len(inst.Handshakes()) == 1 })
	time.Sleep(50 * time.Millisecond) // let the echo loop register the connection
	inst.CloseClients(websocket.StatusCode(4321), "custom reason")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != 4321 || ce.Reason != "custom reason" {
		t.Fatalf("close = %v, want code 4321 with the browser's reason", err)
	}
	if got, _ := h.sessions.Get(s.ID); got.Status != sessions.StatusRunning {
		t.Fatal("a closed CDP connection must not end the session")
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
		return strings.TrimSuffix(got.CDPURL, "/v1/sessions/"+s.ID+"/cdp")
	}
	viaMgmt := func() string {
		var got sessionResponse
		h.manage(http.MethodGet, "/v1/sessions/"+s.ID, "").decode(t, &got)
		return strings.TrimSuffix(got.CDPURL, "/v1/sessions/"+s.ID+"/cdp")
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
	file := h.backend.Config().File
	file.Server.PublicURL = "https://browsers.example.com/"
	if _, err := h.backend.UpdateConfig(file); err != nil {
		t.Fatal(err)
	}
	if got := get("Host", "127.0.0.1:3000"); got != "wss://browsers.example.com" {
		t.Errorf("pinned public URL: %q", got)
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
