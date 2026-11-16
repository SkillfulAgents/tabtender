package chrome

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/SkillfulAgents/tabtender/internal/runtime"
	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
)

func TestLaunchArgs(t *testing.T) {
	headed := launchArgs(runtime.BrowserOptions{ProfileDir: "/profiles/a"}, nil)
	if !slices.Contains(headed, "--user-data-dir=/profiles/a") || !slices.Contains(headed, "--remote-debugging-port=0") {
		t.Fatalf("args = %v", headed)
	}
	for _, arg := range headed {
		if strings.HasPrefix(arg, "--headless") {
			t.Fatalf("headed launch carries %s", arg)
		}
		// Chrome binds the DevTools server to loopback unless told otherwise.
		if strings.HasPrefix(arg, "--remote-debugging-address") || strings.HasPrefix(arg, "--remote-allow-origins") {
			t.Fatalf("launch flags must not widen CDP exposure: %s", arg)
		}
	}
	headless := launchArgs(runtime.BrowserOptions{ProfileDir: "/p", Headless: true}, []string{"--no-sandbox"})
	if !slices.Contains(headless, "--headless=new") || !slices.Contains(headless, "--no-sandbox") {
		t.Fatalf("args = %v", headless)
	}
	if headless[len(headless)-1] != "about:blank" {
		t.Fatalf("the start URL must come last: %v", headless)
	}
}

func TestReadEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), activePortFile)
	cases := map[string]string{
		"51234\n/devtools/browser/abc-123\n": "ws://127.0.0.1:51234/devtools/browser/abc-123",
		"51234\r\n/devtools/browser/x\r\n":   "ws://127.0.0.1:51234/devtools/browser/x",
		"51234\n":                            "",
		"":                                   "",
		"not-a-port\n/devtools/browser/x":    "",
		"0\n/devtools/browser/x":             "",
		"51234\ndevtools":                    "",
	}
	for content, want := range cases {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := readEndpoint(path)
		if got != want || ok != (want != "") {
			t.Errorf("readEndpoint(%q) = %q, %v; want %q", content, got, ok, want)
		}
	}
	if _, ok := readEndpoint(filepath.Join(t.TempDir(), "missing")); ok {
		t.Error("a missing file must not yield an endpoint")
	}
}

func TestExecutableResolution(t *testing.T) {
	missing, _ := New(runtime.RuntimeConfig{ExecutablePath: filepath.Join(t.TempDir(), "nope")})
	if _, err := missing.(*Runtime).Executable(); err == nil {
		t.Fatal("expected an error for a missing executable")
	}
	if _, err := missing.Launch(context.Background(), runtime.BrowserOptions{ProfileDir: t.TempDir()}); err == nil {
		t.Fatal("Launch must fail when the browser is missing")
	}
	self, _ := os.Executable()
	explicit, _ := New(runtime.RuntimeConfig{ExecutablePath: self})
	if got, err := explicit.(*Runtime).Executable(); err != nil || got != self {
		t.Fatalf("Executable = %q, %v", got, err)
	}
	if _, err := explicit.Launch(context.Background(), runtime.BrowserOptions{}); err == nil {
		t.Fatal("Launch must require a profile directory")
	}
}

func TestRegister(t *testing.T) {
	registry := runtime.NewRegistry()
	Register(registry)
	rt, err := registry.Create(Kind, runtime.RuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.(runtime.ExecutableResolver); !ok {
		t.Fatal("the chrome runtime must report its executable")
	}
	if _, err := registry.Create("netscape", runtime.RuntimeConfig{}); err == nil {
		t.Fatal("expected an error for an unknown runtime")
	}
}

// TestLaunchAndClose drives a real browser when one is available.
func TestLaunchAndClose(t *testing.T) {
	rt, _ := New(runtime.RuntimeConfig{
		ExecutablePath: os.Getenv("TABTENDER_CHROME_PATH"),
		ExtraArgs:      strings.Fields(os.Getenv("TABTENDER_E2E_CHROME_ARGS")),
	})
	if _, err := rt.(*Runtime).Executable(); err != nil {
		t.Skipf("no browser available: %v", err)
	}
	profile := filepath.Join(t.TempDir(), "profile")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	inst, err := rt.Launch(ctx, runtime.BrowserOptions{Headless: true, ProfileDir: profile})
	if err != nil {
		t.Fatal(err)
	}
	if !inst.IsRunning() || inst.PID() <= 0 || !platform.Alive(inst.PID()) {
		t.Fatalf("instance not running: pid=%d", inst.PID())
	}
	if !strings.HasPrefix(inst.CDPEndpoint(), "ws://127.0.0.1:") {
		t.Fatalf("the CDP endpoint must be on loopback: %s", inst.CDPEndpoint())
	}
	conn, _, err := websocket.Dial(ctx, inst.CDPEndpoint(), &websocket.DialOptions{HTTPClient: loopbackClient})
	if err != nil {
		t.Fatalf("dial CDP: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Browser.getVersion"}`)); err != nil {
		t.Fatal(err)
	}
	if _, reply, err := conn.Read(ctx); err != nil || !strings.Contains(string(reply), "protocolVersion") {
		t.Fatalf("Browser.getVersion reply = %s (%v)", reply, err)
	}
	_ = conn.CloseNow()

	if err := rt.Close(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if inst.IsRunning() || platform.Alive(inst.PID()) {
		t.Fatal("the browser is still running after Close")
	}
	if err := rt.Close(ctx, inst); err != nil {
		t.Fatalf("closing twice must be harmless: %v", err)
	}
}
