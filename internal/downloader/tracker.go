package downloader

import (
	"sync"
	"weak"

	"github.com/hobeone/gonzbd/internal/job"
)

// articleKey identifies one article of one job instance for try-list and
// in-flight tracking.
//
// The instance is part of the key, not the job ID alone. A retry registers a
// new *job.Job under the same ID while a fetch for the removed instance can
// still be running, and on an ID key that fetch's completion decremented the
// retry's in-flight count and unmarked or cleared the retry's try-list (#665).
// On an instance key it reaches only its own entries.
//
// The instance is held weakly so that an entry left behind by an instance
// does not keep its job and manifest reachable. Weak pointers compare equal
// only when made from the same pointer, even after the object is reclaimed,
// so a later instance cannot alias an earlier one's entry.
//
// jobID is kept beside the instance so ClearJob can clear every instance's
// entries under an ID. keyFor derives both from one *job.Job, so they cannot
// disagree.
//
// artIdx rather than the Message-ID because it is the identifier the manifest
// already assigns, it is unique within a job by construction, and it cannot be
// absent. Two jobs sharing a Message-ID therefore have separate entries.
type articleKey struct {
	jobID  string
	inst   weak.Pointer[job.Job]
	artIdx int32
}

// keyFor is the constructor of articleKey. Its zero value, a nil instance,
// is unreachable through it: j.ID() dereferences j.
// `git grep -n 'articleKey{' -- 'internal/downloader/*.go' ':(exclude)*_test.go'`
// finds 2: the literal below, and this comment quoting the pattern.
func keyFor(j *job.Job, artIdx int32) articleKey {
	return articleKey{jobID: j.ID(), inst: weak.Make(j), artIdx: artIdx}
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

// ClearJob removes all try-list and in-flight tracking entries for a given
// jobID, whichever instance under that ID they were made for.
func (m *dispatchTracker) ClearJob(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.tryList {
		if k.jobID == jobID {
			delete(m.tryList, k)
		}
	}
	for k := range m.inFlight {
		if k.jobID == jobID {
			delete(m.inFlight, k)
		}
	}
}

// Len returns the number of tracked entries.
// Self-locking, used primarily for tests and assertions.
func (m *dispatchTracker) Len() (tryListLen, inFlightLen int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tryList), len(m.inFlight)
}
