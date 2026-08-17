package downloader

import (
	"sync"
)

// articleKey identifies one article for try-list and in-flight tracking.
//
// The job ID is part of the key, not decoration. These maps were previously
// keyed on the Message-ID alone, which made two resident jobs that share a
// Message-ID — the same NZB added twice, or a reposted par2 volume — share a
// single try-list entry: one job's article could reach ErrNoServersLeft
// without ever having been fetched for the other, and the in-flight count
// would skew by the same route.
//
// artIdx rather than the Message-ID because it is the identifier the manifest
// already assigns, it is unique within a job by construction, and it cannot be
// absent. Nothing here needs the Message-ID's content; it needs an identity,
// and the index is the one the rest of the pipeline already agrees on.
type articleKey struct {
	jobID  string
	artIdx int32
}

// dispatchTracker encapsulates in-memory tracking of article fetch attempts
// and active in-flight requests. Exposes Lock/Unlock to support compound
// read-decide-write transactions in tryDispatch, while worker updates
// (decrement, unmark, clear) are self-locking.
type dispatchTracker struct {
	mu       sync.Mutex
	tryList  map[articleKey]serverMask
	inFlight map[articleKey]int
}

func newDispatchTracker() *dispatchTracker {
	return &dispatchTracker{
		tryList:  make(map[articleKey]serverMask),
		inFlight: make(map[articleKey]int),
	}
}

// Lock acquires the tracker's lock. Used for compound transactions in tryDispatch.
func (m *dispatchTracker) Lock() { m.mu.Lock() }

// Unlock releases the tracker's lock.
func (m *dispatchTracker) Unlock() { m.mu.Unlock() }

// InFlightLocked returns the active request count for the key.
// Caller must hold Lock().
func (m *dispatchTracker) InFlightLocked(key articleKey) int {
	return m.inFlight[key]
}

// IncrementInFlightLocked increments the active request count for the key.
// Caller must hold Lock().
func (m *dispatchTracker) IncrementInFlightLocked(key articleKey) {
	m.inFlight[key]++
}

// DecrementInFlight decrements the active request count for the key.
// Self-locking.
func (m *dispatchTracker) DecrementInFlight(key articleKey) {
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
func (m *dispatchTracker) TryListLocked(key articleKey) (serverMask, bool) {
	mask, ok := m.tryList[key]
	return mask, ok
}

// SetTriedLocked updates the server tried mask for the key.
// Caller must hold Lock().
func (m *dispatchTracker) SetTriedLocked(key articleKey, mask serverMask) {
	m.tryList[key] = mask
}

// UnmarkTried removes a server from the tried mask for the key.
// Self-locking.
func (m *dispatchTracker) UnmarkTried(key articleKey, serverIdx int) {
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
func (m *dispatchTracker) ClearTried(key articleKey) {
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
