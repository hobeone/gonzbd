// Package downloader manages a pool of NNTP servers, their connection
// state, per-server penalty tracking, and DNS resolution.
//
// Concurrency model: Server is safe for concurrent use. Simple integer
// counters use sync/atomic; the penalty-expiry timestamp and resolved-
// address cache are guarded by a sync.RWMutex so readers do not block
// on each other. The Active and shouldDeactivateOptional helpers are
// intentionally free of side effects and may be called from any
// goroutine.
//
// What this package does NOT do: dial NNTP sockets, assign articles to
// connections, or schedule reconnect timers. Those responsibilities
// belong to Step 2.3 (Downloader Orchestrator).
package downloader

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/config"
)

// Server wraps a config.ServerConfig and tracks the runtime state of
// one NNTP server's connection pool: bad/good connection counters,
// penalty-expiry timestamp, resolved addresses, and whether the server
// has been administratively deactivated.
//
// Server does not open or close NNTP connections itself — it is a
// pure state record. The dispatcher (Step 2.3) reads Active() before
// handing work to a server and calls ApplyPenalty / RecordBadConnection
// / RecordGoodConnection when connections complete or fail.
type Server struct {
	// cfg is the immutable configuration for this server.
	cfg config.ServerConfig

	// badConns and goodConns are updated atomically. Using atomic.Int64
	// for both avoids lock contention on the hot path (every article
	// completion updates one of these).
	badConns  atomic.Int64
	goodConns atomic.Int64

	mu sync.RWMutex
	// penaltyExpiry is the time after which this server is eligible for
	// dispatch again. Zero means no active penalty.
	penaltyExpiry time.Time
	// resolvedAddrs holds the last successful DNS resolution result.
	// Callers pick addrs[0] when connecting. Guarded by mu.
	resolvedAddrs []netip.Addr
	// resolvedAt records when resolvedAddrs was populated so callers can
	// implement session-scoped caching (re-resolve on reconnect after
	// timeout).
	resolvedAt time.Time
	// deactivated is set to true when shouldDeactivateOptional decides
	// the server's bad-connection ratio is too high. Reset by the
	// dispatcher when the penalty expires.
	deactivated bool
}

// NewServer creates a Server wrapping cfg. Counters start at zero and
// no penalty is set; the server is considered active immediately.
func NewServer(cfg config.ServerConfig) *Server {
	return &Server{cfg: cfg}
}

// Cfg returns the immutable ServerConfig for this server.
func (s *Server) Cfg() config.ServerConfig { return s.cfg }

// Connections returns the configured maximum number of simultaneous
// NNTP connections for this server (cfg.Connections).
func (s *Server) Connections() int { return s.cfg.Connections }

// BadConnections returns the current count of recorded bad connections.
func (s *Server) BadConnections() int64 { return s.badConns.Load() }

// GoodConnections returns the current count of recorded good connections.
func (s *Server) GoodConnections() int64 { return s.goodConns.Load() }

// RecordBadConnection increments the bad-connection counter. Thread-safe.
func (s *Server) RecordBadConnection() { s.badConns.Add(1) }

// RecordGoodConnection increments the good-connection counter. Thread-safe.
func (s *Server) RecordGoodConnection() {
	s.goodConns.Add(1)
	s.badConns.Store(0)
}

// ApplyPenalty sets the penalty-expiry to now+d and, when
// shouldDeactivateOptional returns true, marks the server as
// deactivated. Required servers are never deactivated by penalty.
//
// The now argument is passed explicitly (rather than calling time.Now
// internally) so callers and tests can control the clock.
func (s *Server) ApplyPenalty(d time.Duration) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.penaltyExpiry = now.Add(d)
	if !s.cfg.Required && shouldDeactivateOptional(s) {
		s.deactivated = true
	}
}

// PenaltyExpiry returns the time at which the current penalty expires.
// The zero value means no penalty is active.
func (s *Server) PenaltyExpiry() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.penaltyExpiry
}

// Active reports whether the server is eligible for dispatch at the
// given time. A server is inactive when it is administratively
// deactivated OR when a penalty has not yet expired. The Enable flag
// from config is also checked — a disabled server is never active.
func (s *Server) Active(now time.Time) bool {
	if !s.cfg.Enable {
		return false
	}
	s.mu.RLock()
	deactivated := s.deactivated
	penaltyExpiry := s.penaltyExpiry
	s.mu.RUnlock()

	expired := !penaltyExpiry.IsZero() && now.After(penaltyExpiry)
	if deactivated {
		if expired {
			s.ClearDeactivation()
			return true
		}
		return false
	}
	return penaltyExpiry.IsZero() || expired
}

// ClearDeactivation lifts the deactivated flag when the penalty has
// expired. The dispatcher calls this when it detects that PenaltyExpiry
// has passed so the server can re-enter the pool.
func (s *Server) ClearDeactivation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deactivated = false
	s.penaltyExpiry = time.Time{}
}

// ResolvedAddrs returns the cached DNS resolution result and the time
// it was recorded. An empty slice means no successful resolution has
// been performed yet.
func (s *Server) ResolvedAddrs() ([]netip.Addr, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]netip.Addr, len(s.resolvedAddrs))
	copy(out, s.resolvedAddrs)
	return out, s.resolvedAt
}

// SetResolvedAddrs stores a fresh DNS result. at should be time.Now()
// at the point the lookup completed so callers can implement TTL checks.
func (s *Server) SetResolvedAddrs(addrs []netip.Addr, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolvedAddrs = make([]netip.Addr, len(addrs))
	copy(s.resolvedAddrs, addrs)
	s.resolvedAt = at
}
