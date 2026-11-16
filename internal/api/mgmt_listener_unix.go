//go:build darwin || linux

package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
)

// ManagementSupported reports whether this platform has a local management
// channel.
const ManagementSupported = true

// ListenManagement opens the management Unix socket. Access is restricted
// twice: the socket file is owner-only, and every accepted connection must
// come from a process running as the same user as the daemon (verified with
// kernel-provided peer credentials).
func ListenManagement(path string, logger *slog.Logger) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// A leftover socket file from a previous run would block bind. The
	// daemon holds an exclusive instance lock, so nobody else is using it.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale management socket: %w", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on management socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &peerCheckedListener{UnixListener: ln, uid: uint32(os.Getuid()), logger: logger}, nil
}

type peerCheckedListener struct {
	*net.UnixListener
	uid    uint32
	logger *slog.Logger
}

func (l *peerCheckedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(conn)
		if err == nil && uid == l.uid {
			return conn, nil
		}
		l.logger.Warn("rejected management connection from another user", "peerUid", uid, "error", err)
		_ = conn.Close()
	}
}
