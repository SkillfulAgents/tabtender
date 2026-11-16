// Package logging configures structured logging for tabtenderd: a text log
// on stderr, a size-rotated log file, and an in-memory buffer of recent
// entries served to the management UI.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxLogFileSize = 10 << 20
	bufferCapacity = 1000
)

// Entry is one buffered log record.
type Entry struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Buffer is a fixed-size ring of recent log entries.
type Buffer struct {
	mu      sync.Mutex
	entries []Entry
	next    int
	full    bool
}

// NewBuffer returns a buffer holding up to capacity entries.
func NewBuffer(capacity int) *Buffer { return &Buffer{entries: make([]Entry, capacity)} }

func (b *Buffer) add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[b.next] = e
	b.next = (b.next + 1) % len(b.entries)
	if b.next == 0 {
		b.full = true
	}
}

// Recent returns up to limit entries at or above minLevel, oldest first.
func (b *Buffer) Recent(limit int, minLevel slog.Level) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ordered []Entry
	if b.full {
		ordered = append(ordered, b.entries[b.next:]...)
	}
	ordered = append(ordered, b.entries[:b.next]...)
	out := make([]Entry, 0, len(ordered))
	for _, e := range ordered {
		var lvl slog.Level
		if err := lvl.UnmarshalText([]byte(e.Level)); err == nil && lvl >= minLevel {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// bufferHandler is a slog.Handler that records entries into a Buffer.
type bufferHandler struct {
	buf   *Buffer
	attrs []slog.Attr
}

func (h *bufferHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *bufferHandler) Handle(_ context.Context, r slog.Record) error {
	e := Entry{Time: r.Time, Level: r.Level.String(), Message: r.Message}
	add := func(a slog.Attr) bool {
		if e.Attrs == nil {
			e.Attrs = map[string]string{}
		}
		e.Attrs[a.Key] = fmt.Sprint(a.Value.Resolve().Any())
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	h.buf.add(e)
	return nil
}

func (h *bufferHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &bufferHandler{buf: h.buf, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *bufferHandler) WithGroup(string) slog.Handler { return h }

// fanout dispatches records to several handlers.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// rotatingFile is an io.Writer that keeps one previous generation once the
// file exceeds maxLogFileSize.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func openRotating(path string) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	rf := &rotatingFile{path: path}
	if err := rf.open(); err != nil {
		return nil, err
	}
	return rf, nil
}

func (rf *rotatingFile) open() error {
	f, err := os.OpenFile(rf.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	rf.file, rf.size = f, info.Size()
	return nil
}

func (rf *rotatingFile) Write(p []byte) (int, error) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.size+int64(len(p)) > maxLogFileSize {
		_ = rf.file.Close()
		_ = os.Remove(rf.path + ".1")
		_ = os.Rename(rf.path, rf.path+".1")
		if err := rf.open(); err != nil {
			return 0, err
		}
	}
	n, err := rf.file.Write(p)
	rf.size += int64(n)
	return n, err
}

func (rf *rotatingFile) Close() error {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.file.Close()
}

// Setup builds the daemon logger. When logFile is non-empty, records are
// also appended to it. The returned closer releases the file.
func Setup(logFile string, level slog.Level, stderr io.Writer) (*slog.Logger, *Buffer, io.Closer, error) {
	buf := NewBuffer(bufferCapacity)
	opts := &slog.HandlerOptions{Level: level}
	handlers := fanout{&bufferHandler{buf: buf}}
	if stderr != nil {
		handlers = append(handlers, slog.NewTextHandler(stderr, opts))
	}
	var closer io.Closer = io.NopCloser(nil)
	if logFile != "" {
		rf, err := openRotating(logFile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open log file: %w", err)
		}
		handlers = append(handlers, slog.NewTextHandler(rf, opts))
		closer = rf
	}
	return slog.New(handlers), buf, closer, nil
}
