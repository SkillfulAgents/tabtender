package chrome

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/runtime"
	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
)

func TestLaunchArgs(t *testing.T) {
	pipe := []string{"--remote-debugging-pipe"}
	headed := launchArgs(runtime.BrowserOptions{ProfileDir: "/profiles/a"}, pipe, nil)
	if !slices.Contains(headed, "--user-data-dir=/profiles/a") || !slices.Contains(headed, "--remote-debugging-pipe") {
		t.Fatalf("args = %v", headed)
	}
	// A covered window must keep drawing, or a screencast of it freezes.
	if !slices.Contains(headed, "--disable-backgrounding-occluded-windows") {
		t.Fatalf("headed launch lets a covered window stop drawing: %v", headed)
	}
	for _, arg := range headed {
		if strings.HasPrefix(arg, "--headless") {
			t.Fatalf("headed launch carries %s", arg)
		}
		// A debugging port would be open to every local process.
		if strings.HasPrefix(arg, "--remote-debugging-port") || strings.HasPrefix(arg, "--remote-debugging-address") {
			t.Fatalf("launch flags must not open a debugging port: %s", arg)
		}
	}
	headless := launchArgs(runtime.BrowserOptions{ProfileDir: "/p", Headless: true}, pipe, []string{"--no-sandbox"})
	if !slices.Contains(headless, "--headless=new") || !slices.Contains(headless, "--no-sandbox") {
		t.Fatalf("args = %v", headless)
	}
	// The debugging pipe would make pages read navigator.webdriver as true.
	for _, args := range [][]string{headed, headless} {
		if !slices.Contains(args, "--disable-blink-features=AutomationControlled") {
			t.Fatalf("launch reports the browser to pages as automated: %v", args)
		}
	}
	if headless[len(headless)-1] != "about:blank" {
		t.Fatalf("the start URL must come last: %v", headless)
	}
}

func TestExtraArgsCannotOpenADebuggingEndpoint(t *testing.T) {
	self, _ := os.Executable()
	for _, arg := range []string{"--remote-debugging-port=9222", "--remote-debugging-address=0.0.0.0", "--remote-allow-origins=*", "--remote-debugging-pipe"} {
		if err := checkExtraArgs([]string{"--no-sandbox", arg}); err == nil {
			t.Errorf("%s was accepted", arg)
		}
		rt, _ := New(runtime.RuntimeConfig{ExecutablePath: self, ExtraArgs: []string{arg}})
		if _, err := rt.Launch(context.Background(), runtime.BrowserOptions{ProfileDir: t.TempDir()}); err == nil {
			t.Errorf("Launch accepted %s", arg)
		}
	}
	if err := checkExtraArgs([]string{"--no-sandbox", "--lang=fr"}); err != nil {
		t.Fatal(err)
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
	if _, err := os.Stat(filepath.Join(profile, "DevToolsActivePort")); err == nil {
		t.Fatal("the browser opened a debugging port")
	}

	// Two clients, each with a connection of its own.
	first, err := inst.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	second, err := inst.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ask := func(conn runtime.CDPConn, request, want string) {
		t.Helper()
		if err := conn.Write(ctx, []byte(request)); err != nil {
			t.Fatal(err)
		}
		reply, err := conn.Read(ctx)
		if err != nil || !strings.Contains(string(reply), want) {
			t.Fatalf("%s: reply = %s (%v)", request, reply, err)
		}
		if strings.Contains(string(reply), "sessionId") {
			t.Fatalf("the client's browser session leaked into %s", reply)
		}
	}
	// Both use the same command ID; each gets its own answer.
	ask(first, `{"id":1,"method":"Browser.getVersion"}`, "protocolVersion")
	ask(second, `{"id":1,"method":"Target.getTargets"}`, "targetInfos")
	if err := first.Write(ctx, []byte("not json")); !errors.Is(err, runtime.ErrInvalidMessage) {
		t.Fatalf("invalid message: %v", err)
	}
	_ = first.Close()
	ask(second, `{"id":2,"method":"Browser.getVersion"}`, "protocolVersion")

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

// A browser's helpers (crash handler, updater) inherit its standard error
// and can outlive it by minutes. The browser's exit must be noticed anyway.
func TestBrowserExitIsNoticedWhileHelpersLinger(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("uses a shell script as the browser")
	}
	script := filepath.Join(t.TempDir(), "browser.sh")
	body := "#!/bin/sh\nsleep 30 &\necho 'profile is on fire' >&2\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, _ := New(runtime.RuntimeConfig{ExecutablePath: script})
	started := time.Now()
	_, err := rt.Launch(context.Background(), runtime.BrowserOptions{ProfileDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "exited before it was ready") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "profile is on fire") {
		t.Fatalf("the browser's output is missing from the error: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("noticing the exit took %s", elapsed)
	}
}
