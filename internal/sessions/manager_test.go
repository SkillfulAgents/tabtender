package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/runtime"
	"github.com/SkillfulAgents/tabtender/internal/runtime/platform"
	"github.com/SkillfulAgents/tabtender/internal/runtime/runtimetest"
)

func ptr[T any](v T) *T { return &v }

type fixture struct {
	*Manager
	rt      *runtimetest.Runtime
	dir     string
	journal *Journal
	procs   *fakeProcs
}

func defaultLimits() Limits {
	return Limits{
		DefaultTTL:      time.Hour,
		MaxTTL:          4 * time.Hour,
		MaxConcurrent:   8,
		DefaultHeadless: false,
		HeadedSupported: true,
	}
}

func newFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		rt:      runtimetest.New(),
		dir:     dir,
		journal: NewJournal(filepath.Join(dir, "state", "sessions.json")),
		procs:   &fakeProcs{},
	}
	opts := Options{
		ProfilesDir:     filepath.Join(dir, "profiles"),
		Journal:         f.journal,
		Runtime:         f.rt,
		Limits:          defaultLimits(),
		Processes:       f.procs,
		MonitorInterval: 10 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	f.Manager = NewManager(opts)
	t.Cleanup(func() { f.Shutdown(context.Background()) })
	return f
}

