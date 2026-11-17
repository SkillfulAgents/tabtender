package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/runtime"
)

const (
	defaultMonitorInterval = time.Second
	tombstoneTTL           = 10 * time.Minute
	maxTombstones          = 512
	closeTimeout           = 30 * time.Second
	// maxClientsPerSession bounds the CDP connections one session accepts.
	// Each costs a browser-side CDP session; far more than any real use
	// needs, it only stops a runaway client from piling them up.
	maxClientsPerSession = 64
)

var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// reservedProfileNames are device names that Windows refuses as directory
// names. They are rejected everywhere so profiles stay portable.
var reservedProfileNames = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])$`)

func validProfileName(name string) bool {
	return profileNamePattern.MatchString(name) && !reservedProfileNames.MatchString(name)
}

// Options configures a Manager.
type Options struct {
	// ProfilesDir is the private root for all browser profile directories.
	ProfilesDir string
	// Journal records running sessions for reconciliation.
	Journal *Journal
	// Runtime launches browsers for new sessions.
	Runtime runtime.BrowserRuntime
	Limits  Limits
	Logger  *slog.Logger
	// Processes defaults to the operating system process table.
	Processes ProcessTable
	// MonitorInterval is how often browsers are checked for crashes.
	MonitorInterval time.Duration
	// OnActiveChange, if set, is called with the number of live sessions
	// whenever it changes.
	OnActiveChange func(active int)
}

// Manager owns all sessions.
type Manager struct {
	profilesDir string
	journal     *Journal
	logger      *slog.Logger
	procs       ProcessTable
	onActive    func(int)

	mu         sync.Mutex
	rt         runtime.BrowserRuntime
	limits     Limits
	active     map[string]*entry
	profiles   map[string]string // lowercased profile name -> session ID
	tombstones map[string]Session
	closed     bool

	// lifetime ends when the manager shuts down; browser launches in
	// flight are abandoned with it.
	lifetime    context.Context
	endLifetime context.CancelFunc
	// notifyMu orders OnActiveChange calls, so the last one delivered
	// always carries the current count.
	notifyMu sync.Mutex

	stopMonitor chan struct{}
	monitorDone chan struct{}
}

// entry is the mutable state of a live session, guarded by Manager.mu.
type entry struct {
	session    Session
	rt         runtime.BrowserRuntime
	instance   runtime.BrowserInstance
	profileDir string
	ephemeral  bool
	timer      *time.Timer
	// abort is set when termination is requested while the browser is
	// still launching; Create honors it once the launch returns.
	abort EndReason
	// done is closed once the session is fully terminated and cleaned up.
	done chan struct{}
}

// NewManager returns a Manager and starts its crash monitor.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Processes == nil {
		opts.Processes = OSProcesses{}
	}
	if opts.MonitorInterval <= 0 {
		opts.MonitorInterval = defaultMonitorInterval
	}
	m := &Manager{
		profilesDir: opts.ProfilesDir,
		journal:     opts.Journal,
		logger:      opts.Logger,
		procs:       opts.Processes,
		onActive:    opts.OnActiveChange,
		rt:          opts.Runtime,
		limits:      opts.Limits,
		active:      map[string]*entry{},
		profiles:    map[string]string{},
		tombstones:  map[string]Session{},
		stopMonitor: make(chan struct{}),
		monitorDone: make(chan struct{}),
	}
	m.lifetime, m.endLifetime = context.WithCancel(context.Background())
	go m.monitor(opts.MonitorInterval)
	return m
}

func (m *Manager) ephemeralRoot() string { return filepath.Join(m.profilesDir, "ephemeral") }
func (m *Manager) namedRoot() string     { return filepath.Join(m.profilesDir, "named") }

// SetLimits replaces the session policies; running sessions are unaffected.
func (m *Manager) SetLimits(l Limits) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limits = l
}

// Limits returns the current session policies.
func (m *Manager) Limits() Limits {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limits
}

// SetRuntime replaces the runtime used for new sessions. Sessions already
// running stay with the runtime that launched them.
func (m *Manager) SetRuntime(rt runtime.BrowserRuntime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rt = rt
}

// Runtime returns the runtime used for new sessions.
func (m *Manager) Runtime() runtime.BrowserRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rt
}

func newSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	return "sess_" + strings.ToLower(enc), nil
}

// newSessionToken returns a URL-safe token carrying 256 bits of entropy.
func newSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// TokenMatches reports whether token is the CDP token of session id. The
// comparison is constant-time. Recently ended sessions still match, so that
// the holder of a once-valid URL can be told the session is over instead of
// being treated as a stranger; an ended session cannot be connected to
// either way.
func (m *Manager) TokenMatches(id, token string) bool {
	if token == "" {
		return false
	}
	m.mu.Lock()
	var want string
	if e, ok := m.active[id]; ok {
		want = e.session.Token
	} else if t, ok := m.tombstones[id]; ok {
		want = t.Token
	}
	m.mu.Unlock()
	if want == "" {
		return false
	}
	presented, expected := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}

// resolve validates a request against the limits.
func (l Limits) resolve(req CreateRequest) (ttl time.Duration, headless bool, err error) {
	ttl = l.DefaultTTL
	if req.TTLSeconds != nil {
		maxSeconds := int(l.MaxTTL / time.Second)
		if *req.TTLSeconds < 1 {
			return 0, false, newError(KindValidation, "ttlSeconds must be at least 1, got %d", *req.TTLSeconds)
		}
		if *req.TTLSeconds > maxSeconds {
			return 0, false, newError(KindValidation,
				"ttlSeconds %d exceeds the maximum session TTL of %d seconds", *req.TTLSeconds, maxSeconds)
		}
		ttl = time.Duration(*req.TTLSeconds) * time.Second
	}
	headless = l.DefaultHeadless || !l.HeadedSupported
	if req.Headless != nil {
		if !*req.Headless && !l.HeadedSupported {
			return 0, false, newError(KindValidation,
				"headed browsers are not supported in this environment; request \"headless\": true")
		}
		headless = *req.Headless
	}
	if req.Profile != "" && !validProfileName(req.Profile) {
		return 0, false, newError(KindValidation,
			"profile must be 1-64 characters of letters, digits, '-' or '_' and start with a letter or digit")
	}
	return ttl, headless, nil
}

// Create launches a new browser session.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (Session, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Session{}, newError(KindShuttingDown, "the service is shutting down")
	}
	ttl, headless, err := m.limits.resolve(req)
	if err != nil {
		m.mu.Unlock()
		return Session{}, err
	}
	if len(m.active) >= m.limits.MaxConcurrent {
		limit := m.limits.MaxConcurrent
		m.mu.Unlock()
		return Session{}, newError(KindCapacity, "the maximum of %d concurrent sessions is reached", limit)
	}
	profileKey := strings.ToLower(req.Profile)
	if req.Profile != "" {
		if holder, locked := m.profiles[profileKey]; locked {
			m.mu.Unlock()
			return Session{}, newError(KindProfileInUse, "profile %q is in use by session %s", req.Profile, holder)
		}
	}
	id, err := newSessionID()
	if err != nil {
		m.mu.Unlock()
		return Session{}, fmt.Errorf("generate session id: %w", err)
	}
	token, err := newSessionToken()
	if err != nil {
		m.mu.Unlock()
		return Session{}, fmt.Errorf("generate session token: %w", err)
	}
	e := &entry{
		session: Session{
			ID:        id,
			Token:     token,
			Status:    StatusStarting,
			Profile:   req.Profile,
			Headless:  headless,
			CreatedAt: time.Now().UTC(),
		},
		rt:        m.rt,
		ephemeral: req.Profile == "",
		done:      make(chan struct{}),
	}
	if e.ephemeral {
		e.profileDir = filepath.Join(m.ephemeralRoot(), id)
	} else {
		e.profileDir = filepath.Join(m.namedRoot(), req.Profile)
		m.profiles[profileKey] = id
	}
	// Until the browser is up the expiry is provisional.
	e.session.ExpiresAt = e.session.CreatedAt.Add(ttl)
	m.active[id] = e
	m.mu.Unlock()
	m.notifyActive()

	launchCtx, cancelLaunch := context.WithCancel(ctx)
	stopWatching := context.AfterFunc(m.lifetime, cancelLaunch)
	instance, launchErr := e.rt.Launch(launchCtx, runtime.BrowserOptions{Headless: headless, ProfileDir: e.profileDir})
	stopWatching()
	cancelLaunch()

	m.mu.Lock()
	// A caller that gave up while the browser was starting will never learn
	// the session's ID; keeping the session would only hold a slot.
	abandoned := launchErr == nil && e.abort == "" && ctx.Err() != nil
	if launchErr == nil && e.abort == "" && !abandoned {
		e.instance = instance
		e.session.PID = instance.PID()
		e.session.Status = StatusRunning
		e.session.ExpiresAt = time.Now().UTC().Add(ttl)
		e.timer = time.AfterFunc(ttl, func() { m.expire(id) })
		m.persistLocked()
		snapshot := e.session
		m.mu.Unlock()
		m.logger.Info("session started", "session", id, "pid", snapshot.PID,
			"headless", headless, "profile", req.Profile, "ttl", ttl.String())
		return snapshot, nil
	}
	reason := e.abort
	m.mu.Unlock()

	// The launch failed or the session was terminated while starting.
	if launchErr == nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		if err := e.rt.Close(closeCtx, instance); err != nil {
			m.logger.Warn("failed to close browser of aborted session", "session", id, "error", err)
		}
		cancel()
	}
	m.finish(e, reason, launchErr == nil && !abandoned)
	switch {
	case abandoned:
		m.logger.Info("session discarded: the client gave up while it was starting", "session", id)
		return Session{}, ctx.Err()
	case launchErr != nil && m.lifetime.Err() != nil:
		return Session{}, newError(KindShuttingDown, "the service is shutting down")
	case launchErr != nil:
		m.logger.Error("browser launch failed", "session", id, "error", launchErr)
		return Session{}, &Error{Kind: KindLaunchFailed, Message: "failed to launch browser: " + launchErr.Error(), Err: launchErr}
	}
	return Session{}, newError(KindShuttingDown, "session was terminated while starting")
}

// finish releases everything a session holds and publishes its tombstone.
func (m *Manager) finish(e *entry, reason EndReason, tombstone bool) {
	if e.ephemeral {
		if err := removeAllRetry(e.profileDir); err != nil {
			m.logger.Warn("failed to remove ephemeral profile", "session", e.session.ID, "error", err)
		}
	}
	m.mu.Lock()
	delete(m.active, e.session.ID)
	if e.session.Profile != "" {
		delete(m.profiles, strings.ToLower(e.session.Profile))
	}
	e.session.Status = StatusTerminated
	e.session.EndedAt = time.Now().UTC()
	e.session.EndReason = reason
	e.session.Clients = 0
	if tombstone {
		m.addTombstoneLocked(e.session)
	}
	m.persistLocked()
	m.mu.Unlock()
	close(e.done)
	m.notifyActive()
}

func (m *Manager) addTombstoneLocked(s Session) {
	cutoff := time.Now().Add(-tombstoneTTL)
	for id, t := range m.tombstones {
		if t.EndedAt.Before(cutoff) {
			delete(m.tombstones, id)
		}
	}
	if len(m.tombstones) >= maxTombstones {
		oldestID, oldest := "", time.Time{}
		for id, t := range m.tombstones {
			if oldestID == "" || t.EndedAt.Before(oldest) {
				oldestID, oldest = id, t.EndedAt
			}
		}
		delete(m.tombstones, oldestID)
	}
	m.tombstones[s.ID] = s
}

// persistLocked rewrites the journal with all sessions that own a browser.
func (m *Manager) persistLocked() {
	if m.journal == nil {
		return
	}
	records := make([]Record, 0, len(m.active))
	for _, e := range m.active {
		if e.instance == nil {
			continue
		}
		records = append(records, Record{
			ID:         e.session.ID,
			PID:        e.session.PID,
			ProfileDir: e.profileDir,
			Ephemeral:  e.ephemeral,
			CreatedAt:  e.session.CreatedAt,
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	if err := m.journal.Save(records); err != nil {
		m.logger.Error("failed to persist session journal", "error", err)
	}
}

func (m *Manager) notifyActive() {
	if m.onActive == nil {
		return
	}
	m.notifyMu.Lock()
	defer m.notifyMu.Unlock()
	m.mu.Lock()
	n := len(m.active)
	m.mu.Unlock()
	m.onActive(n)
}

func (m *Manager) expire(id string) {
	if err := m.terminate(context.Background(), id, ReasonExpired); err == nil {
		m.logger.Info("session expired", "session", id)
	}
}

// Terminate ends a session on behalf of a client.
func (m *Manager) Terminate(ctx context.Context, id string) error {
	return m.terminate(ctx, id, ReasonDeleted)
}

// terminate stops the session's browser and reclaims its resources. It
// returns once cleanup is complete, even if another caller started it.
func (m *Manager) terminate(ctx context.Context, id string, reason EndReason) error {
	m.mu.Lock()
	e, ok := m.active[id]
	if !ok {
		m.mu.Unlock()
		return newError(KindNotFound, "session %s not found", id)
	}
	switch e.session.Status {
	case StatusStarting:
		if e.abort == "" {
			e.abort = reason
		}
		m.mu.Unlock()
		return waitDone(ctx, e.done)
	case StatusTerminating:
		m.mu.Unlock()
		return waitDone(ctx, e.done)
	}
	e.session.Status = StatusTerminating
	if e.timer != nil {
		e.timer.Stop()
	}
	m.mu.Unlock()

	// Cleanup must complete even if the requesting client goes away.
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if err := e.rt.Close(closeCtx, e.instance); err != nil {
		m.logger.Warn("browser did not close cleanly", "session", id, "error", err)
	}
	m.finish(e, reason, true)
	m.logger.Info("session terminated", "session", id, "reason", string(reason))
	return nil
}

func waitDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Get returns a live session, or a recently ended one.
func (m *Manager) Get(id string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.active[id]; ok {
		return e.session, true
	}
	if t, ok := m.tombstones[id]; ok && time.Since(t.EndedAt) < tombstoneTTL {
		return t, true
	}
	return Session{}, false
}

// List returns all live sessions, oldest first.
func (m *Manager) List() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]Session, 0, len(m.active))
	for _, e := range m.active {
		list = append(list, e.session)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].ID < list[j].ID
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	return list
}

// ActiveCount returns the number of live sessions.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Attachment is a client's hold on a session's CDP endpoint.
type Attachment struct {
	// Connect opens a CDP connection to the session's browser.
	Connect func(ctx context.Context) (runtime.CDPConn, error)
	// Done is closed when the session has ended.
	Done    <-chan struct{}
	release func()
}

// Release records that the client disconnected. The session keeps running.
func (a *Attachment) Release() { a.release() }

// Attach registers a CDP client on a running session.
func (m *Manager) Attach(id string) (*Attachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.active[id]
	if !ok {
		if _, ended := m.tombstones[id]; ended {
			return nil, newError(KindNotRunning, "session %s has ended", id)
		}
		return nil, newError(KindNotFound, "session %s not found", id)
	}
	if e.session.Status != StatusRunning {
		return nil, newError(KindNotRunning, "session %s is %s", id, e.session.Status)
	}
	if e.session.Clients >= maxClientsPerSession {
		return nil, newError(KindCapacity, "session %s already has %d CDP clients connected", id, maxClientsPerSession)
	}
	e.session.Clients++
	var once sync.Once
	return &Attachment{
		Connect: e.instance.Connect,
		Done:    e.done,
		release: func() {
			once.Do(func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				if e.session.Clients > 0 {
					e.session.Clients--
				}
			})
		},
	}, nil
}

// monitor detects browsers that exited on their own and reclaims them.
func (m *Manager) monitor(interval time.Duration) {
	defer close(m.monitorDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopMonitor:
			return
		case <-ticker.C:
		}
		// Expiry timers run on the monotonic clock, which stands still
		// while the machine sleeps; the wall clock catches sessions that
		// outlived their TTL that way.
		now := time.Now()
		m.mu.Lock()
		var crashed, overdue []string
		for id, e := range m.active {
			if e.session.Status != StatusRunning {
				continue
			}
			switch {
			case !e.instance.IsRunning():
				crashed = append(crashed, id)
			case now.After(e.session.ExpiresAt):
				overdue = append(overdue, id)
			}
		}
		m.mu.Unlock()
		for _, id := range crashed {
			m.logger.Warn("browser exited unexpectedly", "session", id)
			go func() { _ = m.terminate(context.Background(), id, ReasonCrashed) }()
		}
		for _, id := range overdue {
			go m.expire(id)
		}
	}
}

// Shutdown terminates every session and stops the manager. New sessions are
// rejected from the moment it is called.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.endLifetime()
	ids := make([]string, 0, len(m.active))
	for id := range m.active {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	close(m.stopMonitor)
	<-m.monitorDone

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.terminate(ctx, id, ReasonShutdown); err != nil {
				var se *Error
				if !errors.As(err, &se) || se.Kind != KindNotFound {
					m.logger.Warn("session did not terminate during shutdown", "session", id, "error", err)
				}
			}
		}()
	}
	wg.Wait()
}

// Profiles lists the named persistent profiles on disk.
func (m *Manager) Profiles() ([]ProfileInfo, error) {
	entries, err := os.ReadDir(m.namedRoot())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	infos := make([]ProfileInfo, 0, len(entries))
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		holder, inUse := m.profiles[strings.ToLower(de.Name())]
		infos = append(infos, ProfileInfo{Name: de.Name(), InUse: inUse, SessionID: holder})
	}
	return infos, nil
}

// DeleteProfile removes a named profile that is not in use.
func (m *Manager) DeleteProfile(name string) error {
	if !validProfileName(name) {
		return newError(KindValidation, "invalid profile name")
	}
	m.mu.Lock()
	if holder, locked := m.profiles[strings.ToLower(name)]; locked {
		m.mu.Unlock()
		return newError(KindProfileInUse, "profile %q is in use by session %s", name, holder)
	}
	// Hold the profile lock while deleting so no session can start on it.
	m.profiles[strings.ToLower(name)] = "(deleting)"
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.profiles, strings.ToLower(name))
		m.mu.Unlock()
	}()
	dir := filepath.Join(m.namedRoot(), name)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return newError(KindNotFound, "profile %q not found", name)
	}
	return removeAllRetry(dir)
}
