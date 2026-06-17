package downloader

import (
	"sync"
)

// dispatchTracker encapsulates in-memory tracking of article fetch attempts
// and active in-flight requests. Exposes Lock/Unlock to support compound
// read-decide-write transactions in tryDispatch, while worker updates
// (decrement, unmark, clear) are self-locking.
type dispatchTracker struct {
	mu       sync.Mutex
	tryList  map[string]serverMask
	inFlight map[string]int
}

func newDispatchTracker() *dispatchTracker {
	return &dispatchTracker{
		tryList:  make(map[string]serverMask),
		inFlight: make(map[string]int),
	}
}

// Lock acquires the tracker's lock. Used for compound transactions in tryDispatch.
func (m *dispatchTracker) Lock() { m.mu.Lock() }

// Unlock releases the tracker's lock.
func (m *dispatchTracker) Unlock() { m.mu.Unlock() }

// InFlightLocked returns the active request count for the key.
// Caller must hold Lock().
func (m *dispatchTracker) InFlightLocked(key string) int {
	return m.inFlight[key]
}

// IncrementInFlightLocked increments the active request count for the key.
// Caller must hold Lock().
func (m *dispatchTracker) IncrementInFlightLocked(key string) {
	m.inFlight[key]++
}

// DecrementInFlight decrements the active request count for the key.
// Self-locking.
func (m *dispatchTracker) DecrementInFlight(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inFlight[key] <= 1 {
		delete(m.inFlight, key)
		return
	}
	m.inFlight[key]--
}

// TryListLocked returns the server tried mask for the key.
// Caller must hold Lock().
func (m *dispatchTracker) TryListLocked(key string) (serverMask, bool) {
	mask, ok := m.tryList[key]
	return mask, ok
}

// SetTriedLocked updates the server tried mask for the key.
// Caller must hold Lock().
func (m *dispatchTracker) SetTriedLocked(key string, mask serverMask) {
	m.tryList[key] = mask
}

// UnmarkTried removes a server from the tried mask for the key.
// Self-locking.
func (m *dispatchTracker) UnmarkTried(key string, serverIdx int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mask, ok := m.tryList[key]
	if !ok {
		return
	}
	mask.unset(serverIdx)
	if mask.isEmpty() {
		delete(m.tryList, key)
	} else {
		m.tryList[key] = mask
	}
}

// ClearTried removes the entire tried mask entry for the key.
// Self-locking.
func (m *dispatchTracker) ClearTried(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tryList, key)
}

// Len returns the number of tracked entries.
// Self-locking, used primarily for tests and assertions.
func (m *dispatchTracker) Len() (tryListLen, inFlightLen int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tryList), len(m.inFlight)
}
