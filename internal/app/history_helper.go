package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// downloadCompleteness returns the percentage (0-100) of a job's bytes that
// were successfully retrieved. failedBytes is the number of bytes that could
// not be fetched from any configured server. Returns 0 when totalBytes is
// non-positive (nothing downloaded) or when every byte failed.
func downloadCompleteness(totalBytes, failedBytes int64) int64 {
	if totalBytes <= 0 || failedBytes >= totalBytes {
		return 0
	}
	if failedBytes < 0 {
		failedBytes = 0
	}
	return int64(float64(totalBytes-failedBytes) / float64(totalBytes) * 100)
}

// buildHistoryEntry is a pure function that computes the history.Entry for a
// completed post-processing job. It reads only from job and produces no side
// effects, making it independently testable.
func buildHistoryEntry(job *postproc.Job) history.Entry {
	stageLogJSON, _ := json.Marshal(job.StageLog)

	p := job.Queue.Progress()
	// expectedBytes shares its walk and predicate with RemainingBytes and
	// FailedBytes (both exclude Deferred files), so completeness and the
	// downloaded identity below combine figures from the same universe —
	// see ExpectedBytes's doc comment for why pairing either with
	// job.Queue.TotalBytes() instead would misreport them for a job with
	// deferred par2 volumes.
	//
	// entry.Bytes also uses expectedBytes rather than TotalBytes(), even
	// though Bytes is not part of the downloaded identity: the UI renders
	// "Downloaded of Bytes (Bytes-Downloaded failed)" as one sentence (see
	// HistoryRow.svelte), so Bytes has to describe the same file set as
	// Downloaded or that arithmetic reports bytes as failed that were only
	// ever deferred, never dispatched, never failed. A job finalized while
	// maybeReleaseRecoveryVolumes has left a deferred volume in the
	// manifest (see the unreadable-manifest case documented at app.go:1017,
	// where verification cannot run and the volume stays deferred) is
	// exactly the case this would otherwise misreport.
	expectedBytes := p.ExpectedBytes()

	var downloadDuration int64
	if !p.DownloadStarted().IsZero() && !p.DownloadFinished().IsZero() {
		downloadDuration = int64(p.DownloadFinished().Sub(p.DownloadStarted()).Seconds())
	}
	if downloadDuration == 0 {
		downloadDuration = 1
	}

	var postprocDuration int64
	for _, se := range job.StageLog {
		postprocDuration += int64(se.Elapsed.Seconds())
	}

	// Download health: byte-based rather than article-based because a failed
	// article is marked both Done and Failed (Done = resolved, not succeeded).
	completeness := downloadCompleteness(expectedBytes, p.FailedBytes())
	downloaded := expectedBytes - p.FailedBytes() - p.RemainingBytes()

	// Sort server names for deterministic output in history entries.
	stats := p.ServerStats()
	serverNames := make([]string, 0, len(stats))
	for s := range stats {
		serverNames = append(serverNames, s)
	}
	slices.Sort(serverNames)
	serverStatsParts := make([]string, 0, len(serverNames))
	for _, s := range serverNames {
		b := stats[s]
		serverStatsParts = append(serverStatsParts, fmt.Sprintf("%s=%.1f MB", s, float64(b)/(1024*1024)))
	}

	repairSummary := "No repair needed"
	for _, se := range job.StageLog {
		if se.Stage == "repair" {
			if se.Err != nil {
				repairSummary = fmt.Sprintf("Repair failed: %v", se.Err)
			} else {
				repairSummary = "Repair OK"
				if len(se.Lines) > 0 {
					repairSummary = se.Lines[0]
				}
			}
			break
		}
	}

	entry := history.Entry{
		Completed: time.Now(),
		Name:      job.Queue.Name,
		NzbName:   job.Queue.Filename,
		NZBBackup: job.Queue.NZBBackup,
		Category:  job.Queue.Category,
		// The pp/script/password columns have existed since the initial
		// migration but were never populated. Retry rebuilds a job from its
		// NZB, which carries none of these, so without them a retried job
		// silently drops to its category defaults — and an encrypted
		// archive fails to unpack for want of a password we had.
		PP:           strconv.Itoa(job.Queue.PP),
		Script:       job.Queue.Script,
		Password:     job.Queue.Password,
		Status:       "Completed",
		NzoID:        job.Queue.ID,
		Storage:      job.FinalDir,
		Path:         job.FinalDir,
		DownloadTime: downloadDuration,
		PostprocTime: postprocDuration,
		StageLog:     string(stageLogJSON),
		Bytes:        expectedBytes,
		Downloaded:   downloaded,
		Completeness: completeness,
		TimeAdded:    job.Queue.Added,
		URLInfo:      repairSummary,
		Meta:         strings.Join(serverStatsParts, ", "),
	}
	if job.ParError || job.UnpackError || job.FailMsg != "" {
		entry.Status = "Failed"
		entry.FailMessage = job.FailMsg
		entry.Path = job.DownloadDir
	}
	return entry
}
