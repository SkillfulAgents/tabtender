//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
)

// Acceptance 1-3: five concurrent sessions, each an independent native
// browser process with a working authenticated CDP endpoint and isolated
// browsing state.
func TestFiveConcurrentIsolatedSessions(t *testing.T) {
	d := newSpec(t).start(t)
	const n = 5
	sessions := make([]session, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessions[i] = d.createSession("")
		}()
	}
	wg.Wait()

	pids := map[int]bool{}
	clients := make([]*cdpClient, n)
	for i, s := range sessions {
		if s.Status != "running" || !strings.HasPrefix(s.CDPURL, "ws://127.0.0.1:") {
			t.Fatalf("session %d: %+v", i, s)
		}
		proc := mustBrowserProcess(t, d.ephemeralProfileDir(s.ID))
		pids[proc.PID] = true
		clients[i] = d.connect(s)
		var version struct {
			Product string `json:"product"`
		}
		if err := json.Unmarshal(clients[i].call("Browser.getVersion", nil), &version); err != nil || version.Product == "" {
			t.Fatalf("session %d: Browser.getVersion = %+v (%v)", i, version, err)
		}
	}
	if len(pids) != n {
		t.Fatalf("expected %d distinct browser processes, got %d", n, len(pids))
	}
	if got := len(d.listSessions()); got != n {
		t.Fatalf("listed %d sessions, want %d", got, n)
	}

	// Each browser has its own cookie jar.
	for i, c := range clients {
		c.call("Storage.setCookies", map[string]any{"cookies": []map[string]any{
			{"name": "owner", "value": fmt.Sprintf("session-%d", i), "url": "http://isolation.test"},
		}})
	}
	for i, c := range clients {
		var jar struct {
			Cookies []struct{ Name, Value string } `json:"cookies"`
		}
		if err := json.Unmarshal(c.call("Storage.getCookies", nil), &jar); err != nil {
			t.Fatal(err)
		}
		if len(jar.Cookies) != 1 || jar.Cookies[0].Value != fmt.Sprintf("session-%d", i) {
			t.Fatalf("session %d sees cookies %+v", i, jar.Cookies)
		}
	}

	// Lifecycles are independent: ending one session leaves the rest alone.
	d.deleteSession(sessions[0].ID)
	if _, err := clients[1].tryCall("Browser.getVersion", nil); err != nil {
		t.Fatalf("session 1 was disturbed by deleting session 0: %v", err)
	}
	if got := len(d.listSessions()); got != n-1 {
		t.Fatalf("listed %d sessions after delete, want %d", got, n-1)
	}
}

// Acceptance 4: clients can disconnect and reconnect without losing the
// session, and a disconnect never terminates it.
func TestReconnectKeepsSessionState(t *testing.T) {
	d := newSpec(t).start(t)
	s := d.createSession("")

	first := d.connect(s)
	target := first.openPage("about:blank")
	first.evaluate(target, `document.title = "still here"; window.marker = 42`)
	if got, _ := d.getSession(s.ID); got.ConnectedClients != 1 {
		t.Fatalf("connectedClients = %d, want 1", got.ConnectedClients)
	}
	first.close()
	eventually(t, 10*time.Second, "the client to be released", func() bool {
		got, _ := d.getSession(s.ID)
		return got.ConnectedClients == 0
	})
	if got, _ := d.getSession(s.ID); got.Status != "running" {
		t.Fatalf("session status after disconnect: %q", got.Status)
	}

	second := d.connect(s)
	if got := second.evaluate(target, `document.title + ":" + window.marker`); got != `"still here:42"` {
		t.Fatalf("page state after reconnect = %s", got)
	}

	// An abrupt disconnect (no close handshake) is just as harmless.
	_ = second.conn.CloseNow()
	third := d.connect(s)
	if got := third.evaluate(target, `window.marker`); got != "42" {
		t.Fatalf("page state after abrupt reconnect = %s", got)
	}
}

