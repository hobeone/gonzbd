package app

import (
	"encoding/json"
	"fmt"
	"slices"
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
	totalBytes := job.Queue.TotalBytes()

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
	completeness := downloadCompleteness(totalBytes, p.FailedBytes())
	downloaded := totalBytes - p.FailedBytes() - p.RemainingBytes()

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
		Completed:    time.Now(),
		Name:         job.Queue.Name,
		NzbName:      job.Queue.Filename,
		Category:     job.Queue.Category,
		Status:       "Completed",
		NzoID:        job.Queue.ID,
		Storage:      job.FinalDir,
		Path:         job.FinalDir,
		DownloadTime: downloadDuration,
		PostprocTime: postprocDuration,
		StageLog:     string(stageLogJSON),
		Bytes:        totalBytes,
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
