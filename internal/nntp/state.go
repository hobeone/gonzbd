// Package nntp implements a minimal NNTP client tailored to SABnzbd's
// binary-download workload: fetch-by-Message-ID, aggressive pipelining,
// strict state machine, and fine-grained TLS verification control.
//
// The package intentionally does *not* implement POST, IHAVE, NEWGROUPS,
// NEWNEWS, GROUP selection, OVER/XOVER, or any other verb unrelated to
// Usenet binary retrieval. The protocol surface is narrow on purpose;
// see spec §3.2 for the state machine and §3.5 for pipelining semantics.
//
// # Concurrency model
//
// Each *Conn owns its TCP socket and is safe for concurrent use by
// multiple goroutines. A caller issues `Fetch(ctx, msgid)` and blocks
// until the response arrives (or ctx is cancelled). Internally a single
// reader goroutine consumes the socket; callers serialise writes under
// a mutex and await their result on a per-command channel. The
// pipelining semaphore bounds in-flight commands to
// `ServerConfig.PipeliningRequests`.
package nntp

import "fmt"

// State is the lifecycle position of a single NNTP connection. The
// state machine mirrors spec §3.2 but collapses USER_SENT/USER_OK and
// PASS_SENT into transient states that only exist while Authenticate
// is blocked waiting for a response — Authenticate drives the whole
// sequence synchronously, so no external code observes those interim
// states. What the rest of the package sees is: Disconnected →
// Connected → Authenticated → Ready → Closed.
//
// 480 re-authentication is modelled by transitioning Ready → Connected
// and letting the caller re-run Authenticate; the state machine does
// not model a separate "re-auth" state because the transitions are the
// same ones already exercised during initial login.
type State int

const (
	// StateDisconnected is the zero value. A Conn in this state has
	// no live socket; call Dial to advance.
	StateDisconnected State = iota

	// StateConnected means the TCP (and TLS) connection is up and the
	// server greeting (200/201) has been consumed. Ready to send
	// AUTHINFO.
	StateConnected

	// StateAuthenticated means AUTHINFO USER+PASS succeeded (281). The
	// connection is not yet considered dispatch-ready because some
	// servers gate command availability behind a CAPABILITIES probe.
	StateAuthenticated

	// StateReady means the connection has completed any post-auth
	// probing and may service Fetch/Stat/… calls.
	StateReady

	// StateClosed is terminal. The socket is closed and the Conn is
	// no longer usable; discard it and Dial a fresh one.
	StateClosed
)

// String returns the canonical lowercase name used in error messages
// and logs.
func (s State) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnected:
		return "connected"
	case StateAuthenticated:
		return "authenticated"
	case StateReady:
		return "ready"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// canTransition reports whether moving from s to next is legal. The
// allowed edges are:
//
//	Disconnected  → Connected, Closed
//	Connected     → Authenticated, Closed              (auth happy path)
//	Connected     → Ready, Closed                      (no-auth servers)
//	Authenticated → Ready, Closed
//	Ready         → Connected, Closed                  (480 re-auth)
//	Closed        → (nothing; terminal)
//
// Disallowed moves surface as an errInvalidTransition from the caller
// attempting them.
func (s State) canTransition(next State) bool {
	switch s {
	case StateDisconnected:
		return next == StateConnected || next == StateClosed
	case StateConnected:
		return next == StateAuthenticated || next == StateReady || next == StateClosed
	case StateAuthenticated:
		return next == StateReady || next == StateClosed
	case StateReady:
		return next == StateConnected || next == StateClosed
	case StateClosed:
		return false
	default:
		return false
	}
}

// errInvalidTransition is returned when a state change would violate
// canTransition. It wraps the from/to pair for diagnostic clarity;
// callers can errors.Is(err, ErrInvalidState) to detect the class.
type errInvalidTransition struct {
	from, to State
}

func (e errInvalidTransition) Error() string {
	return fmt.Sprintf("nntp: invalid state transition %s → %s", e.from, e.to)
}

func (e errInvalidTransition) Is(target error) bool {
	return target == ErrInvalidState
}
