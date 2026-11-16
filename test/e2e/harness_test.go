//go:build e2e

// Package e2e runs the real tabtenderd binary against a real browser.
//
// Environment:
//
//	TABTENDERD_PATH            use a prebuilt binary instead of building one
//	TABTENDER_CHROME_PATH      browser to launch (default: auto-discovery)
//	TABTENDER_E2E_CHROME_ARGS  extra browser flags, space separated
//	                           (e.g. "--no-sandbox" on Linux CI runners)
//	TABTENDER_E2E_HEADED=1     also run the headed-browser test
//	TABTENDER_E2E_TUNNEL=1     also run the real Cloudflare Quick Tunnel test
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
)

const testAPIKey = "e2e-key-0123456789abcdef0123456789abcdef"

var daemonBinary string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if path := os.Getenv("TABTENDERD_PATH"); path != "" {
		daemonBinary = path
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "tabtender-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best effort
	daemonBinary = filepath.Join(dir, "tabtenderd")
	if runtime.GOOS == "windows" {
		daemonBinary += ".exe"
	}
	build := exec.Command("go", "build", "-o", daemonBinary, "../../cmd/tabtenderd")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building tabtenderd failed:", err)
		return 1
	}
	return m.Run()
}

// shortTempDir returns a temporary directory with a short path: Unix socket
// paths are limited to roughly 100 bytes.
func shortTempDir(t *testing.T) string {
	t.Helper()
	base := ""
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "tt-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Cleanup(func() {
		// Browsers may still be flushing files right after being killed.
		for range 50 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
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

// daemonSpec describes one tabtenderd installation: its directories and
// settings survive restarts of the process.
type daemonSpec struct {
	dir        string
	port       int
	configYAML string
	env        map[string]string
	noAPIKey   bool
}

func newSpec(t *testing.T) *daemonSpec {
	return &daemonSpec{dir: shortTempDir(t), port: freePort(t), env: map[string]string{}}
}

func (s *daemonSpec) dataDir() string    { return filepath.Join(s.dir, "data") }
func (s *daemonSpec) configPath() string { return filepath.Join(s.dir, "config.yaml") }
func (s *daemonSpec) socketPath() string { return filepath.Join(s.dir, "mgmt.sock") }
func (s *daemonSpec) baseURL() string    { return fmt.Sprintf("http://127.0.0.1:%d", s.port) }

type daemon struct {
	t       *testing.T
	spec    *daemonSpec
	cmd     *exec.Cmd
	output  *syncBuffer
	exited  chan struct{}
	exitErr error
	apiKey  string
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// launch starts tabtenderd without waiting for readiness.
func (s *daemonSpec) launch(t *testing.T) *daemon {
	t.Helper()
	config := s.configYAML
	if args := strings.Fields(os.Getenv("TABTENDER_E2E_CHROME_ARGS")); len(args) > 0 && !strings.Contains(config, "browser:") {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = strconv.Quote(a)
		}
		config += "\nbrowser:\n  extraArgs: [" + strings.Join(quoted, ", ") + "]\n"
	}
	if _, err := os.Stat(s.configPath()); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(s.configPath(), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"TABTENDER_CONFIG_PATH":     s.configPath(),
		"TABTENDER_DATA_DIR":        s.dataDir(),
		"TABTENDER_PORT":            strconv.Itoa(s.port),
		"TABTENDER_MGMT_SOCKET":     s.socketPath(),
		"TABTENDER_SECRETS_BACKEND": "file",
	}
	if !s.noAPIKey {
		env["TABTENDER_API_KEY"] = testAPIKey
	}
	for k, v := range s.env {
		env[k] = v
	}
	d := &daemon{t: t, spec: s, output: &syncBuffer{}, exited: make(chan struct{}), apiKey: env["TABTENDER_API_KEY"]}
	d.cmd = exec.Command(daemonBinary)
	d.cmd.Env = os.Environ()
	for k, v := range env {
		if v == "" {
			continue
		}
		d.cmd.Env = append(d.cmd.Env, k+"="+v)
	}
	d.cmd.Stdout, d.cmd.Stderr = d.output, d.output
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		d.exitErr = d.cmd.Wait()
		close(d.exited)
	}()
	t.Cleanup(func() {
		d.stop()
		if t.Failed() {
			t.Logf("tabtenderd output:\n%s", d.output.String())
		}
	})
	return d
}

