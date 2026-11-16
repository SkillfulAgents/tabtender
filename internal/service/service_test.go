package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManualStartMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if ManualStartRequested(dir) {
		t.Fatal("no request was made yet")
	}
	if err := RequestManualStart(dir); err != nil {
		t.Fatal(err)
	}
	if !ManualStartRequested(dir) {
		t.Fatal("the request was not recorded")
	}

	// A marker from an earlier boot must not carry over.
	if err := os.WriteFile(filepath.Join(dir, manualStartFile), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ManualStartRequested(dir) {
		t.Fatal("a marker from a previous boot was honored")
	}

	if err := RequestManualStart(dir); err != nil {
		t.Fatal(err)
	}
	ClearManualStart(dir)
	if ManualStartRequested(dir) {
		t.Fatal("the marker survived ClearManualStart")
	}
	ClearManualStart(dir) // clearing twice is harmless
}

func TestNotAWindowsServiceUnderTest(t *testing.T) {
	if IsWindowsService() {
		t.Fatal("tests do not run under the Service Control Manager")
	}
}
