package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupWritesToAllSinks(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "tabtenderd.log")
	var stderr bytes.Buffer
	logger, buffer, closer, err := Setup(logFile, slog.LevelInfo, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("hidden")
	logger.Info("hello", "session", "sess_1")
	logger.With("component", "tunnel").Error("boom", "attempt", 3)
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	file, _ := os.ReadFile(logFile)
	for name, out := range map[string]string{"stderr": stderr.String(), "file": string(file)} {
		if !strings.Contains(out, "hello") || !strings.Contains(out, "boom") || strings.Contains(out, "hidden") {
			t.Errorf("%s sink content unexpected:\n%s", name, out)
		}
	}

	entries := buffer.Recent(10, slog.LevelInfo)
	if len(entries) != 2 {
		t.Fatalf("buffered %d entries, want 2", len(entries))
	}
	if entries[0].Message != "hello" || entries[0].Attrs["session"] != "sess_1" {
		t.Fatalf("unexpected first entry: %+v", entries[0])
	}
	if entries[1].Level != "ERROR" || entries[1].Attrs["component"] != "tunnel" || entries[1].Attrs["attempt"] != "3" {
		t.Fatalf("unexpected second entry: %+v", entries[1])
	}
	if errs := buffer.Recent(10, slog.LevelWarn); len(errs) != 1 || errs[0].Message != "boom" {
		t.Fatalf("level filter returned %+v", errs)
	}
}

func TestBufferKeepsMostRecentEntries(t *testing.T) {
	buffer := NewBuffer(3)
	logger := slog.New(&bufferHandler{buf: buffer})
	for _, msg := range []string{"a", "b", "c", "d", "e"} {
		logger.Info(msg)
	}
	var got []string
	for _, e := range buffer.Recent(0, slog.LevelInfo) {
		got = append(got, e.Message)
	}
	if strings.Join(got, "") != "cde" {
		t.Fatalf("buffer = %v, want [c d e]", got)
	}
	if limited := buffer.Recent(2, slog.LevelInfo); len(limited) != 2 || limited[1].Message != "e" {
		t.Fatalf("limit returned %+v", limited)
	}
}

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotating.log")
	rf, err := openRotating(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close() //nolint:errcheck // test cleanup
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for range 11 {
		if _, err := rf.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("no rotated generation: %v", err)
	}
	if current.Size() != 1<<20 || previous.Size() != 10<<20 {
		t.Fatalf("sizes after rotation: current=%d previous=%d", current.Size(), previous.Size())
	}
}