// start launches tabtenderd and waits until it serves requests.
func (s *daemonSpec) start(t *testing.T) *daemon {
	t.Helper()
	d := s.launch(t)
	d.waitReady()
	return d
}

// waitReady blocks until the daemon answers HTTP requests and, where the
// platform has one, its management socket accepts connections.
func (d *daemon) waitReady() {
	d.t.Helper()
	probe := &http.Client{Timeout: 2 * time.Second}
	defer probe.CloseIdleConnections()
	ready := func() bool {
		resp, err := probe.Get(d.spec.baseURL() + "/v1/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		if runtime.GOOS == "windows" {
			return true
		}
		conn, err := net.DialTimeout("unix", d.spec.socketPath(), time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-d.exited:
			d.t.Fatalf("tabtenderd exited during startup: %v\n%s", d.exitErr, d.output.String())
		default:
		}
		if ready() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatalf("tabtenderd did not become ready\n%s", d.output.String())
}

func (d *daemon) running() bool {
	select {
	case <-d.exited:
		return false
	default:
		return true
	}
}

// stop shuts the daemon down gracefully where the platform allows it.
func (d *daemon) stop() {
	if !d.running() {
		return
	}
	if runtime.GOOS == "windows" {
		_ = d.cmd.Process.Kill()
	} else {
		_ = d.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-d.exited:
	case <-time.After(60 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.exited
	}
}

// kill simulates a daemon crash.
func (d *daemon) kill() {
	_ = d.cmd.Process.Kill()
	<-d.exited
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type session struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	Headless         bool      `json:"headless"`
	Profile          string    `json:"profile"`
	ExpiresAt        time.Time `json:"expiresAt"`
	CDPURL           string    `json:"cdpUrl"`
	ConnectedClients int       `json:"connectedClients"`
	EndReason        string    `json:"endReason"`
}

type result struct {
	status int
	body   []byte
}

func (r result) into(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

func (r result) errorCode(t *testing.T) string {
	t.Helper()
	var e apiError
	r.into(t, &e)
	return e.Error.Code
}

func doRequest(t *testing.T, client *http.Client, method, url, key, body string) result {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	data, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, body: data}
}

var plainClient = &http.Client{Timeout: 90 * time.Second}

// api calls the authenticated network API.
func (d *daemon) api(method, path, body string) result {
	d.t.Helper()
	return doRequest(d.t, plainClient, method, d.spec.baseURL()+path, d.apiKey, body)
}

// manage calls the local management API over the Unix socket.
func (d *daemon) manage(method, path, body string) result {
	d.t.Helper()
	if runtime.GOOS == "windows" {
		d.t.Fatal("the management socket is not available on Windows")
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", d.spec.socketPath())
		},
	}}
	defer client.CloseIdleConnections()
	return doRequest(d.t, client, method, "http://tabtender"+path, "", body)
}

func (d *daemon) createSession(body string) session {
	d.t.Helper()
	if body == "" {
		body = `{"headless": true}`
	}
	res := d.api(http.MethodPost, "/v1/sessions", body)
	if res.status != http.StatusCreated {
		d.t.Fatalf("create session: %d %s", res.status, res.body)
	}
	var s session
	res.into(d.t, &s)
	return s
}

func (d *daemon) getSession(id string) (session, int) {
	d.t.Helper()
	res := d.api(http.MethodGet, "/v1/sessions/"+id, "")
	var s session
	if res.status == http.StatusOK {
		res.into(d.t, &s)
	}
	return s, res.status
}

func (d *daemon) listSessions() []session {
	d.t.Helper()
	var list struct {
		Sessions []session `json:"sessions"`
	}
	d.api(http.MethodGet, "/v1/sessions", "").into(d.t, &list)
	return list.Sessions
}

func (d *daemon) deleteSession(id string) {
	d.t.Helper()
	if res := d.api(http.MethodDelete, "/v1/sessions/"+id, ""); res.status != http.StatusOK {
		d.t.Fatalf("delete session: %d %s", res.status, res.body)
	}
}

func (d *daemon) ephemeralProfileDir(id string) string {
	return filepath.Join(d.spec.dataDir(), "profiles", "ephemeral", id)
}

