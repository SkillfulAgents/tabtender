package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/SkillfulAgents/tabtender/internal/runtime"
	"github.com/SkillfulAgents/tabtender/internal/runtime/runtimetest"
	"github.com/SkillfulAgents/tabtender/internal/tunnel"
)

const testKey = "daemon-test-key-0123456789abcdef0123456789"

// fakeTunnel is a third-party style tunnel provider.
type fakeTunnel struct{ url string }

func (f fakeTunnel) Start(context.Context, int) (string, error) { return f.url, nil }
func (f fakeTunnel) Stop(context.Context) error                 { return nil }
func (f fakeTunnel) Status() tunnel.TunnelStatus {
	return tunnel.TunnelStatus{State: tunnel.StateRunning, URL: f.url}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // test
	return ln.Addr().(*net.TCPAddr).Port
}

// TestAdaptersAreReplaceable runs the complete daemon with a browser runtime
// and a tunnel provider that the core has never heard of. Nothing in the
// session manager or API layer changes: adapters are selected by name
// through the factories (acceptance criterion 8).
func TestAdaptersAreReplaceable(t *testing.T) {
	dir, err := os.MkdirTemp("", "tt-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	port := freePort(t)
	env := map[string]string{
		"TABTENDER_CONFIG_PATH":     filepath.Join(dir, "config.yaml"),
		"TABTENDER_DATA_DIR":        filepath.Join(dir, "data"),
		"TABTENDER_MGMT_SOCKET":     filepath.Join(dir, "m.sock"),
		"TABTENDER_SECRETS_BACKEND": "file",
		"TABTENDER_PORT":            fmt.Sprint(port),
		"TABTENDER_API_KEY":         testKey,
		"TABTENDER_BROWSER_RUNTIME": runtimetest.Kind,
		"TABTENDER_TUNNEL_PROVIDER": "carrier-pigeon",
	}
	fakeRuntime := runtimetest.New()
	runtimes := runtime.NewRegistry()
	fakeRuntime.Register(runtimes)
	tunnels := tunnel.NewRegistry()
	tunnels.Register("carrier-pigeon", func(tunnel.TunnelConfig) (tunnel.TunnelProvider, error) {
		return fakeTunnel{url: "https://pigeon.example"}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
			Runtimes:  runtimes,
			Tunnels:   tunnels,
		})
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon exited with %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("daemon did not stop")
		}
	}
	t.Cleanup(stop)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	call := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer resp.Body.Close() //nolint:errcheck // test
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if status, _ := call(http.MethodGet, "/v1/health", ""); status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	status, body := call(http.MethodPost, "/v1/sessions", `{"ttlSeconds": 60}`)
	if status != http.StatusCreated {
		t.Fatalf("create session on the fake runtime: %d %s", status, body)
	}
	var session struct {
		ID     string `json:"id"`
		CDPURL string `json:"cdpUrl"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatal(err)
	}
	if len(fakeRuntime.Instances()) != 1 {
		t.Fatal("the session was not launched on the substituted runtime")
	}

	// The CDP proxy works against the substituted runtime unchanged.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, session.CDPURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + testKey}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(dialCtx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if typ, data, err := conn.Read(dialCtx); err != nil || typ != websocket.MessageBinary || len(data) != 3 {
		t.Fatalf("echo through the proxy: %v %v %v", typ, data, err)
	}
	_ = conn.CloseNow()

	_, health := call(http.MethodGet, "/v1/health", "")
	for _, want := range []string{`"runtime":"fake"`, `"provider":"carrier-pigeon"`, `"url":"https://pigeon.example"`} {
		if !strings.Contains(string(health), want) {
			t.Fatalf("health = %s, want it to contain %s", health, want)
		}
	}
	// The substituted tunnel's URL is what remote callers are handed.
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/sessions/"+session.ID, nil)
	req.Host = "pigeon.example"
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	remote, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(remote), `"cdpUrl":"wss://pigeon.example/v1/sessions/`+session.ID+`/cdp?token=`) {
		t.Fatalf("remote view = %s", remote)
	}

	stop()
	if fakeRuntime.Running() != 0 {
		t.Fatal("shutdown left browsers of the substituted runtime running")
	}
}

func TestStartupFailsOnUnknownAdapters(t *testing.T) {
	for variable, want := range map[string]string{
		"TABTENDER_BROWSER_RUNTIME": "unknown browser runtime",
		"TABTENDER_TUNNEL_PROVIDER": "unknown tunnel provider",
	} {
		t.Run(variable, func(t *testing.T) { testUnknownAdapter(t, variable, want) })
	}
}

func testUnknownAdapter(t *testing.T, variable, want string) {
	dir := t.TempDir()
	env := map[string]string{
		"TABTENDER_CONFIG_PATH":     filepath.Join(dir, "config.yaml"),
		"TABTENDER_DATA_DIR":        filepath.Join(dir, "data"),
		"TABTENDER_SECRETS_BACKEND": "file",
		"TABTENDER_PORT":            fmt.Sprint(freePort(t)),
		"TABTENDER_API_KEY":         testKey,
		"TABTENDER_MGMT_SOCKET":     filepath.Join(dir, "m.sock"),
	}
	env[variable] = "netscape"
	err := Run(context.Background(), Options{LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v", err)
	}
}