// Acceptance 5 (explicit termination): the browser process tree, CDP
// connections, and temporary profile are all reclaimed.
func TestDeleteCleansUpEverything(t *testing.T) {
	d := newSpec(t).start(t)
	s := d.createSession("")
	profile := d.ephemeralProfileDir(s.ID)
	proc := mustBrowserProcess(t, profile)
	client := d.connect(s)
	client.call("Browser.getVersion", nil)

	d.deleteSession(s.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := client.conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("client close = %v, want going away", err)
	}
	assertGone(t, "the browser", proc.PID)
	assertDirRemoved(t, profile)
	if got, status := d.getSession(s.ID); status != http.StatusOK || got.Status != "terminated" || got.EndReason != "deleted" {
		t.Fatalf("session after delete: %d %+v", status, got)
	}
	if _, resp, err := dialCDP(s.CDPURL, d.apiKey, nil); err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("connecting to a terminated session: err=%v", err)
	}
	if got := len(d.listSessions()); got != 0 {
		t.Fatalf("%d sessions still listed", got)
	}
}

// Acceptance 5 (TTL): expiry is enforced even while a client is connected.
func TestTTLExpiryCleansUp(t *testing.T) {
	d := newSpec(t).start(t)
	s := d.createSession(`{"headless": true, "ttlSeconds": 3}`)
	profile := d.ephemeralProfileDir(s.ID)
	proc := mustBrowserProcess(t, profile)
	client := d.connect(s)
	client.call("Browser.getVersion", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := client.conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("client close on expiry = %v, want going away", err)
	}
	if time.Now().Before(s.ExpiresAt.Add(-500 * time.Millisecond)) {
		t.Fatalf("session ended before its expiry time %s", s.ExpiresAt)
	}
	eventually(t, 10*time.Second, "the session to be marked expired", func() bool {
		got, _ := d.getSession(s.ID)
		return got.Status == "terminated" && got.EndReason == "expired"
	})
	assertGone(t, "the browser", proc.PID)
	assertDirRemoved(t, profile)
}

func TestTTLValidation(t *testing.T) {
	spec := newSpec(t)
	spec.env["TABTENDER_MAX_SESSION_TTL"] = "60"
	spec.env["TABTENDER_DEFAULT_SESSION_TTL"] = "30"
	d := spec.start(t)

	res := d.api(http.MethodPost, "/v1/sessions", `{"headless": true, "ttlSeconds": 61}`)
	if res.status != http.StatusBadRequest || res.errorCode(t) != "validation_error" || !strings.Contains(string(res.body), "60") {
		t.Fatalf("ttl above maximum: %d %s", res.status, res.body)
	}
	s := d.createSession("")
	if ttl := time.Until(s.ExpiresAt); ttl < 20*time.Second || ttl > 31*time.Second {
		t.Fatalf("default TTL from the environment not applied: %s", ttl)
	}
}

// A browser that dies on its own is detected and its resources reclaimed.
func TestBrowserCrashIsDetected(t *testing.T) {
	spec := newSpec(t)
	spec.env["TABTENDER_MAX_SESSIONS"] = "1"
	d := spec.start(t)
	s := d.createSession("")
	profile := d.ephemeralProfileDir(s.ID)
	proc := mustBrowserProcess(t, profile)

	if err := platform.KillTree(proc.PID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "the crash to be detected", func() bool {
		got, _ := d.getSession(s.ID)
		return got.Status == "terminated" && got.EndReason == "crashed"
	})
	assertDirRemoved(t, profile)
	// The slot is free again.
	d.createSession("")
}

