package constants

import "time"

// Server penalty durations applied when an NNTP server returns specific
// errors. After a penalty fires, the affected server is held out of the
// dispatch pool for the listed duration; a scheduler event re-enables it
// when the penalty expires.
//
// Values come from the upstream Python implementation
// (sabnzbd/downloader.py) and spec §3.6. Python stores them as floats in
// minutes; Go uses time.Duration so callers do not need to remember the
// unit. The PenaltyVeryShort value is intentionally sub-minute (6s).
const (
	// PenaltyUnknown applies to unrecognized error responses.
	PenaltyUnknown = 3 * time.Minute

	// PenaltyVeryShort applies to bare 400 errors with no diagnostic
	// information. Six seconds (Python: 0.1 minutes).
	PenaltyVeryShort = 6 * time.Second

	// PenaltyShort is the minimal penalty used when no_penalties is set
	// in configuration.
	PenaltyShort = 1 * time.Minute

	// Penalty502 applies to 502 / 503 server-side errors.
	Penalty502 = 5 * time.Minute

	// PenaltyTimeout applies after repeated timeouts on the same server.
	PenaltyTimeout = 10 * time.Minute

	// PenaltyShare applies when a server returns a "maximum connections"
	// 400 error indicating account sharing.
	PenaltyShare = 10 * time.Minute

	// PenaltyTooMany applies when a server reports too many simultaneous
	// connections.
	PenaltyTooMany = 10 * time.Minute

	// PenaltyPerm applies to permanent authentication failures (bad
	// username or password).
	PenaltyPerm = 10 * time.Minute
)

// OptionalDeactivationThreshold is the bad-connection ratio above which an
// optional server is deactivated for a penalty period. Required servers are
// never deactivated, regardless of this ratio. See spec §3.6.
const OptionalDeactivationThreshold = 0.3
