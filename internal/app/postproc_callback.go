package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// onPostProcDone is the OnJobDone callback registered with the PostProcessor.
// It runs once per job when the post-processing pipeline finishes (success or
// failure). It persists the final job state to disk, writes a history entry,
// removes the job from the queue, and broadcasts the job_finalized event.
//
// Extracted from the inline closure in New() so it can be unit-tested
// independently against a fake history repository and notifier.
func (app *Application) onPostProcDone(job *postproc.Job) {
	log := app.log

	stageLogJSON, _ := json.Marshal(job.StageLog)
	var downloadDuration int64
	if !job.Queue.DownloadStarted.IsZero() && !job.Queue.DownloadFinished.IsZero() {
		downloadDuration = int64(job.Queue.DownloadFinished.Sub(job.Queue.DownloadStarted).Seconds())
	}
	if downloadDuration == 0 {
		downloadDuration = 1
	}

	// Compute post-processing duration from stage log.
	var postprocDuration int64
	for _, se := range job.StageLog {
		postprocDuration += int64(se.Elapsed.Seconds())
	}

	// Compute article-level completion stats.
	var totalArticles, doneArticles int
	for fi := range job.Queue.Files {
		for ai := range job.Queue.Files[fi].Articles {
			totalArticles++
			a := &job.Queue.Files[fi].Articles[ai]
			if a.Done {
				doneArticles++
			}
		}
	}
	var completeness int64
	if totalArticles > 0 {
		completeness = int64((float64(doneArticles) / float64(totalArticles)) * 100)
	}
	downloaded := job.Queue.TotalBytes - job.Queue.FailedBytes - job.Queue.RemainingBytes

	// Build per-server download stats string, sorted for deterministic output.
	serverNames := make([]string, 0, len(job.Queue.ServerStats))
	for s := range job.Queue.ServerStats {
		serverNames = append(serverNames, s)
	}
	sort.Strings(serverNames)
	serverStatsParts := make([]string, 0, len(serverNames))
	for _, s := range serverNames {
		b := job.Queue.ServerStats[s]
		serverStatsParts = append(serverStatsParts, fmt.Sprintf("%s=%.1f MB", s, float64(b)/(1024*1024)))
	}
	serverStats := strings.Join(serverStatsParts, ", ")

	repairSummary := "No repair needed"
	for _, entry := range job.StageLog {
		if entry.Stage == "repair" {
			if entry.Err != nil {
				repairSummary = fmt.Sprintf("Repair failed: %v", entry.Err)
			} else {
				repairSummary = "Repair OK"
				if len(entry.Lines) > 0 {
					repairSummary = entry.Lines[0]
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
		Bytes:        job.Queue.TotalBytes,
		Downloaded:   downloaded,
		Completeness: completeness,
		TimeAdded:    job.Queue.Added,
		URLInfo:      repairSummary,
		Meta:         serverStats,
	}
	if job.ParError || job.UnpackError || job.FailMsg != "" {
		entry.Status = "Failed"
		entry.FailMessage = job.FailMsg
		entry.Path = job.DownloadDir
	}

	// Save the full job payload first — RetryHistoryJob needs this file to
	// re-enqueue. If this fails, don't commit to the DB or remove from queue.
	histJobsDir := filepath.Join(app.cfg.AdminDir, "history", "jobs")
	if err := os.MkdirAll(histJobsDir, 0o750); err != nil {
		log.Warn("failed to create history jobs dir", "err", err)
	}
	jobPath := filepath.Join(histJobsDir, job.Queue.ID+".json.gz")
	if err := queue.SaveJob(jobPath, job.Queue); err != nil {
		log.Error("failed to save final job state; keeping job in queue",
			"job", job.Queue.ID, "err", err)
		app.emitter.Broadcast(Event{Type: "queue_updated"})
		return
	}
	if app.historyRepo != nil {
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := app.historyRepo.Add(dbCtx, entry); err != nil {
			log.Error("failed to add history entry; keeping job in queue for recovery",
				"job", job.Queue.ID, "err", err)
			dbCancel()
			_ = os.Remove(jobPath) // clean up orphaned payload
			app.emitter.Broadcast(Event{Type: "queue_updated"})
			return
		}
		dbCancel()
	}
	if err := app.queue.Remove(job.Queue.ID); err != nil {
		log.Warn("failed to remove job from queue after post-proc", "job", job.Queue.ID, "err", err)
	}
	select {
	case app.postProcComplete <- PostProcComplete{JobID: job.Queue.ID}:
	default:
	}
	// job_finalized signals a queue→history transition: both stores subscribe
	// to it and refresh together from a single trigger.
	app.emitter.Broadcast(Event{Type: "job_finalized", NzoID: job.Queue.ID})

	// Fire notification with a bounded timeout so a misbehaving sink can't
	// hang the postproc worker.
	if app.notifyDispatcher != nil {
		evtType := notifier.PostProcessingComplete
		title := "Download completed"
		if entry.Status == "Failed" {
			evtType = notifier.PostProcessingFailed
			title = "Download failed"
		}
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
		app.notifyDispatcher.Dispatch(notifyCtx, notifier.Event{
			Type:      evtType,
			Title:     title,
			Body:      entry.Name,
			JobName:   entry.Name,
			Timestamp: time.Now(),
		})
		notifyCancel()
	}
}
