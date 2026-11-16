// Package power keeps the machine awake while browser sessions are active.
package power

import "sync"

// Inhibitor prevents idle system sleep while held.
type Inhibitor struct {
	mu      sync.Mutex
	enabled bool
	wanted  bool
	held    bool
	release func()
}

// NewInhibitor returns an inhibitor; enabled mirrors app.preventSleep.
func NewInhibitor(enabled bool) *Inhibitor { return &Inhibitor{enabled: enabled} }

// SetEnabled turns the feature on or off, applying the change immediately.
func (i *Inhibitor) SetEnabled(enabled bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.enabled = enabled
	i.syncLocked()
}

// SetActive reports whether sessions are currently active.
func (i *Inhibitor) SetActive(active bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.wanted = active
	i.syncLocked()
}

// Held reports whether sleep is currently being prevented.
func (i *Inhibitor) Held() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.held
}

// Close releases the inhibition.
func (i *Inhibitor) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.wanted = false
	i.syncLocked()
}

func (i *Inhibitor) syncLocked() {
	want := i.enabled && i.wanted
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
