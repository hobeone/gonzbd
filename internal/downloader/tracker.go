package downloader

import (
	"sync"
)

type dispatchTracker interface {
	Lock()
	Unlock()
	InFlightLocked(key string) int
	IncrementInFlightLocked(key string)
	DecrementInFlightLocked(key string)
	TryListLocked(key string) (serverMask, bool)
	SetTriedLocked(key string, mask serverMask)
	UnmarkTriedLocked(key string, serverIdx int)
	ClearTriedLocked(key string)
	LenLocked() (tryListLen, inFlightLen int)
}

type mapTracker struct {
	mu       sync.Mutex
	tryList  map[string]serverMask
	inFlight map[string]int
}

func newMapTracker() *mapTracker {
	return &mapTracker{
		tryList:  make(map[string]serverMask),
		inFlight: make(map[string]int),
	}
}

func (m *mapTracker) Lock()   { m.mu.Lock() }
func (m *mapTracker) Unlock() { m.mu.Unlock() }

func (m *mapTracker) InFlightLocked(key string) int {
	return m.inFlight[key]
}

func (m *mapTracker) IncrementInFlightLocked(key string) {
	m.inFlight[key]++
}

func (m *mapTracker) DecrementInFlightLocked(key string) {
	if m.inFlight[key] <= 1 {
		delete(m.inFlight, key)
		return
	}
	m.inFlight[key]--
}

func (m *mapTracker) TryListLocked(key string) (serverMask, bool) {
	mask, ok := m.tryList[key]
	return mask, ok
}

func (m *mapTracker) SetTriedLocked(key string, mask serverMask) {
	m.tryList[key] = mask
}

func (m *mapTracker) UnmarkTriedLocked(key string, serverIdx int) {
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

func (m *mapTracker) ClearTriedLocked(key string) {
	delete(m.tryList, key)
}

func (m *mapTracker) LenLocked() (tryListLen, inFlightLen int) {
	return len(m.tryList), len(m.inFlight)
}
