package constants

// Status is the lifecycle state of a job (NzbObject) as exposed to the API
// and rendered in the web UI. Values match the upstream SABnzbd Status class
// (see Python sabnzbd/constants.py) verbatim, because external clients —
// including third-party apps — pattern-match on these strings.
type Status string

// Job statuses. Strings are stable wire values; do not change them.
const (
	// StatusIdle: queue is empty / nothing to do.
	StatusIdle Status = "Idle"
	// StatusQueued: job is waiting for its turn to download or post-process.
	StatusQueued Status = "Queued"
	// StatusGrabbing: fetching an NZB from an external site (URL-add).
	StatusGrabbing Status = "Grabbing"
	// StatusFetching: downloading extra par2 files for repair.
	StatusFetching Status = "Fetching"
	// StatusDownloading: normal article download is in progress.
	StatusDownloading Status = "Downloading"
	// StatusPaused: job is paused.
	StatusPaused Status = "Paused"
	// StatusPropagating: job is delayed waiting for article propagation.
	StatusPropagating Status = "Propagating"
	// StatusChecking: pre-download check (e.g. quick-check) is running.
	StatusChecking Status = "Checking"
	// StatusQuickCheck: post-processing quick-check is running.
	StatusQuickCheck Status = "QuickCheck"
	// StatusVerifying: par2 verification is running.
	StatusVerifying Status = "Verifying"
	// StatusRepairing: par2 repair is running.
	StatusRepairing Status = "Repairing"
	// StatusExtracting: archive extraction (rar/7z) is running.
	StatusExtracting Status = "Extracting"
	// StatusMoving: completed files are being moved to the final location.
	StatusMoving Status = "Moving"
	// StatusRunning: user post-processing script is running.
	StatusRunning Status = "Running"
	// StatusCompleted: job finished successfully (now in history).
	StatusCompleted Status = "Completed"
	// StatusFailed: job finished with a failure (now in history).
	StatusFailed Status = "Failed"
	// StatusDeleted: job has been deleted and is being removed.
	StatusDeleted Status = "Deleted"
)

// AllStatuses returns every Status constant declared above, in declaration
// order. It is the single list callers and tests enumerate over, so that
// adding a status means updating one place rather than every hand-written
// copy of the enum.
//
// This is still written by hand — Go cannot enumerate a const block at
// runtime — but it is not trusted to be complete. TestAllStatuses_Exhaustive
// parses this file's const block and fails if a declared Status is missing
// here, which is what makes downstream exhaustiveness checks (queue's
// TestJobPhase_EveryStatusIsMappedDeliberately) meaningful: without that
// backstop a new status would be absent from this list too, and every loop
// over it would skip the one status nobody had considered.
func AllStatuses() []Status {
	return []Status{
		StatusIdle,
		StatusQueued,
		StatusGrabbing,
		StatusFetching,
		StatusDownloading,
		StatusPaused,
		StatusPropagating,
		StatusChecking,
		StatusQuickCheck,
		StatusVerifying,
		StatusRepairing,
		StatusExtracting,
		StatusMoving,
		StatusRunning,
		StatusCompleted,
		StatusFailed,
		StatusDeleted,
	}
}
