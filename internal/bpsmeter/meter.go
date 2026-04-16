// Package bpsmeter tracks real-time download speed (BPS) across all servers
// and per-server, enforces configurable quotas, persists state across restarts,
// and provides a live-updatable speed limiter.
//
// Rolling-window BPS: samples are stored in a ring buffer of 1-second buckets.
// Each call to Record stamps bytes into the current bucket; BPS is the sum of
// bytes across all buckets inside the window divided by the window duration.
// This is O(window) on read and O(1) on write, with a natural decay as old
// buckets age out — no exponential smoothing needed.
package bpsmeter

import (
	"sync"
	"time"
)

const defaultWindow = 10 * time.Second

// bucket holds the bytes received and the second-aligned timestamp it belongs to.
type bucket struct {
	ts    time.Time
	bytes int64
}

// serverState holds per-server ring buffer and lifetime total.
type serverState struct {
	buckets  []bucket
	head     int // next write index
	lifetime int64
}

// Meter tracks byte counts over time. Safe for concurrent callers.
// The rolling BPS is computed over Window (default 10 s) using a ring buffer
// of 1-second buckets. Buckets older than Window are ignored, causing BPS to
// decay naturally to 0 once traffic stops.
type Meter struct {
	mu      sync.Mutex
	clock   func() time.Time
	window  time.Duration
	bucketN int // number of buckets == ceil(window/s)+1

	// "" key is the aggregate across all servers.
	servers map[string]*serverState
}

// NewMeter creates a Meter with the given rolling window and clock function.
// Pass nil for clock to use time.Now.
func NewMeter(window time.Duration, clock func() time.Time) *Meter {
	if window <= 0 {
		window = defaultWindow
	}
	if clock == nil {
		clock = time.Now
	}
	n := int(window.Seconds()) + 2
	m := &Meter{
		clock:   clock,
		window:  window,
		bucketN: n,
		servers: make(map[string]*serverState),
	}
	m.servers[""] = m.newServerState()
	return m
}

func (m *Meter) newServerState() *serverState {
	return &serverState{
		buckets: make([]bucket, m.bucketN),
	}
}

// getOrCreate returns the serverState for server, creating it if needed.
// Must be called with m.mu held.
func (m *Meter) getOrCreate(server string) *serverState {
	s, ok := m.servers[server]
	if !ok {
		s = m.newServerState()
		m.servers[server] = s
	}
	return s
}

// record adds n bytes to a single serverState at the given time.
func (m *Meter) record(s *serverState, now time.Time, n int64) {
	sec := now.Truncate(time.Second)
	cur := &s.buckets[s.head%m.bucketN]

	if cur.ts.Equal(sec) {
		cur.bytes += n
	} else {
		s.head++
		s.buckets[s.head%m.bucketN] = bucket{ts: sec, bytes: n}
	}
	s.lifetime += n
}

// Record adds n bytes to the total. server may be "" for aggregate-only.
// Safe for many concurrent callers.
func (m *Meter) Record(server string, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock()
	if server != "" {
		m.record(m.getOrCreate(server), now, n)
	}
	m.record(m.servers[""], now, n)
}

// bps computes the rolling BPS for the given serverState at time now.
func (m *Meter) bps(s *serverState, now time.Time) float64 {
	cutoff := now.Add(-m.window)
	var total int64
	for i := range s.buckets {
		b := &s.buckets[i]
		if !b.ts.IsZero() && b.ts.After(cutoff) {
			total += b.bytes
		}
	}
	return float64(total) / m.window.Seconds()
}

// BPS returns the current rolling BPS for the named server.
// Pass "" for the aggregate across all servers.
func (m *Meter) BPS(server string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.servers[server]
	if !ok {
		return 0
	}
	return m.bps(s, m.clock())
}

// Total returns lifetime bytes for the named server.
func (m *Meter) Total(server string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.servers[server]
	if !ok {
		return 0
	}
	return s.lifetime
}

// MeterSnapshot is a point-in-time view of the Meter.
type MeterSnapshot struct {
	BPS     float64
	Total   int64
	Servers map[string]ServerStat
	At      time.Time
}

// ServerStat holds the current BPS and lifetime total for a single server.
type ServerStat struct {
	BPS   float64
	Total int64
}

// Snapshot returns a point-in-time view of all per-server totals and current BPS.
func (m *Meter) Snapshot() MeterSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock()
	agg := m.servers[""]
	snap := MeterSnapshot{
		BPS:     m.bps(agg, now),
		Total:   agg.lifetime,
		Servers: make(map[string]ServerStat, len(m.servers)-1),
		At:      now,
	}
	for k, s := range m.servers {
		if k == "" {
			continue
		}
		snap.Servers[k] = ServerStat{
			BPS:   m.bps(s, now),
			Total: s.lifetime,
		}
	}
	return snap
}
