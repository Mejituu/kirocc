package customprov

import "sync"

// mediaMemory records provider+model pairs learned to reject media input.
// Process-local, grows monotonically, cleared only by restart.
type mediaMemory struct {
	mu     sync.Mutex
	models map[string]struct{}
}

func newMediaMemory() *mediaMemory {
	return &mediaMemory{models: make(map[string]struct{})}
}

// mediaMemoryKey joins provider and model with a NUL separator so the two
// dimensions cannot bleed into each other.
func mediaMemoryKey(provider, model string) string {
	return provider + "\x00" + model
}

func (m *mediaMemory) learn(provider, model string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.models == nil {
		m.models = make(map[string]struct{})
	}
	m.models[mediaMemoryKey(provider, model)] = struct{}{}
}

func (m *mediaMemory) has(provider, model string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.models[mediaMemoryKey(provider, model)]
	return ok
}
