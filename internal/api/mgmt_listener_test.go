//go:build darwin || linux

package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestManagementSocketIsPrivateAndPeerChecked(t *testing.T) {
	// Unix socket paths are length-limited, so avoid t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "tt-mgmt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "sub", "mgmt.sock")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A stale socket file from a crashed daemon must not block startup.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := ListenManagement(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // test

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 0600", perm)
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})}
	go srv.Serve(ln)  //nolint:errcheck // test
	defer srv.Close() //nolint:errcheck // test

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	resp, err := client.Get("http://tabtender/anything")
	if err != nil {
		t.Fatalf("same-user connection rejected: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if body, _ := io.ReadAll(resp.Body); string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
}

func TestPeerUIDReportsCallingUser(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tt-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "peer.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // test
	go func() {
		if c, err := net.Dial("unix", path); err == nil {
			defer c.Close() //nolint:errcheck // test
			_, _ = io.ReadAll(c)
		}
	}()
	conn, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // test
	uid, err := peerUID(conn)
	if err != nil {
		t.Fatal(err)
	}
	if uid != uint32(os.Getuid()) {
		t.Fatalf("peer uid = %d, want %d", uid, os.Getuid())
	}
}
