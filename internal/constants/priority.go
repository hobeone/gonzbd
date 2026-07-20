// Package constants holds the static, plan-mandated constants of the gonzbd
// Go reimplementation: priorities, statuses, penalty durations, and buffer
// limits. Values mirror the Python reference implementation
// (sabnzbd/constants.py) and the functional specification.
//
// Constants are grouped by concern in separate files within this package:
//
//   - priority.go  — job priority levels
//   - status.go    — job status, duplicate status, add-NZB result
//   - penalty.go   — server penalty durations
//   - limits.go    — buffer sizes, cache limits, queue limits, byte units
//
// Nothing in this package depends on any other internal package; it may be
// imported freely.
package constants

import "strconv"

// Priority is the scheduling priority of a job in the download queue. Higher
// numeric values are scheduled before lower ones. Values are not contiguous —
// PausedPriority and StopPriority are sentinels used internally rather than
// "one step lower" priorities, and DefaultPriority is a sentinel meaning
// "inherit the priority configured for this job's category".
//
// Use the named constants below; do not construct Priority values from raw
// integers in callers.
type Priority int8

// Job priority levels. Numeric values are fixed by spec §4.2 and the upstream
// Python implementation; do not renumber. Iota is intentionally not used —
// the gap between PausedPriority (-2) and StopPriority (-4), and the
// out-of-range DefaultPriority (-100), would make iota-based numbering
// brittle and error-prone.
const (
	// RepairPriority is reserved for internal par2-repair jobs that must
	// run ahead of normal user work.
	RepairPriority Priority = 3

	// ForcePriority bypasses the global pause state and downloads
	// immediately.
	ForcePriority Priority = 2

	// HighPriority schedules ahead of NormalPriority work.
	HighPriority Priority = 1

	// NormalPriority is the default priority for user-added jobs unless a
	// category override applies.
	NormalPriority Priority = 0

	// LowPriority schedules behind NormalPriority work.
	LowPriority Priority = -1

	// PausedPriority marks a job as paused; the queue will not dispatch
	// articles for it until it is unpaused.
	PausedPriority Priority = -2

	// StopPriority is an internal sentinel used to halt processing of a
	// job (used during shutdown / removal flows).
	StopPriority Priority = -4

	// DefaultPriority is a sentinel meaning "inherit the priority
	// configured for this job's category". It must be resolved to a
	// concrete priority before the job is enqueued.
	DefaultPriority Priority = -100
)

// IsValid reports whether p is one of the defined Priority sentinels.
func (p Priority) IsValid() bool {
	switch p {
	case RepairPriority, ForcePriority, HighPriority, NormalPriority, LowPriority, PausedPriority, StopPriority, DefaultPriority:
		return true
	default:
		return false
	}
}

// String returns a human-readable label for the priority. Unknown numeric
// priorities (including the sentinel DefaultPriority that should have been
// resolved) return their decimal representation prefixed with "Priority(".
func (p Priority) String() string {
	switch p {
	case RepairPriority:
		return "Repair"
	case ForcePriority:
		return "Force"
	case HighPriority:
		return "High"
	case NormalPriority:
		return "Normal"
	case LowPriority:
		return "Low"
	case PausedPriority:
		return "Paused"
	case StopPriority:
		return "Stop"
	case DefaultPriority:
		return "Default"
	default:
		return "Priority(" + strconv.Itoa(int(p)) + ")"
	}
}
