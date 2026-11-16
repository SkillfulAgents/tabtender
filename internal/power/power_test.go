package power

import (
	"runtime"
	"testing"
)

func TestInhibitorHoldsOnlyWhileEnabledAndActive(t *testing.T) {
	supported := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	i := NewInhibitor(true)
	defer i.Close()
	if i.Held() {
		t.Fatal("held without active sessions")
	}
	i.SetActive(true)
	if i.Held() != supported {
		t.Fatalf("held = %v with active sessions (supported=%v)", i.Held(), supported)
	}
	i.SetActive(true) // idempotent
	i.SetEnabled(false)
	if i.Held() {
		t.Fatal("still held after the feature was disabled")
	}
	i.SetEnabled(true)
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
	i := NewInhibitor(false)
	i.SetActive(true)
	if i.Held() {
		t.Fatal("a disabled inhibitor must not prevent sleep")
	}
}
