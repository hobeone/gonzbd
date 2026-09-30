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

// withFailureNotes returns ppJob, or when notes or extra is non-empty a shallow
// copy of it whose StageLog ends with one warning entry listing them: each
// note as a failure reported and not applied, then each extra line as given.
// The copy leaves the post-processor's own ppJob unwritten, since its History
// snapshots it.
func withFailureNotes(ppJob *postproc.Job, notes []string, extra ...string) *postproc.Job {
	if len(notes) == 0 && len(extra) == 0 {
		return ppJob
	}
	lines := make([]string, 0, len(notes)+len(extra))
	for _, n := range notes {
		lines = append(lines, "Warning: failure reported during post-processing, not applied: "+n)
	}
	for _, e := range extra {
		lines = append(lines, "Warning: "+e)
	}
	cp := *ppJob
	cp.StageLog = append(slices.Clone(ppJob.StageLog), postproc.StageLogEntry{
		Stage:   "warnings",
		Started: time.Now(),
		Lines:   lines,
	})
	return &cp
}

// buildHistoryEntry is a pure function that computes the history.Entry for a
// completed post-processing job. It reads only from ppJob and produces no side
// effects, making it independently testable.
func buildHistoryEntry(ppJob *postproc.Job) history.Entry {
	stageLogJSON, _ := json.Marshal(ppJob.StageLog)

	var expectedBytes, downloaded, completeness int64
	var downloadDuration int64
	var serverStatsParts []string

	p := ppJob.Job.Progress()

	if p != nil {
		expected, _, failed := p.ProgressFigures()
		expectedBytes = expected
		if !p.DownloadStarted().IsZero() && !p.DownloadFinished().IsZero() {
			downloadDuration = int64(p.DownloadFinished().Sub(p.DownloadStarted()).Seconds())
		}
		if downloadDuration == 0 {
			downloadDuration = 1
		}

		completeness = downloadCompleteness(expectedBytes, failed)
		downloaded = DurableBytesOf(p)

		stats := p.ServerStats()
		serverNames := make([]string, 0, len(stats))
		for s := range stats {
			serverNames = append(serverNames, s)
		}
		slices.Sort(serverNames)
		for _, s := range serverNames {
			b := stats[s]
			serverStatsParts = append(serverStatsParts, fmt.Sprintf("%s=%.1f MB", s, float64(b)/(1024*1024)))
		}
	}

	var postprocDuration int64
	for _, se := range ppJob.StageLog {
		postprocDuration += int64(se.Elapsed.Seconds())
	}

	repairSummary := "No repair needed"
	for _, se := range ppJob.StageLog {
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
		Name:         ppJob.Job.Name(),
		NzbName:      ppJob.Filename,
		NZBBackup:    ppJob.NZBBackup,
		Category:     ppJob.Category,
		PP:           strconv.Itoa(ppJob.PP),
		Script:       ppJob.Script,
		Password:     ppJob.Password,
		Status:       "Completed",
		NzoID:        ppJob.Job.ID(),
		Storage:      ppJob.FinalDir,
		Path:         ppJob.FinalDir,
		DownloadTime: downloadDuration,
		PostprocTime: postprocDuration,
		StageLog:     string(stageLogJSON),
		Bytes:        expectedBytes,
		Downloaded:   downloaded,
		Completeness: completeness,
		TimeAdded:    ppJob.Job.Added(),
		URLInfo:      repairSummary,
		Meta:         strings.Join(serverStatsParts, ", "),
	}
	if ppJob.ParError || ppJob.UnpackError || ppJob.FailMsg != "" {
		entry.Status = "Failed"
		entry.FailMessage = ppJob.FailMsg
		entry.Path = ppJob.DownloadDir
	}
	return entry
}