func (f *fixture) mustCreate(t *testing.T, req CreateRequest) Session {
	t.Helper()
	s, err := f.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

func wantKind(t *testing.T, err error, kind ErrorKind) {
	t.Helper()
	var se *Error
	if !errors.As(err, &se) || se.Kind != kind {
		t.Fatalf("error = %v, want kind %s", err, kind)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCreateStartsAnIsolatedBrowser(t *testing.T) {
	f := newFixture(t, nil)
	before := time.Now()
	s := f.mustCreate(t, CreateRequest{})

	if !regexp.MustCompile(`^sess_[a-z2-7]{26}$`).MatchString(s.ID) {
		t.Fatalf("session id %q is not an unpredictable sess_ identifier", s.ID)
	}
	if s.Status != StatusRunning || s.Headless {
		t.Fatalf("unexpected session: %+v", s)
	}
	if ttl := s.ExpiresAt.Sub(before); ttl < 59*time.Minute || ttl > 61*time.Minute {
		t.Fatalf("default TTL not applied: expires in %s", ttl)
	}
	inst := f.rt.Instances()[0]
	if s.PID != inst.PID() {
		t.Fatalf("pid = %d, want %d", s.PID, inst.PID())
	}
	wantDir := filepath.Join(f.dir, "profiles", "ephemeral", s.ID)
	if inst.Options.ProfileDir != wantDir {
		t.Fatalf("profile dir = %q, want %q", inst.Options.ProfileDir, wantDir)
	}
	got, ok := f.Get(s.ID)
	if !ok || got.ID != s.ID {
		t.Fatal("Get did not return the session")
	}
}

func TestSessionTokens(t *testing.T) {
	f := newFixture(t, nil)
	a, b := f.mustCreate(t, CreateRequest{}), f.mustCreate(t, CreateRequest{})
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(a.Token) {
		t.Fatalf("token %q is not 256 bits of URL-safe base64", a.Token)
	}
	if a.Token == b.Token {
		t.Fatal("sessions share a token")
	}
	if got, _ := f.Get(a.ID); got.Token != a.Token {
		t.Fatal("the token changed during the session's life")
	}
	if !f.TokenMatches(a.ID, a.Token) {
		t.Fatal("a session's own token must match")
	}
	for name, ok := range map[string]bool{
		"another session's token": f.TokenMatches(a.ID, b.Token),
		"empty token":             f.TokenMatches(a.ID, ""),
		"unknown session":         f.TokenMatches("sess_unknown", a.Token),
		"empty against unknown":   f.TokenMatches("sess_unknown", ""),
		"prefix":                  f.TokenMatches(a.ID, a.Token[:20]),
	} {
		if ok {
			t.Errorf("%s matched", name)
		}
	}

	// The token is not part of what survives a daemon restart.
	raw, err := os.ReadFile(filepath.Join(f.dir, "state", "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), a.Token) || strings.Contains(string(raw), b.Token) {
		t.Fatal("session tokens were written to disk")
	}

	// After the session ends it cannot be attached to, token or not; the
	// token still identifies its former holder so they can be told why.
	if err := f.Terminate(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Attach(a.ID); err == nil {
		t.Fatal("an ended session accepted a client")
	}
	if !f.TokenMatches(a.ID, a.Token) || f.TokenMatches(a.ID, b.Token) {
		t.Fatal("unexpected token matching for an ended session")
	}
}

func TestSessionIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id, err := newSessionID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}

func TestConcurrentSessionsAreIndependent(t *testing.T) {
	f := newFixture(t, nil)
	const n = 5
	var wg sync.WaitGroup
	sessions := make([]Session, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessions[i] = f.mustCreate(t, CreateRequest{})
		}()
	}
	wg.Wait()

	pids, dirs := map[int]bool{}, map[string]bool{}
	for _, inst := range f.rt.Instances() {
		pids[inst.PID()] = true
		dirs[inst.Options.ProfileDir] = true
	}
	if len(pids) != n || len(dirs) != n {
		t.Fatalf("expected %d distinct browsers and profiles, got %d and %d", n, len(pids), len(dirs))
	}
	if len(f.List()) != n || f.ActiveCount() != n {
		t.Fatalf("List/ActiveCount disagree: %d/%d", len(f.List()), f.ActiveCount())
	}

	// Ending one session leaves the others untouched.
	if err := f.Terminate(context.Background(), sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	if f.rt.Running() != n-1 || len(f.List()) != n-1 {
		t.Fatalf("after terminating one: running=%d listed=%d", f.rt.Running(), len(f.List()))
	}
}

func TestRequestValidation(t *testing.T) {
	f := newFixture(t, nil)
	cases := []struct {
		name string
		req  CreateRequest
		want string
	}{
		{"zero ttl", CreateRequest{TTLSeconds: ptr(0)}, "at least 1"},
		{"negative ttl", CreateRequest{TTLSeconds: ptr(-5)}, "at least 1"},
		{"ttl above max", CreateRequest{TTLSeconds: ptr(14401)}, "exceeds the maximum session TTL of 14400"},
		{"profile traversal", CreateRequest{Profile: "../evil"}, "profile must be"},
		{"profile separator", CreateRequest{Profile: "a/b"}, "profile must be"},
		{"profile too long", CreateRequest{Profile: strings.Repeat("a", 65)}, "profile must be"},
		{"profile named like a Windows device", CreateRequest{Profile: "NUL"}, "profile must be"},
		{"profile named like a Windows port", CreateRequest{Profile: "com1"}, "profile must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Create(context.Background(), tc.req)
			wantKind(t, err, KindValidation)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
	if len(f.rt.Instances()) != 0 {
		t.Fatal("invalid requests must not launch browsers")
	}

	s := f.mustCreate(t, CreateRequest{TTLSeconds: ptr(14400), Headless: ptr(true), Profile: "work_profile-1"})
	if !s.Headless || s.Profile != "work_profile-1" {
		t.Fatalf("overrides not applied: %+v", s)
	}
	if ttl := time.Until(s.ExpiresAt); ttl < 3*time.Hour+59*time.Minute {
		t.Fatalf("maximum TTL not honored: %s", ttl)
	}
}

func TestHeadlessPolicy(t *testing.T) {
	t.Run("configured default", func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.Limits.DefaultHeadless = true })
		if s := f.mustCreate(t, CreateRequest{}); !s.Headless {
			t.Fatal("default headless not applied")
		}
		if s := f.mustCreate(t, CreateRequest{Headless: ptr(false)}); s.Headless {
			t.Fatal("explicit headed request ignored")
		}
	})
	t.Run("no interactive desktop", func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.Limits.HeadedSupported = false })
		if s := f.mustCreate(t, CreateRequest{}); !s.Headless {
			t.Fatal("sessions must default to headless without a desktop")
		}
		_, err := f.Create(context.Background(), CreateRequest{Headless: ptr(false)})
		wantKind(t, err, KindValidation)
	})
}