// Acceptance 6: restarting the service deterministically reconciles recorded
// sessions and orphaned browser processes.
func TestRestartReconcilesOrphans(t *testing.T) {
	spec := newSpec(t)
	first := spec.start(t)
	ephemeral := first.createSession("")
	first.createSession(`{"headless": true, "profile": "keeper"}`)
	ephemeralDir := first.ephemeralProfileDir(ephemeral.ID)
	namedDir := first.namedProfileDir("keeper")
	procs := []platform.Process{mustBrowserProcess(t, ephemeralDir), mustBrowserProcess(t, namedDir)}

	first.kill() // simulate a daemon crash: no graceful cleanup happens

	if runtime.GOOS != "windows" {
		// On Unix the browsers outlive the daemon; reconciliation must
		// find and stop them. (On Windows a job object already killed them.)
		for _, p := range procs {
			if !platform.Alive(p.PID) {
				t.Fatalf("expected orphaned browser %d to still be running", p.PID)
			}
		}
		if _, err := os.Stat(ephemeralDir); err != nil {
			t.Fatalf("expected leftover ephemeral profile: %v", err)
		}
	}

	second := spec.start(t)
	for _, p := range procs {
		assertGone(t, "orphaned browser", p.PID)
	}
	assertDirRemoved(t, ephemeralDir)
	if _, err := os.Stat(namedDir); err != nil {
		t.Fatalf("the named profile must survive reconciliation: %v", err)
	}
	if got := second.listSessions(); len(got) != 0 {
		t.Fatalf("sessions after restart: %+v", got)
	}
	if _, status := second.getSession(ephemeral.ID); status != http.StatusNotFound {
		t.Fatalf("old session lookup after restart: %d", status)
	}
	if !strings.Contains(second.output.String(), "reconciled previous state") {
		t.Fatal("reconciliation was not reported in the log")
	}

	// The service is fully usable again, including the previously locked profile.
	again := second.createSession(`{"headless": true, "profile": "keeper"}`)
	second.connect(again).call("Browser.getVersion", nil)
}

func TestSecondInstanceOnSameDataDirIsRefused(t *testing.T) {
	spec := newSpec(t)
	spec.start(t)

	clone := *spec
	clone.port = freePort(t)
	clone.env = map[string]string{"TABTENDER_MGMT_SOCKET": filepath.Join(spec.dir, "other.sock")}
	second := clone.launch(t)
	select {
	case <-second.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the second instance kept running")
	}
	if second.exitErr == nil || !strings.Contains(second.output.String(), "already running") {
		t.Fatalf("exit=%v output=%s", second.exitErr, second.output.String())
	}
}

// Acceptance 7: environment variables override file configuration.
func TestEnvironmentOverridesConfigFile(t *testing.T) {
	spec := newSpec(t)
	filePort := freePort(t)
	spec.configYAML = fmt.Sprintf("server:\n  port: %d\nsessions:\n  maxConcurrent: 5\n  defaultTTL: 120\n", filePort)
	spec.env["TABTENDER_MAX_SESSIONS"] = "1"
	d := spec.start(t) // TABTENDER_PORT (spec.port) must win over the file

	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", filePort), 500*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("the service listens on the file-configured port despite TABTENDER_PORT")
	}
	var health struct {
		Sessions struct{ Max int } `json:"sessions"`
	}
	d.api(http.MethodGet, "/v1/health", "").into(t, &health)
	if health.Sessions.Max != 1 {
		t.Fatalf("max sessions = %d, want the environment value 1", health.Sessions.Max)
	}
	s := d.createSession("")
	if ttl := time.Until(s.ExpiresAt); ttl > 121*time.Second || ttl < 100*time.Second {
		t.Fatalf("file-configured default TTL not applied: %s", ttl)
	}
	if res := d.api(http.MethodPost, "/v1/sessions", `{"headless": true}`); res.status != http.StatusTooManyRequests || res.errorCode(t) != "capacity_exceeded" {
		t.Fatalf("second session with TABTENDER_MAX_SESSIONS=1: %d %s", res.status, res.body)
	}

	if runtime.GOOS == "windows" {
		return
	}
	var view struct {
		File struct {
			Sessions struct{ MaxConcurrent int } `json:"sessions"`
		} `json:"file"`
		Effective struct {
			Sessions struct{ MaxConcurrent int } `json:"sessions"`
		} `json:"effective"`
		EnvOverrides map[string]struct{ Variable string } `json:"envOverrides"`
	}
	d.manage(http.MethodGet, "/mgmt/v1/config", "").into(t, &view)
	if view.File.Sessions.MaxConcurrent != 5 || view.Effective.Sessions.MaxConcurrent != 1 ||
		view.EnvOverrides["sessions.maxConcurrent"].Variable != "TABTENDER_MAX_SESSIONS" ||
		view.EnvOverrides["server.port"].Variable != "TABTENDER_PORT" {
		t.Fatalf("config view = %+v", view)
	}
}

