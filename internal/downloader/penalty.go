package downloader

import (
	"errors"
	"net"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/telemetry"
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
//	ErrDesynced          → PenaltyUnknown (protocol desync; see below)
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
	case errors.Is(err, nntp.ErrDesynced):
		// Same duration the catch-all would give, stated explicitly so
		// it is a decision rather than a fall-through. A desync is a
		// real server-side fault and backing off is right, but nothing
		// here justifies a harsher figure: it may be a one-off, and
		// PenaltyPerm would retire a server on a single bad response.
		// Revisit with evidence — classifyConnError now counts these
		// under their own label, which is where that evidence will
		// come from.
		return constants.PenaltyUnknown
	case errors.Is(err, nntp.ErrInvalidState):
		// Programming error; log and apply a short hold to avoid spin.
		return constants.PenaltyShort
	default:
		return constants.PenaltyUnknown
	}
}

// classifyConnError maps a connection-level failure (a dial error, or a
// Fetch/Stat error other than the 430/ErrNoArticle case already handled
// elsewhere) to a telemetry.PipelineErrors class label. Its sentinel set
// overlaps heavily with PenaltyFor's — both classify the same underlying
// errors — but is not identical: PenaltyFor exists to pick a penalty
// duration, this exists to name a diagnostic cause, and the two sets can
// diverge (e.g. ErrInvalidCredential is a local config defect with no
// penalty-worthy server behavior, but is still worth naming here).
func classifyConnError(err error) string {
	switch {
	case errors.Is(err, nntp.ErrAuthRejected):
		return telemetry.ErrClassNNTPAuthRejected
	case errors.Is(err, nntp.ErrServerUnavailable):
		return telemetry.ErrClassNNTPServerUnavailable
	case errors.Is(err, nntp.ErrTransient):
		return telemetry.ErrClassNNTPTransient
	case errors.Is(err, nntp.ErrAuthRequired):
		return telemetry.ErrClassNNTPAuthRequired
	case errors.Is(err, nntp.ErrClosed):
		return telemetry.ErrClassConnClosed
	case errors.Is(err, nntp.ErrInvalidState):
		return telemetry.ErrClassNNTPInvalidState
	case errors.Is(err, nntp.ErrInvalidCredential):
		return telemetry.ErrClassInvalidCredential
	case errors.Is(err, nntp.ErrDesynced):
		// Naming this apart from other_connection_error is the whole
		// point of the sentinel. A desync means the server answered
		// about an article we did not ask for — a server bug or a
		// MITM, not a flaky link — and it is the one connection
		// failure here that warrants a human looking at it. Folded
		// into the catch-all it is invisible.
		return telemetry.ErrClassNNTPDesynced
	}
	// Check Timeout() before *net.OpError: net.OpError itself implements
	// Timeout() bool, so checking OpError first would swallow every real
	// network timeout into "network_error" and leave "timeout" dead code.
	if timeoutErr, ok := errors.AsType[interface {
		error
		Timeout() bool
	}](err); ok && timeoutErr.Timeout() {
		return telemetry.ErrClassTimeout
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return telemetry.ErrClassNetworkError
	}
	return telemetry.ErrClassOtherConnectionError
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
