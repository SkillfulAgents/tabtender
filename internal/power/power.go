// Package power keeps the machine awake while the service needs it.
package power

import "sync"

// Policy says when idle system sleep is prevented.
type Policy struct {
	// WhileActive prevents sleep while browser sessions are active.
	WhileActive bool
	// Always prevents sleep for as long as the service runs.
	Always bool
}

// Inhibitor prevents idle system sleep while its policy calls for it.
type Inhibitor struct {
	mu      sync.Mutex
	policy  Policy
	active  bool
	closed  bool
	held    bool
	release func()
}

// NewInhibitor returns an inhibitor that follows policy from the start.
func NewInhibitor(policy Policy) *Inhibitor {
	i := &Inhibitor{}
	i.SetPolicy(policy)
	return i
}

// SetPolicy changes the policy, applying it immediately.
func (i *Inhibitor) SetPolicy(policy Policy) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.policy = policy
	i.syncLocked()
}

// SetActive reports whether sessions are currently active.
func (i *Inhibitor) SetActive(active bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.active = active
	i.syncLocked()
}

// Held reports whether sleep is currently being prevented.
func (i *Inhibitor) Held() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.held
}

// Close releases the inhibition for good.
func (i *Inhibitor) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	i.syncLocked()
}

func (i *Inhibitor) syncLocked() {
	want := !i.closed && (i.policy.Always || i.policy.WhileActive && i.active)
	switch {
	case want && !i.held:
		if release, err := acquire(); err == nil {
			i.release, i.held = release, true
		}
	case !want && i.held:
		i.release()
		i.release, i.held = nil, false
	}
}
