// Package sessions implements the browser session lifecycle: creation under
// concurrency limits, TTL enforcement, crash detection, termination with
// resource cleanup, and startup reconciliation.
//
// The package depends only on the runtime.BrowserRuntime abstraction, never
// on a concrete browser.
package sessions

import (
	"fmt"
	"time"
)

// Status is the lifecycle state of a session.
type Status string

// Session lifecycle states.
const (
	StatusStarting    Status = "starting"
	StatusRunning     Status = "running"
	StatusTerminating Status = "terminating"
	StatusTerminated  Status = "terminated"
)

// EndReason explains why a session ended.
type EndReason string

// Reasons a session can end.
const (
	ReasonDeleted  EndReason = "deleted"
	ReasonExpired  EndReason = "expired"
	ReasonCrashed  EndReason = "crashed"
	ReasonShutdown EndReason = "shutdown"
)

// Session is an immutable snapshot of a session's state.
type Session struct {
	ID        string
	Status    Status
	Profile   string
	Headless  bool
	PID       int
	Clients   int
	CreatedAt time.Time
	ExpiresAt time.Time
	EndedAt   time.Time
	EndReason EndReason
}

// CreateRequest carries the caller-supplied session parameters. Nil fields
// fall back to configured defaults.
type CreateRequest struct {
	TTLSeconds *int
	Headless   *bool
	Profile    string
}

// Limits are the runtime-adjustable session policies.
type Limits struct {
	DefaultTTL      time.Duration
	MaxTTL          time.Duration
	MaxConcurrent   int
	DefaultHeadless bool
	// HeadedSupported is false where no interactive desktop exists (for
	// example a Windows Service in session 0); headed requests are rejected.
	HeadedSupported bool
}

// ErrorKind classifies session errors for the API layer.
type ErrorKind string

// Session error kinds.
const (
	KindValidation   ErrorKind = "validation_error"
	KindCapacity     ErrorKind = "capacity_exceeded"
	KindProfileInUse ErrorKind = "profile_in_use"
	KindNotFound     ErrorKind = "session_not_found"
	KindNotRunning   ErrorKind = "session_not_running"
	KindLaunchFailed ErrorKind = "browser_launch_failed"
	KindShuttingDown ErrorKind = "shutting_down"
)

// Error is a classified session error.
type Error struct {
	Kind    ErrorKind
	Message string
	Err     error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

func newError(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// ProfileInfo describes a named persistent profile.
type ProfileInfo struct {
	Name      string
	InUse     bool
	SessionID string
}