func (d *daemon) namedProfileDir(name string) string {
	return filepath.Join(d.spec.dataDir(), "profiles", "named", name)
}

// browserProcess finds the main browser process using profileDir.
func browserProcess(t *testing.T, profileDir string) (platform.Process, bool) {
	t.Helper()
	procs, err := platform.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		main, owns := true, false
		for _, arg := range p.Args {
			if strings.HasPrefix(arg, "--type=") {
				main = false
			}
			if strings.EqualFold(arg, "--user-data-dir="+profileDir) {
				owns = true
			}
		}
		if main && owns {
			return p, true
		}
	}
	return platform.Process{}, false
}

func mustBrowserProcess(t *testing.T, profileDir string) platform.Process {
	t.Helper()
	p, ok := browserProcess(t, profileDir)
	if !ok {
		t.Fatalf("no browser process is using %s", profileDir)
	}
	return p
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func assertGone(t *testing.T, what string, pid int) {
	t.Helper()
	eventually(t, 20*time.Second, what+" to exit", func() bool { return !platform.Alive(pid) })
}

func assertDirRemoved(t *testing.T, dir string) {
	t.Helper()
	eventually(t, 20*time.Second, dir+" to be removed", func() bool {
		_, err := os.Stat(dir)
		return errors.Is(err, os.ErrNotExist)
	})
}

// cdpClient is a minimal raw CDP client.
type cdpClient struct {
	t    *testing.T
	conn *websocket.Conn
	next int
}

func dialCDP(url, key string, httpClient *http.Client) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	header := http.Header{}
	if key != "" {
		header.Set("Authorization", "Bearer "+key)
	}
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header, HTTPClient: httpClient})
	if conn != nil {
		conn.SetReadLimit(-1)
	}
	return conn, resp, err
}

func (d *daemon) connect(s session) *cdpClient {
	d.t.Helper()
	conn, _, err := dialCDP(s.CDPURL, d.apiKey, nil)
	if err != nil {
		d.t.Fatalf("connect to %s: %v", s.CDPURL, err)
	}
	d.t.Cleanup(func() { _ = conn.CloseNow() })
	return &cdpClient{t: d.t, conn: conn}
}

func (c *cdpClient) close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

// call sends a CDP command and returns its result, skipping events.
func (c *cdpClient) call(method string, params any, sessionID ...string) json.RawMessage {
	c.t.Helper()
	res, err := c.tryCall(method, params, sessionID...)
	if err != nil {
		c.t.Fatalf("CDP %s: %v", method, err)
	}
	return res
}

func (c *cdpClient) tryCall(method string, params any, sessionID ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c.next++
	msg := map[string]any{"id": c.next, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if len(sessionID) > 0 {
		msg["sessionId"] = sessionID[0]
	}
	data, _ := json.Marshal(msg)
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, err
	}
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			return nil, err
		}
		if reply.ID != c.next {
			continue // an event or a stale reply
		}
		if reply.Error != nil {
			return nil, errors.New(reply.Error.Message)
		}
		return reply.Result, nil
	}
}

// openPage opens a tab and returns its target ID.
func (c *cdpClient) openPage(url string) string {
	c.t.Helper()
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(c.call("Target.createTarget", map[string]any{"url": url}), &created); err != nil {
		c.t.Fatal(err)
	}
	return created.TargetID
}

// evaluate runs JavaScript in a tab and returns the JSON-encoded value.
func (c *cdpClient) evaluate(targetID, expression string) string {
	c.t.Helper()
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(c.call("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}), &attached); err != nil {
		c.t.Fatal(err)
	}
	var evaluated struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	raw := c.call("Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, attached.SessionID)
	if err := json.Unmarshal(raw, &evaluated); err != nil {
		c.t.Fatal(err)
	}
	return string(evaluated.Result.Value)
}

func (c *cdpClient) pageURLs() []string {
	c.t.Helper()
	var targets struct {
		TargetInfos []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(c.call("Target.getTargets", nil), &targets); err != nil {
		c.t.Fatal(err)
	}
	var urls []string
	for _, info := range targets.TargetInfos {
		if info.Type == "page" {
			urls = append(urls, info.URL)
		}
	}
	return urls
}
