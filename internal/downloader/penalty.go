package downloader

import (
	"errors"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/nntp"
)

// PenaltyFor maps an error returned by an NNTP operation to the
// appropriate server penalty duration (spec §3.6). The returned
// duration should be passed directly to Server.ApplyPenalty.
//
// The mapping is:
//
//	ErrAuthRejected      → PenaltyPerm   (bad credentials)
//	ErrServerUnavailable → Penalty502    (502/503 from server)
//	ErrTransient         → PenaltyVeryShort (bare 4xx, unknown cause)
//	ErrNoArticle         → 0             (not a server fault; no penalty)
//	ErrAuthRequired      → PenaltyShort  (re-auth prompt; transient)
//	ErrClosed            → PenaltyUnknown (unexpected disconnect)
//	anything else        → PenaltyUnknown (catch-all)
//
// A zero return means no penalty should be applied.
func PenaltyFor(err error) time.Duration {
	switch {
	case errors.Is(err, nntp.ErrAuthRejected):
		return constants.PenaltyPerm
	case errors.Is(err, nntp.ErrServerUnavailable):
		return constants.Penalty502
	case errors.Is(err, nntp.ErrTransient):
		return constants.PenaltyVeryShort
	case errors.Is(err, nntp.ErrNoArticle):
		// Not a server fault — article absent from this server's spool.
		// Move on to the next server; no penalty.
		return 0
	case errors.Is(err, nntp.ErrAuthRequired):
		// Server asked for auth unexpectedly mid-session. Treat as transient.
		return constants.PenaltyShort
	case errors.Is(err, nntp.ErrClosed):
		// Connection closed unexpectedly — unknown cause.
		return constants.PenaltyUnknown
	case errors.Is(err, nntp.ErrInvalidState):
		// Programming error; log and apply a short hold to avoid spin.
		return constants.PenaltyShort
	default:
		return constants.PenaltyUnknown
	}
}

// shouldDeactivateOptional returns true when server s is an optional
// (non-required) server whose bad-connection ratio exceeds
// constants.OptionalDeactivationThreshold (0.3). Required servers are
// never deactivated regardless of their ratio.
//
// The function is called with s.mu held in ApplyPenalty; it reads the
// atomic counters directly so it does not need to acquire a lock of its
// own.
//
// Optional and Required are independent boolean flags in ServerConfig.
// The deactivation logic only fires when Optional==true AND
// Required==false; a server that is both optional and required is
// treated as required (never auto-deactivated).
func shouldDeactivateOptional(s *Server) bool {
	if !s.cfg.Optional || s.cfg.Required {
		return false
	}
	total := s.cfg.Connections
	if total <= 0 {
		return false
	}
	bad := s.badConns.Load()
	ratio := float64(bad) / float64(total)
	return ratio > constants.OptionalDeactivationThreshold
}
