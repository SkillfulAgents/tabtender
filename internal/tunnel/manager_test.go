package tunnel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// scriptedProvider fails its first failures starts, then succeeds.
type scriptedProvider struct {
	mu       sync.Mutex
	url      string
	failures int
	starts   int
	stops    int
	running  bool
}

func (p *scriptedProvider) Start(context.Context, int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
	if p.starts <= p.failures {
		return "", errors.New("network unreachable")
	}
	p.running = true
	return p.url, nil
}

func (p *scriptedProvider) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops++
	p.running = false
	return nil
}

func (p *scriptedProvider) Status() TunnelStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return TunnelStatus{State: StateRunning, URL: p.url}
	}
	return TunnelStatus{State: StateStopped}
}

func (p *scriptedProvider) counts() (starts, stops int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts, p.stops
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

func newTestManager(t *testing.T, providers map[string]*scriptedProvider) *Manager {
	t.Helper()
	registry := NewRegistry()
	for kind, p := range providers {
		registry.Register(kind, func(TunnelConfig) (TunnelProvider, error) { return p, nil })
	}
	m := NewManager(registry, 3000, nil)
	m.SetRetryDelay(20 * time.Millisecond)
	t.Cleanup(m.Stop)
	return m
}

func TestManagerDefaultsToLocalOnly(t *testing.T) {
	m := newTestManager(t, nil)
	if got := m.Status(); got != (TunnelStatus{Provider: KindNone, State: StateDisabled}) {
		t.Fatalf("status = %+v", got)
	}
	if m.PublicURL() != "" {
		t.Fatal("local-only access must not report a public URL")
	}
}

func TestManagerSwitchesProvidersAtRuntime(t *testing.T) {
	a := &scriptedProvider{url: "https://a.example"}
	b := &scriptedProvider{url: "https://b.example"}
	m := newTestManager(t, map[string]*scriptedProvider{"a": a, "b": b})

	if err := m.Apply("a", TunnelConfig{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "provider a to run", func() bool { return m.PublicURL() == "https://a.example" })
	if got := m.Status(); got.Provider != "a" || got.State != StateRunning {
		t.Fatalf("status = %+v", got)
	}

	if err := m.Apply("b", TunnelConfig{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "provider b to run", func() bool { return m.PublicURL() == "https://b.example" })
	if _, stops := a.counts(); stops != 1 {
		t.Fatalf("the replaced provider was stopped %d times, want 1", stops)
	}

	if err := m.Apply(KindNone, TunnelConfig{}); err != nil {
		t.Fatal(err)
	}
	if m.PublicURL() != "" || m.Status().State != StateDisabled {
		t.Fatalf("status after disabling = %+v", m.Status())
	}
	if _, stops := b.counts(); stops != 1 {
		t.Fatal("provider b was not stopped")
	}
}

func TestManagerRetriesFailedStarts(t *testing.T) {
	p := &scriptedProvider{url: "https://late.example", failures: 2}
	m := newTestManager(t, map[string]*scriptedProvider{"flaky": p})
	if err := m.Apply("flaky", TunnelConfig{}); err != nil {
		t.Fatal(err)
	}
	sawError := false
	eventually(t, "the tunnel to come up after retries", func() bool {
		status := m.Status()
		if status.State == StateError && status.Error == "network unreachable" {
			sawError = true
		}
		return status.State == StateRunning
	})
	if starts, _ := p.counts(); starts != 3 {
		t.Fatalf("starts = %d, want 3", starts)
	}
	if !sawError {
		t.Log("error state was not observed (timing), but retries succeeded")
	}
	if m.Status().Error != "" {
		t.Fatalf("stale error after recovery: %+v", m.Status())
	}
}

func TestManagerRejectsUnknownProviderAndKeepsCurrent(t *testing.T) {
	a := &scriptedProvider{url: "https://a.example"}
	m := newTestManager(t, map[string]*scriptedProvider{"a": a})
	if err := m.Apply("a", TunnelConfig{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "provider a to run", func() bool { return m.PublicURL() != "" })
	if err := m.Apply("ngrok", TunnelConfig{}); err == nil {
		t.Fatal("expected an error for an unregistered provider")
	}
	if m.PublicURL() != "https://a.example" {
		t.Fatal("a failed switch must leave the current tunnel running")
	}
}

func TestManagerStopDuringRetryBackoff(t *testing.T) {
	p := &scriptedProvider{failures: 1 << 30}
	m := newTestManager(t, map[string]*scriptedProvider{"down": p})
	m.SetRetryDelay(time.Hour)
	if err := m.Apply("down", TunnelConfig{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the error state", func() bool { return m.Status().State == StateError })
	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked on the retry backoff")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register("zeta", func(TunnelConfig) (TunnelProvider, error) { return noneProvider{}, nil })
	if kinds := r.Kinds(); len(kinds) != 2 || kinds[0] != KindNone || kinds[1] != "zeta" {
		t.Fatalf("kinds = %v", kinds)
	}
	if _, err := r.Create("missing", TunnelConfig{}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestManagerBacksOffBetweenFailedStarts(t *testing.T) {
	p := &scriptedProvider{failures: 1 << 30}
	m := newTestManager(t, map[string]*scriptedProvider{"flaky": p})
	m.SetRetryDelay(10 * time.Millisecond)
	if err := m.Apply("flaky", TunnelConfig{}); err != nil {
		t.Fatal(err)
	}
	// Doubling pauses of 10, 20, 40, 80 and 160 ms allow five attempts in
	// 250 ms; a fixed 10 ms pause would allow about twenty-five.
	time.Sleep(250 * time.Millisecond)
	if starts, _ := p.counts(); starts < 3 || starts > 7 {
		t.Fatalf("%d start attempts in 250ms, want a handful", starts)
	}
}

func TestManagerRefusesProvidersAfterClose(t *testing.T) {
	p := &scriptedProvider{url: "https://a.example"}
	m := newTestManager(t, map[string]*scriptedProvider{"a": p})
	if err := m.Apply("a", TunnelConfig{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the provider to run", func() bool { return m.PublicURL() != "" })
	m.Close()
	if _, stops := p.counts(); stops != 1 || m.PublicURL() != "" {
		t.Fatalf("the provider was not stopped: stops=%d", stops)
	}
	// A configuration change racing with shutdown must not start a tunnel
	// that nothing would ever stop.
	if err := m.Apply("a", TunnelConfig{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Apply after Close: %v", err)
	}
	if starts, _ := p.counts(); starts != 1 {
		t.Fatalf("a provider was started after Close (%d starts)", starts)
	}
}
