package secrets

import "sync"

// Memory is an in-process Store intended for tests.
type Memory struct {
	mu     sync.Mutex
	values map[string]string
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{values: map[string]string{}} }

// Get implements Store.
func (m *Memory) Get(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[name]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Set implements Store.
func (m *Memory) Set(name, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[name] = value
	return nil
}

// Delete implements Store.
func (m *Memory) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, name)
	return nil
}

// Backend implements Store.
func (m *Memory) Backend() string { return "memory" }