func TestConcurrencyLimit(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Limits.MaxConcurrent = 2 })
	a := f.mustCreate(t, CreateRequest{})
	f.mustCreate(t, CreateRequest{})
	_, err := f.Create(context.Background(), CreateRequest{})
	wantKind(t, err, KindCapacity)

	if err := f.Terminate(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	f.mustCreate(t, CreateRequest{}) // capacity is released by termination

	limits := f.Limits()
	limits.MaxConcurrent = 3
	f.SetLimits(limits)
	f.mustCreate(t, CreateRequest{}) // raised limit applies immediately
}

func TestSessionsBeingLaunchedCountAgainstTheLimit(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Limits.MaxConcurrent = 1 })
	f.rt.Gate = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.Create(context.Background(), CreateRequest{})
		done <- err
	}()
	eventually(t, "the session to be starting", func() bool { return f.ActiveCount() == 1 })
	if list := f.List(); list[0].Status != StatusStarting {
		t.Fatalf("status = %s, want starting", list[0].Status)
	}
	_, err := f.Create(context.Background(), CreateRequest{})
	wantKind(t, err, KindCapacity)
	close(f.rt.Gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNamedProfilesAreExclusiveAndPersistent(t *testing.T) {
	f := newFixture(t, nil)
	a := f.mustCreate(t, CreateRequest{Profile: "Work"})
	profileDir := filepath.Join(f.dir, "profiles", "named", "Work")
	if f.rt.Instances()[0].Options.ProfileDir != profileDir {
		t.Fatalf("unexpected profile dir %q", f.rt.Instances()[0].Options.ProfileDir)
	}

	_, err := f.Create(context.Background(), CreateRequest{Profile: "Work"})
	wantKind(t, err, KindProfileInUse)
	// Case-insensitive filesystems would map this to the same directory.
	_, err = f.Create(context.Background(), CreateRequest{Profile: "work"})
	wantKind(t, err, KindProfileInUse)
	f.mustCreate(t, CreateRequest{Profile: "other"})

	profiles, err := f.Profiles()
	if err != nil {
		t.Fatal(err)
	}
	inUse := map[string]string{}
	for _, p := range profiles {
		if p.InUse {
			inUse[p.Name] = p.SessionID
		}
	}
	if inUse["Work"] != a.ID || len(inUse) != 2 {
		t.Fatalf("profiles = %+v", profiles)
	}
	wantKind(t, f.DeleteProfile("Work"), KindProfileInUse)

	if err := f.Terminate(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profileDir); err != nil {
		t.Fatalf("named profile must persist after its session ends: %v", err)
	}
	f.mustCreate(t, CreateRequest{Profile: "Work"}) // lock released
}

func TestDeleteProfile(t *testing.T) {
	f := newFixture(t, nil)
	s := f.mustCreate(t, CreateRequest{Profile: "scratch"})
	if err := f.Terminate(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.DeleteProfile("scratch"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "profiles", "named", "scratch")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("profile directory still exists")
	}
	wantKind(t, f.DeleteProfile("scratch"), KindNotFound)
	wantKind(t, f.DeleteProfile("../x"), KindValidation)
}

func TestTerminateCleansUp(t *testing.T) {
	f := newFixture(t, nil)
	s := f.mustCreate(t, CreateRequest{})
	profileDir := f.rt.Instances()[0].Options.ProfileDir

	if err := f.Terminate(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if f.rt.Running() != 0 {
		t.Fatal("browser still running")
	}
	if _, err := os.Stat(profileDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ephemeral profile was not removed")
	}
	if len(f.List()) != 0 {
		t.Fatal("terminated session still listed")
	}
	ended, ok := f.Get(s.ID)
	if !ok || ended.Status != StatusTerminated || ended.EndReason != ReasonDeleted || ended.EndedAt.IsZero() {
		t.Fatalf("tombstone = %+v (found=%v)", ended, ok)
	}
	wantKind(t, f.Terminate(context.Background(), s.ID), KindNotFound)
	wantKind(t, f.Terminate(context.Background(), "sess_unknown"), KindNotFound)
}

func TestTerminateSucceedsEvenIfBrowserCloseFails(t *testing.T) {
	f := newFixture(t, nil)
	f.rt.CloseErr = errors.New("stubborn browser")
	s := f.mustCreate(t, CreateRequest{})
	if err := f.Terminate(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if f.ActiveCount() != 0 {
		t.Fatal("session resources must be reclaimed regardless")
	}
}

func TestConcurrentTerminateIsSafe(t *testing.T) {
	f := newFixture(t, nil)
	s := f.mustCreate(t, CreateRequest{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.Terminate(context.Background(), s.ID)
		}()
	}
	wg.Wait()
	if f.ActiveCount() != 0 || f.rt.Running() != 0 {
		t.Fatal("session not cleaned up")
	}
}

func TestTTLExpiryIsIndependentOfClients(t *testing.T) {
	f := newFixture(t, nil)
	s := f.mustCreate(t, CreateRequest{TTLSeconds: ptr(1)})
	attachment, err := f.Attach(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer attachment.Release()

	select {
	case <-attachment.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("session with a connected client did not expire")
	}
	ended, _ := f.Get(s.ID)
	if ended.EndReason != ReasonExpired {
		t.Fatalf("end reason = %q, want expired", ended.EndReason)
	}
	if elapsed := ended.EndedAt.Sub(s.CreatedAt); elapsed < 900*time.Millisecond {
		t.Fatalf("expired too early: %s", elapsed)
	}
	if f.rt.Running() != 0 {
		t.Fatal("browser survived expiry")
	}
	if _, err := os.Stat(f.rt.Instances()[0].Options.ProfileDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ephemeral profile survived expiry")
	}
}

func TestCrashedBrowserIsReclaimed(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Limits.MaxConcurrent = 1 })
	s := f.mustCreate(t, CreateRequest{Profile: "crashy"})
	f.rt.Instances()[0].Crash()

	eventually(t, "the crash to be detected", func() bool { return f.ActiveCount() == 0 })
	ended, ok := f.Get(s.ID)
	if !ok || ended.Status != StatusTerminated || ended.EndReason != ReasonCrashed {
		t.Fatalf("tombstone = %+v", ended)
	}
	// Capacity and the profile lock are both released.
	f.mustCreate(t, CreateRequest{Profile: "crashy"})
}

func TestLaunchFailureReleasesEverything(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Limits.MaxConcurrent = 1 })
	f.rt.LaunchErr = errors.New("no browser installed")
	_, err := f.Create(context.Background(), CreateRequest{Profile: "p"})
	wantKind(t, err, KindLaunchFailed)
	if !strings.Contains(err.Error(), "no browser installed") {
		t.Fatalf("cause missing from %q", err.Error())
	}
	if f.ActiveCount() != 0 {
		t.Fatal("failed launch left a session behind")
	}
	f.rt.LaunchErr = nil
	f.mustCreate(t, CreateRequest{Profile: "p"})
}