func TestInvalidConfigurationFailsFast(t *testing.T) {
	spec := newSpec(t)
	spec.configYAML = "sessions:\n  maxConcurrent: 0\n"
	d := spec.launch(t)
	select {
	case <-d.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon started with an invalid configuration")
	}
	if d.exitErr == nil || !strings.Contains(d.output.String(), "sessions.maxConcurrent") {
		t.Fatalf("exit=%v output=%s", d.exitErr, d.output.String())
	}
}

func TestAuthenticationIsEnforcedOnTheNetwork(t *testing.T) {
	d := newSpec(t).start(t)
	s := d.createSession("")
	for _, key := range []string{"", "wrong-key", testAPIKey[:len(testAPIKey)-1]} {
		for _, path := range []string{"/v1/health", "/v1/sessions", "/v1/sessions/" + s.ID, "/mgmt/v1/apikey"} {
			if res := doRequest(t, plainClient, http.MethodGet, d.spec.baseURL()+path, key, ""); res.status != http.StatusUnauthorized {
				t.Errorf("GET %s with key %q: %d", path, key, res.status)
			}
		}
		if _, resp, err := dialCDP(s.CDPURL, key, nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("CDP handshake with key %q was not rejected with 401 (err=%v)", key, err)
		}
	}
	// Management routes do not exist on the network listener at all.
	if res := d.api(http.MethodGet, "/mgmt/v1/apikey", ""); res.status != http.StatusNotFound {
		t.Fatalf("management route on the network listener: %d", res.status)
	}
	// Credentials never reach the log.
	if strings.Contains(d.output.String(), testAPIKey) {
		t.Fatal("the API key appears in the daemon log")
	}
	// The browser's own CDP port only listens on loopback and is not part
	// of any API response.
	if strings.Contains(string(d.api(http.MethodGet, "/v1/sessions/"+s.ID, "").body), "devtools") {
		t.Fatal("the raw browser endpoint leaked into the API")
	}
}

// The API key is generated on first start, stored outside the config file,
// stable across restarts, and never passed on to browser processes.
func TestGeneratedAPIKeyLifecycle(t *testing.T) {
	spec := newSpec(t)
	spec.noAPIKey = true
	d := spec.start(t)

	readKey := func(d *daemon) string {
		if runtime.GOOS == "windows" {
			// No management UI on Windows: one-time retrieval file.
			data, err := os.ReadFile(filepath.Join(spec.dataDir(), "secrets", "initial-api-key.txt"))
			if err != nil {
				t.Fatalf("initial key file: %v", err)
			}
			return strings.TrimSpace(string(data))
		}
		var key struct {
			APIKey    string `json:"apiKey"`
			Source    string `json:"source"`
			Rotatable bool   `json:"rotatable"`
		}
		d.manage(http.MethodGet, "/mgmt/v1/apikey", "").into(t, &key)
		if key.Source != "file" || !key.Rotatable {
			t.Fatalf("key metadata = %+v", key)
		}
		return key.APIKey
	}
	key := readKey(d)
	if len(key) != 67 || !strings.HasPrefix(key, "tt_") {
		t.Fatalf("generated key has unexpected shape (%d chars)", len(key))
	}
	d.apiKey = key
	if res := d.api(http.MethodGet, "/v1/health", ""); res.status != http.StatusOK {
		t.Fatalf("generated key rejected: %d", res.status)
	}
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(filepath.Join(spec.dataDir(), "secrets", "initial-api-key.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the one-time key file must disappear after the key is first used")
		}
	}
	if config, _ := os.ReadFile(spec.configPath()); strings.Contains(string(config), key) {
		t.Fatal("the API key was written to the configuration file")
	}

	d.stop()
	restarted := spec.start(t)
	restarted.apiKey = key
	if res := restarted.api(http.MethodGet, "/v1/health", ""); res.status != http.StatusOK {
		t.Fatalf("key did not survive a restart: %d", res.status)
	}

	if runtime.GOOS == "windows" {
		return
	}
	var rotated struct {
		APIKey string `json:"apiKey"`
	}
	restarted.manage(http.MethodPost, "/mgmt/v1/apikey/rotate", "").into(t, &rotated)
	if res := restarted.api(http.MethodGet, "/v1/health", ""); res.status != http.StatusUnauthorized {
		t.Fatalf("the old key still works after rotation: %d", res.status)
	}
	restarted.apiKey = rotated.APIKey
	if res := restarted.api(http.MethodGet, "/v1/health", ""); res.status != http.StatusOK {
		t.Fatalf("the rotated key was rejected: %d", res.status)
	}
}

