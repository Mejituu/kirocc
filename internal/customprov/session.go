package customprov

import (
	"container/list"
	"sync"
)

// KiroProviderName is the reserved name for the Kiro path: mappings groups
// key it, mapping targets route to it, and providers may not take it.
const KiroProviderName = "kiro"

// sessionTableCapacity bounds how many Claude Code sessions the
// session-following table tracks. A var so tests can shrink it.
var sessionTableCapacity = 1000

// sessionTable maps a Claude Code session ID to the provider its most
// recent unmapped /v1/messages request used. A map plus a doubly-linked
// list gives least-recently-updated eviction under one mutex.
type sessionTable struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
}

// sessionEntry is the LRU payload; the session ID is repeated here so the
// oldest element can be evicted without a reverse index.
type sessionEntry struct {
	sessionID string
	provider  string
}

// newSessionTable builds a table evicting past capacity entries.
func newSessionTable(capacity int) *sessionTable {
	return &sessionTable{
		max:   capacity,
		ll:    list.New(),
		items: make(map[string]*list.Element, capacity),
	}
}

// get returns the provider the session last used, refreshing the entry's
// recency.
func (t *sessionTable) get(sessionID string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.items[sessionID]; ok {
		t.ll.MoveToFront(el)
		return el.Value.(sessionEntry).provider, true
	}
	return "", false
}

// put records a session's provider, evicting the least recently updated
// entry when the table is over capacity.
func (t *sessionTable) put(sessionID, provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.items[sessionID]; ok {
		el.Value = sessionEntry{sessionID: sessionID, provider: provider}
		t.ll.MoveToFront(el)
		return
	}
	t.items[sessionID] = t.ll.PushFront(sessionEntry{sessionID: sessionID, provider: provider})
	for t.ll.Len() > t.max {
		oldest := t.ll.Back()
		if oldest == nil {
			return
		}
		delete(t.items, oldest.Value.(sessionEntry).sessionID)
		t.ll.Remove(oldest)
	}
}