func TestAttachTracksClients(t *testing.T) {
	f := newFixture(t, nil)
	s := f.mustCreate(t, CreateRequest{})
	a, err := f.Attach(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.Attach(s.ID)
	if conn, err := a.Connect(context.Background()); err != nil || f.rt.Instances()[0].Connections() != 1 {
		t.Fatalf("Connect must reach the session's browser: %v", err)
	} else {
		_ = conn.Close()
	}
	if got, _ := f.Get(s.ID); got.Clients != 2 {
		t.Fatalf("clients = %d, want 2", got.Clients)
	}
	a.Release()
	a.Release() // idempotent
	if got, _ := f.Get(s.ID); got.Clients != 1 {
		t.Fatalf("clients = %d, want 1", got.Clients)
	}
	b.Release()

	// Disconnecting every client does not end the session.
	if got, _ := f.Get(s.ID); got.Status != StatusRunning || got.Clients != 0 {
		t.Fatalf("session after disconnects: %+v", got)
	}

	if err := f.Terminate(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.Attach(s.ID)
	wantKind(t, err, KindNotRunning)
	_, err = f.Attach("sess_missing")
	wantKind(t, err, KindNotFound)
}

func TestCDPClientsPerSessionAreBounded(t *testing.T) {
	f := newFixture(t, nil)
	s := f.mustCreate(t, CreateRequest{})
	attachments := make([]*Attachment, 0, maxClientsPerSession)
	for range maxClientsPerSession {
		a, err := f.Attach(s.ID)
		if err != nil {
			t.Fatal(err)
		}
		attachments = append(attachments, a)
	}
	_, err := f.Attach(s.ID)
	wantKind(t, err, KindCapacity)
	// A client leaving makes room again.
	attachments[0].Release()
	if _, err := f.Attach(s.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSessionsExpireByTheWallClock(t *testing.T) {
	f := newFixture(t, nil)
	s := f.mustCreate(t, CreateRequest{TTLSeconds: ptr(3600)})
	// A machine that slept: the expiry timer has not fired, yet the
	// session's time is up.
	f.mu.Lock()
	f.active[s.ID].session.ExpiresAt = time.Now().Add(-time.Minute)
	f.mu.Unlock()
	eventually(t, "the overdue session to expire", func() bool {
		got, _ := f.Get(s.ID)
		return got.Status == StatusTerminated && got.EndReason == ReasonExpired
	})
}

// cancelingRuntime launches browsers successfully, but only after the
// caller's context has been canceled.
type cancelingRuntime struct {
	runtime.BrowserRuntime
	cancel context.CancelFunc
}

func (r *cancelingRuntime) Launch(_ context.Context, opts runtime.BrowserOptions) (runtime.BrowserInstance, error) {
	r.cancel()
	return r.BrowserRuntime.Launch(context.Background(), opts)
}

func TestSessionOfAClientThatGaveUpIsDiscarded(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	f.SetRuntime(&cancelingRuntime{BrowserRuntime: f.rt, cancel: cancel})
	if _, err := f.Create(ctx, CreateRequest{Profile: "abandoned"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	if f.ActiveCount() != 0 || f.rt.Running() != 0 {
		t.Fatalf("the session lingers: active=%d running=%d", f.ActiveCount(), f.rt.Running())
	}
	// Its slot and profile are free again.
	f.SetRuntime(f.rt)
	f.mustCreate(t, CreateRequest{Profile: "abandoned"})
}

func TestShutdownTerminatesEverythingAndRejectsNewSessions(t *testing.T) {
	f := newFixture(t, nil)
	a := f.mustCreate(t, CreateRequest{})
	f.mustCreate(t, CreateRequest{Profile: "keep"})

	// A session still launching when shutdown begins is cleaned up too.
	f.rt.Gate = make(chan struct{})
	starting := make(chan error, 1)
	go func() {
		_, err := f.Create(context.Background(), CreateRequest{})
		starting <- err
	}()
	eventually(t, "third session to be starting", func() bool { return f.ActiveCount() == 3 })

	shutdownDone := make(chan struct{})
	go func() {
		f.Shutdown(context.Background())
		close(shutdownDone)
	}()
	// The launch in flight is abandoned rather than waited for: the gate
	// never opens.
	wantKind(t, <-starting, KindShuttingDown)
	<-shutdownDone

	if f.rt.Running() != 0 || f.ActiveCount() != 0 {
		t.Fatalf("after shutdown: running=%d active=%d", f.rt.Running(), f.ActiveCount())
	}
	if ended, _ := f.Get(a.ID); ended.EndReason != ReasonShutdown {
		t.Fatalf("end reason = %q", ended.EndReason)
	}
	_, err := f.Create(context.Background(), CreateRequest{})
	wantKind(t, err, KindShuttingDown)
	if records, _ := f.journal.Load(); len(records) != 0 {
		t.Fatalf("journal not empty after shutdown: %+v", records)
	}
}

func TestActiveChangeNotifications(t *testing.T) {
	var mu sync.Mutex
	var last int
	f := newFixture(t, func(o *Options) {
		o.OnActiveChange = func(n int) {
			mu.Lock()
			last = n
			mu.Unlock()
		}
	})
	get := func() int {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	s := f.mustCreate(t, CreateRequest{})
	if get() != 1 {
		t.Fatalf("active = %d after create", get())
	}
	_ = f.Terminate(context.Background(), s.ID)
	if get() != 0 {
		t.Fatalf("active = %d after terminate", get())
	}
}

func TestJournalTracksRunningSessions(t *testing.T) {
	f := newFixture(t, nil)
	a := f.mustCreate(t, CreateRequest{})
	b := f.mustCreate(t, CreateRequest{Profile: "named"})
	records, err := f.journal.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("journal has %d records, want 2", len(records))
	}
	byID := map[string]Record{}
	for _, r := range records {
		byID[r.ID] = r
	}
	if r := byID[a.ID]; r.PID != a.PID || !r.Ephemeral || !strings.HasSuffix(r.ProfileDir, a.ID) {
		t.Fatalf("record for ephemeral session: %+v", r)
	}
	if r := byID[b.ID]; r.Ephemeral {
		t.Fatalf("record for named session: %+v", r)
	}
	_ = f.Terminate(context.Background(), a.ID)
	records, _ = f.journal.Load()
	if len(records) != 1 || records[0].ID != b.ID {
		t.Fatalf("journal after terminate: %+v", records)
	}
}

// fakeProcs is a scriptable process table.
type fakeProcs struct {
	mu     sync.Mutex
	procs  map[int][]string
	killed []int
}

func (p *fakeProcs) set(pid int, args ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.procs == nil {
		p.procs = map[int][]string{}
	}
	p.procs[pid] = args
}

func (p *fakeProcs) Args(pid int) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	args, ok := p.procs[pid]
	return args, ok
}

func (p *fakeProcs) List() ([]platform.Process, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []platform.Process
	for pid, args := range p.procs {
		out = append(out, platform.Process{PID: pid, Args: args})
	}
	return out, nil
}

func (p *fakeProcs) KillTree(pid int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killed = append(p.killed, pid)
	delete(p.procs, pid)
	return nil
}

func (p *fakeProcs) wasKilled(pid int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.killed {
		if k == pid {
			return true
		}
	}
	return false
}

func TestReconcile(t *testing.T) {
	f := newFixture(t, nil)
	profiles := filepath.Join(f.dir, "profiles")
	ephemeral := func(id string) string { return filepath.Join(profiles, "ephemeral", id) }
	named := filepath.Join(profiles, "named", "work")
	for _, dir := range []string{ephemeral("sess_a"), ephemeral("sess_b"), ephemeral("sess_orphan"), ephemeral("sess_stray"), named} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	keepFile := filepath.Join(named, "Cookies")
	if err := os.WriteFile(keepFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.journal.Save([]Record{
		{ID: "sess_a", PID: 100, ProfileDir: ephemeral("sess_a"), Ephemeral: true},
		{ID: "sess_b", PID: 200, ProfileDir: ephemeral("sess_b"), Ephemeral: true},
		{ID: "sess_c", PID: 300, ProfileDir: named},
		{ID: "sess_d", PID: 400, ProfileDir: ephemeral("sess_d"), Ephemeral: true},
	}); err != nil {
		t.Fatal(err)
	}
	// 100: our browser, still alive.
	f.procs.set(100, "chrome", "--user-data-dir="+ephemeral("sess_a"))
	// 200: the PID was reused by an unrelated process — must not be touched.
	f.procs.set(200, "postgres", "-D", "/var/lib/postgres")
	// 300: our browser on a named profile.
	f.procs.set(300, "chrome", "--headless=new", "--user-data-dir="+named)
	// 400: gone.
	// 500: a browser the journal never recorded.
	f.procs.set(500, "chrome", "--user-data-dir="+ephemeral("sess_orphan"))
	// 600: somebody else's browser with an unrelated profile.
	f.procs.set(600, "chrome", "--user-data-dir="+filepath.Join(t.TempDir(), "someone-elses-profile"))
	// 700: a process that merely mentions our directory positionally.
	f.procs.set(700, "du", "-sh", ephemeral("sess_stray"))

	report, err := f.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{100, 300, 500} {
		if !f.procs.wasKilled(pid) {
			t.Errorf("pid %d (ours) was not terminated", pid)
		}
	}
	for _, pid := range []int{200, 600, 700} {
		if f.procs.wasKilled(pid) {
			t.Errorf("pid %d (not ours) was terminated", pid)
		}
	}
	want := ReconcileReport{RecordedSessions: 4, BrowsersTerminated: 3, ForeignProcessesSkipped: 1, ProfilesRemoved: 4}
	if report != want {
		t.Fatalf("report = %+v, want %+v", report, want)
	}
	if entries, _ := os.ReadDir(filepath.Join(profiles, "ephemeral")); len(entries) != 0 {
		t.Fatalf("%d ephemeral profiles survived", len(entries))
	}
	if _, err := os.Stat(keepFile); err != nil {
		t.Fatal("named profile data must survive reconciliation")
	}
	if records, _ := f.journal.Load(); len(records) != 0 {
		t.Fatalf("journal not cleared: %+v", records)
	}

	// Reconciliation is deterministic: a second pass finds nothing to do.
	again, err := f.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again != (ReconcileReport{}) {
		t.Fatalf("second pass report = %+v, want zero", again)
	}
}

func TestReconcileSurvivesCorruptJournal(t *testing.T) {
	f := newFixture(t, nil)
	orphanDir := filepath.Join(f.dir, "profiles", "ephemeral", "sess_x")
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "state", "sessions.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.procs.set(42, "chrome", "--user-data-dir="+orphanDir)

	report, err := f.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !f.procs.wasKilled(42) || report.BrowsersTerminated != 1 || report.ProfilesRemoved != 1 {
		t.Fatalf("report = %+v", report)
	}
	if records, err := f.journal.Load(); err != nil || len(records) != 0 {
		t.Fatalf("journal not repaired: %v %+v", err, records)
	}
}

func TestOwnershipMatching(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	dir := filepath.Join(root, "ephemeral", "sess_1")
	if !referencesDir([]string{"chrome", "--user-data-dir=" + dir}, dir) {
		t.Error("exact flag value must match")
	}
	if referencesDir([]string{"chrome", "--user-data-dir=" + dir + "-other"}, dir) {
		t.Error("a sibling directory must not match")
	}
	if referencesDir([]string{"ls", dir}, dir) {
		t.Error("positional arguments must not match")
	}
	if !referencesDirUnder([]string{"--user-data-dir=" + dir}, root) {
		t.Error("a directory inside the root must match")
	}
	if referencesDirUnder([]string{"--user-data-dir=" + root}, root) {
		t.Error("the root itself must not match")
	}
	if referencesDirUnder([]string{"--user-data-dir=" + root + "-backup" + string(filepath.Separator) + "x"}, root) {
		t.Error("a path that merely shares a prefix must not match")
	}
	if referencesDirUnder([]string{"--flag=relative/profiles/x"}, root) {
		t.Error("relative paths must not match")
	}
}
