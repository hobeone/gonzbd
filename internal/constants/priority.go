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
// PausedPriority and StopPriority are requests rather than "one step lower"
// priorities, and DefaultPriority is a sentinel meaning "inherit the priority
// configured for this job's category" (IsConcrete).
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

	// ForcePriority schedules ahead of HighPriority work. It obeys the
	// global pause like every other priority: unlike SABnzbd's Force, it
	// does not download while the queue is paused.
	ForcePriority Priority = 2

	// HighPriority schedules ahead of NormalPriority work.
	HighPriority Priority = 1

	// NormalPriority is the default priority for user-added jobs unless a
	// category override applies.
	NormalPriority Priority = 0

	// LowPriority schedules behind NormalPriority work.
	LowPriority Priority = -1

	// PausedPriority is a request rather than a priority a job holds:
	// config.ResolvePriority turns it into the priority the job's category
	// resolves to plus a pause of the job.
	PausedPriority Priority = -2

	// StopPriority is SABnzbd's request to end a job. gonzbd does not
	// implement it: the API refuses it as a requested priority.
	StopPriority Priority = -4

	// DefaultPriority is a sentinel meaning "inherit the priority
	// configured for this job's category". config.ResolvePriority resolves
	// it to a concrete priority before a job holds it.
	DefaultPriority Priority = -100
)

// IsConcrete reports whether p is a priority a job can hold: Repair, Force,
// High, Normal or Low. Paused, Stop, Default and every undefined value are
// not, and Dispatcher.SetPriority refuses them.
func (p Priority) IsConcrete() bool {
	switch p {
	case RepairPriority, ForcePriority, HighPriority, NormalPriority, LowPriority:
		return true
	default:
		return false
	}
}

// String returns a human-readable label for the priority. Unknown numeric
// priorities return their decimal representation prefixed with "Priority(".
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
