package nntp

import (
	"errors"
	"fmt"
)

// Sentinel errors. Callers errors.Is against these to branch on error
// class without string matching (FR-NNTP-1).
var (
	// ErrClosed is returned when Fetch/Stat are called on a Conn whose
	// socket has been closed, either via Close or by a server
	// disconnect.
	ErrClosed = errors.New("nntp: connection closed")

	// ErrInvalidState is returned when a Conn method is called in a
	// state where it is not valid (e.g. Fetch before Authenticate).
	ErrInvalidState = errors.New("nntp: invalid state")

	// ErrAuthRequired is returned when the server responds with 480
	// "authentication required" to a command. Callers should close
	// the Conn and Dial a fresh one — re-authentication mid-session
	// is supported by the protocol but not implemented here because
	// the downloader prefers to cycle connections on auth failure.
	ErrAuthRequired = errors.New("nntp: authentication required")

	// ErrAuthRejected is returned when AUTHINFO USER or AUTHINFO
	// PASS is rejected (481/482). Usually means bad credentials;
	// the dispatcher applies PENALTY_PERM.
	ErrAuthRejected = errors.New("nntp: authentication rejected")

	// ErrNoArticle is returned when the server responds 430/423 —
	// the article was not found. Callers move on to the next server
	// in the try-list.
	ErrNoArticle = errors.New("nntp: article not available")

	// ErrServerUnavailable is returned for 502/503 — the server is
	// refusing service for the time being. Callers apply PENALTY_502.
	ErrServerUnavailable = errors.New("nntp: server unavailable")

	// ErrTransient is returned for generic 4xx responses without a
	// more specific meaning. Callers typically retry on another
	// connection or apply PENALTY_VERYSHORT.
	ErrTransient = errors.New("nntp: transient server error")

	// ErrInvalidCredential is returned when a username or password
	// contains characters that could inject NNTP commands (CR, LF,
	// null) — the credential counterpart to the Message-ID rules internal/nzb enforces.
	ErrInvalidCredential = errors.New("nntp: credential contains illegal control characters")
)

// ServerError wraps an unexpected NNTP status code so callers can
// inspect the wire response when logging or debugging. The sentinel
// returned by errors.Unwrap is one of the Err* constants above when
// the code maps to a known category; otherwise it is bare.
type ServerError struct {
	Code int
	Text string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("nntp: server responded %d %s", e.Code, e.Text)
}

// Unwrap returns the sentinel error for this status code so
// errors.Is(err, ErrServerUnavailable) etc. work correctly.
func (e *ServerError) Unwrap() error {
	return classifyStatus(e.Code)
}

// classifyStatus maps an NNTP status code onto one of the sentinel
// errors so callers can branch without knowing the wire codes.
func classifyStatus(code int) error {
	switch code {
	case 430, 423:
		return ErrNoArticle
	case 480:
		return ErrAuthRequired
	case 481, 482:
		return ErrAuthRejected
	case 502, 503:
		return ErrServerUnavailable
	}
	if code >= 400 && code < 600 {
		return ErrTransient
	}
	return nil
}