// Hosts without a management UI are provisioned by dropping a key file.
func TestAdministratorProvisionedKey(t *testing.T) {
	spec := newSpec(t)
	spec.noAPIKey = true
	secretsDir := filepath.Join(spec.dataDir(), "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const provisioned = "administrator-chosen-key-0123456789abcdef"
	dropFile := filepath.Join(secretsDir, "api-key.provision")
	if err := os.WriteFile(dropFile, []byte(provisioned+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := spec.start(t)
	d.apiKey = provisioned
	if res := d.api(http.MethodGet, "/v1/health", ""); res.status != http.StatusOK {
		t.Fatalf("provisioned key rejected: %d %s", res.status, res.body)
	}
	if _, err := os.Stat(dropFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the plaintext provisioning file must be consumed")
	}
	d.stop()
	restarted := spec.start(t)
	restarted.apiKey = provisioned
	if res := restarted.api(http.MethodGet, "/v1/health", ""); res.status != http.StatusOK {
		t.Fatalf("provisioned key did not persist: %d", res.status)
	}
}

func TestBrowsersDoNotInheritCredentials(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reading another process's environment is only straightforward on Linux")
	}
	d := newSpec(t).start(t)
	s := d.createSession("")
	proc := mustBrowserProcess(t, d.ephemeralProfileDir(s.ID))
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", proc.PID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(environ), "TABTENDER_") {
		t.Fatal("the browser process inherited TABTENDER_* variables")
	}
}

func TestGracefulShutdownStopsBrowsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("graceful stop on Windows goes through the Service Control Manager (covered by the service test)")
	}
	d := newSpec(t).start(t)
	a, b := d.createSession(""), d.createSession(`{"headless": true, "profile": "p"}`)
	procA := mustBrowserProcess(t, d.ephemeralProfileDir(a.ID))
	procB := mustBrowserProcess(t, d.namedProfileDir(b.Profile))

	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.exited:
	case <-time.After(60 * time.Second):
		t.Fatal("the daemon did not exit on SIGTERM")
	}
	if d.exitErr != nil {
		t.Fatalf("exit status after SIGTERM: %v", d.exitErr)
	}
	assertGone(t, "browser A", procA.PID)
	assertGone(t, "browser B", procB.PID)
	assertDirRemoved(t, d.ephemeralProfileDir(a.ID))
	if _, err := os.Stat(d.spec.socketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the management socket was left behind")
	}
}

