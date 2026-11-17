package power

import (
	"runtime"
	"testing"
)

// supported reports whether this platform can prevent sleep at all.
var supported = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

func TestInhibitorHoldsOnlyWhileEnabledAndActive(t *testing.T) {
	i := NewInhibitor(Policy{WhileActive: true})
	defer i.Close()
	if i.Held() {
		t.Fatal("held without active sessions")
	}
	i.SetActive(true)
	if i.Held() != supported {
		t.Fatalf("held = %v with active sessions (supported=%v)", i.Held(), supported)
	}
	i.SetActive(true) // idempotent
	i.SetPolicy(Policy{})
	if i.Held() {
		t.Fatal("still held after the feature was disabled")
	}
	i.SetPolicy(Policy{WhileActive: true})
	if i.Held() != supported {
		t.Fatal("not re-acquired after re-enabling with active sessions")
	}
	i.SetActive(false)
	if i.Held() {
		t.Fatal("still held without active sessions")
	}
	i.SetActive(true)
	i.Close()
	if i.Held() {
		t.Fatal("still held after Close")
	}
}

func TestDisabledInhibitorNeverHolds(t *testing.T) {
	i := NewInhibitor(Policy{})
	i.SetActive(true)
	if i.Held() {
		t.Fatal("a disabled inhibitor must not prevent sleep")
	}
}

func TestAlwaysHoldsWithoutSessions(t *testing.T) {
	i := NewInhibitor(Policy{Always: true})
	if i.Held() != supported {
		t.Fatalf("held = %v from the start (supported=%v)", i.Held(), supported)
	}
	i.SetActive(true)
	i.SetActive(false)
	if i.Held() != supported {
		t.Fatal("released when the last session ended")
	}
	i.SetPolicy(Policy{WhileActive: true})
	if i.Held() {
		t.Fatal("still held after falling back to sessions only")
	}
	i.SetPolicy(Policy{Always: true})
	i.Close()
	if i.Held() {
		t.Fatal("still held after Close")
	}
	i.SetActive(true)
	if i.Held() {
		t.Fatal("re-acquired after Close")
	}
}
