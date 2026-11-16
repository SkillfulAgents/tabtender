//go:build !darwin && !linux

package api

import (
	"errors"
	"log/slog"
	"net"
)

// ManagementSupported reports whether this platform has a local management
// channel. On Windows, management happens through the configuration file,
// environment variables, and the authenticated HTTP API.
const ManagementSupported = false

// ListenManagement is unavailable on this platform.
func ListenManagement(string, *slog.Logger) (net.Listener, error) {
	return nil, errors.New("the local management channel is not supported on this platform")
}