// Settings changed through the management channel apply live and persist.
func TestManagementConfigAppliesLiveAndPersists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the local management channel is not available on Windows")
	}
	spec := newSpec(t)
	d := spec.start(t)

	res := d.manage(http.MethodPut, "/mgmt/v1/config", `{"sessions": {"defaultTTL": 90, "maxTTL": 600, "maxConcurrent": 1}}`)
	if res.status != http.StatusOK {
		t.Fatalf("PUT config: %d %s", res.status, res.body)
	}
	s := d.createSession("") // no restart needed
	if ttl := time.Until(s.ExpiresAt); ttl > 91*time.Second || ttl < 70*time.Second {
		t.Fatalf("new default TTL not applied live: %s", ttl)
	}
	if res := d.api(http.MethodPost, "/v1/sessions", `{"headless": true}`); res.status != http.StatusTooManyRequests {
		t.Fatalf("new concurrency limit not applied live: %d", res.status)
	}

	// A restart-required setting is saved but flagged, not applied.
	otherPort := freePort(t)
	res = d.manage(http.MethodPut, "/mgmt/v1/config", fmt.Sprintf(`{"server": {"host": "127.0.0.1", "port": %d}}`, otherPort))
	var view struct {
		PendingRestart []string `json:"pendingRestart"`
	}
	res.into(t, &view)
	if len(view.PendingRestart) != 0 {
		// TABTENDER_PORT pins the port in this harness, so nothing is pending.
		t.Fatalf("pendingRestart = %v although the port is environment-controlled", view.PendingRestart)
	}

	d.stop()
	restarted := spec.start(t)
	var health struct {
		Sessions struct{ Max int } `json:"sessions"`
	}
	restarted.api(http.MethodGet, "/v1/health", "").into(t, &health)
	if health.Sessions.Max != 1 {
		t.Fatalf("settings did not persist across a restart: max=%d", health.Sessions.Max)
	}
	saved, _ := os.ReadFile(spec.configPath())
	if !strings.Contains(string(saved), fmt.Sprintf("port: %d", otherPort)) {
		t.Fatalf("file-layer port not saved:\n%s", saved)
	}
}

// Acceptance 13: sessions run in a native, headed browser.
func TestHeadedSession(t *testing.T) {
	if os.Getenv("TABTENDER_E2E_HEADED") == "" {
		t.Skip("set TABTENDER_E2E_HEADED=1 to run (needs an interactive desktop)")
	}
	d := newSpec(t).start(t)
	s := d.createSession(`{}`) // browser.headless defaults to false
	if s.Headless {
		t.Fatal("expected a headed session by default")
	}
	proc := mustBrowserProcess(t, d.ephemeralProfileDir(s.ID))
	for _, arg := range proc.Args {
		if strings.HasPrefix(arg, "--headless") {
			t.Fatalf("headed session was launched with %s", arg)
		}
	}
	client := d.connect(s)
	target := client.openPage("about:blank")
	// A real window has non-zero outer dimensions.
	if got := client.evaluate(target, `window.outerWidth > 0 && window.outerHeight > 0`); got != "true" {
		t.Fatalf("window dimensions check = %s", got)
	}
	var window struct {
		Bounds struct {
			WindowState string `json:"windowState"`
		} `json:"bounds"`
	}
	if err := json.Unmarshal(client.call("Browser.getWindowForTarget", map[string]any{"targetId": target}), &window); err != nil {
		t.Fatal(err)
	}
	if window.Bounds.WindowState == "" {
		t.Fatal("no window state reported for a headed browser")
	}
}

