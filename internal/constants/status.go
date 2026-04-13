package constants

// Status is the lifecycle state of a job (NzbObject) as exposed to the API
// and rendered in the web UI. Values match the upstream sabnzbd Status class
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

// AllStatuses lists every defined job status in display order. It exists
// primarily for tests and for UI code that iterates known statuses.
var AllStatuses = []Status{
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

// DuplicateStatus describes how a newly added job relates to existing jobs
// (in the active queue or history). Strings match the Python
// DuplicateStatus class verbatim.
type DuplicateStatus string

// Duplicate detection results. See spec §4.5.
const (
	// DuplicateExact: an exact duplicate was found.
	DuplicateExact DuplicateStatus = "Duplicate"
	// DuplicateAlternative: a near match was found (different release group
	// or quality).
	DuplicateAlternative DuplicateStatus = "Duplicate Alternative"
	// SmartDuplicate: a series-aware duplicate was detected (matching
	// title + season + episode via guessit).
	SmartDuplicate DuplicateStatus = "Smart Duplicate"
	// SmartDuplicateAlternative: smart-match with differences.
	SmartDuplicateAlternative DuplicateStatus = "Smart Duplicate Alternative"
	// DuplicateIgnored: a duplicate was found but configuration says ignore it.
	DuplicateIgnored DuplicateStatus = "Duplicate Ignored"
)

// AddNzbFileResult is the outcome of attempting to add an NZB file to the
// queue. Strings match the Python AddNzbFileResult class verbatim because
// they appear in API responses.
type AddNzbFileResult string

// NZB-add outcomes.
const (
	// AddRetry: the file could not be read; the caller may retry.
	AddRetry AddNzbFileResult = "Retry"
	// AddError: the file was rejected (duplicate, malformed, etc.).
	AddError AddNzbFileResult = "Error"
	// AddPrequeueRejected: the pre-queue script rejected the job.
	AddPrequeueRejected AddNzbFileResult = "Pre-queue rejected"
	// AddOK: the job was successfully added.
	AddOK AddNzbFileResult = "OK"
	// AddNoFilesFound: the file is malformed or contains no NZB content.
	AddNoFilesFound AddNzbFileResult = "No files found"
)