// Acceptance 15: a Cloudflare Quick Tunnel provides an authenticated public
// endpoint, and sessions created through it return wss:// CDP URLs that work.
func TestQuickTunnelEndToEnd(t *testing.T) {
	if os.Getenv("TABTENDER_E2E_TUNNEL") == "" {
		t.Skip("set TABTENDER_E2E_TUNNEL=1 to run (needs internet access)")
	}
	spec := newSpec(t)
	spec.env["TABTENDER_TUNNEL_PROVIDER"] = "cloudflare-quick"
	d := spec.start(t)

	var health struct {
		Tunnel struct{ Provider, State, URL, Error string } `json:"tunnel"`
	}
	eventually(t, 4*time.Minute, "the tunnel to come up", func() bool {
		d.api(http.MethodGet, "/v1/health", "").into(t, &health)
		return health.Tunnel.State == "running"
	})
	public, err := url.Parse(health.Tunnel.URL)
	if err != nil || public.Scheme != "https" || !strings.HasSuffix(public.Hostname(), ".trycloudflare.com") {
		t.Fatalf("tunnel URL = %q", health.Tunnel.URL)
	}
	t.Logf("public URL: %s", public)

	// New tunnel hostnames take a while to appear in DNS, and asking the
	// system resolver too early caches the negative answer. Resolve through
	// DNS-over-HTTPS and pin the address instead.
	client := pinnedClient(t, public.Hostname())

	// Right after a tunnel registers, Cloudflare's edge can answer 530 for a
	// few seconds until routing has propagated. Once it routes, the very
	// first thing an unauthenticated caller must see is our 401.
	eventually(t, 3*time.Minute, "the Cloudflare edge to route to the tunnel", func() bool {
		req, _ := http.NewRequest(http.MethodGet, public.String()+"/v1/health", nil)
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close() //nolint:errcheck // test
		if resp.StatusCode >= 500 {
			time.Sleep(2 * time.Second)
			return false
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated request through the tunnel: %d", resp.StatusCode)
		}
		return true
	})
	if res := doRequest(t, client, http.MethodGet, public.String()+"/v1/health", "wrong", ""); res.status != http.StatusUnauthorized {
		t.Fatalf("wrong key through the tunnel: %d", res.status)
	}
	created := doRequest(t, client, http.MethodPost, public.String()+"/v1/sessions", testAPIKey, `{"headless": true}`)
	if created.status != http.StatusCreated {
		t.Fatalf("create session through the tunnel: %d %s", created.status, created.body)
	}
	var s session
	created.into(t, &s)
	wantPrefix := "wss://" + public.Host + "/v1/sessions/"
	if !strings.HasPrefix(s.CDPURL, wantPrefix) {
		t.Fatalf("cdpUrl = %q, want prefix %q", s.CDPURL, wantPrefix)
	}

	if _, resp, err := dialCDP(s.CDPURL, "", client); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated CDP through the tunnel: err=%v", err)
	}
	conn, _, err := dialCDP(s.CDPURL, testAPIKey, client)
	if err != nil {
		t.Fatalf("CDP through the tunnel: %v", err)
	}
	defer conn.CloseNow() //nolint:errcheck // test
	cdp := &cdpClient{t: t, conn: conn}
	target := cdp.openPage("about:blank")
	if got := cdp.evaluate(target, `6 * 7`); got != "42" {
		t.Fatalf("evaluate through the tunnel = %s", got)
	}
	// Reconnect through the tunnel.
	cdp.close()
	again, _, err := dialCDP(s.CDPURL, testAPIKey, client)
	if err != nil {
		t.Fatalf("reconnect through the tunnel: %v", err)
	}
	defer again.CloseNow() //nolint:errcheck // test
	if urls := (&cdpClient{t: t, conn: again}).pageURLs(); len(urls) == 0 {
		t.Fatal("no pages after reconnecting through the tunnel")
	}
	if res := doRequest(t, client, http.MethodDelete, public.String()+"/v1/sessions/"+s.ID, testAPIKey, ""); res.status != http.StatusOK {
		t.Fatalf("delete through the tunnel: %d", res.status)
	}
}

// pinnedClient returns an HTTP client that connects to host at an address
// resolved through DNS-over-HTTPS, bypassing the system resolver.
func pinnedClient(t *testing.T, host string) *http.Client {
	t.Helper()
	var address string
	eventually(t, 4*time.Minute, "DNS for "+host, func() bool {
		req, _ := http.NewRequest(http.MethodGet, "https://cloudflare-dns.com/dns-query?type=A&name="+url.QueryEscape(host), nil)
		req.Header.Set("Accept", "application/dns-json")
		resp, err := plainClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close() //nolint:errcheck // test
		var answer struct {
			Answer []struct {
				Type int    `json:"type"`
				Data string `json:"data"`
			} `json:"Answer"`
		}
		if json.NewDecoder(resp.Body).Decode(&answer) != nil {
			return false
		}
		for _, a := range answer.Answer {
			if a.Type == 1 {
				address = a.Data
				return true
			}
		}
		time.Sleep(3 * time.Second)
		return false
	})
	t.Logf("%s resolves to %s", host, address)
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	return &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{
		// HTTP/1.1 keeps the WebSocket upgrade simple.
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, _ := net.SplitHostPort(addr)
			return dialer.DialContext(ctx, network, net.JoinHostPort(address, port))
		},
	}}
}
