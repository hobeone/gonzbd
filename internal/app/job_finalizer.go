package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// jobFinalizer handles the queue→history transition when the post-processor
// finishes a job: build the history entry, persist the job payload for retry,
// write history, remove the job from the active queue, and fire the completion
// notification. Extracted from Application (#109 Step 3).
//
// It holds *Application only for read-only, construction-immutable dependencies
// (config, historyRepo, queue, postProcComplete, ctx, log, emit,
// notifyDispatcher); it introduces no lock of its own.
type jobFinalizer struct {
	app *Application
}

func newJobFinalizer(app *Application) *jobFinalizer {
	return &jobFinalizer{app: app}
}

// finalize is called by the post-processor (OnJobDone) when a job is done
// (success or failure). Formerly Application.finalizeJob.
func (f *jobFinalizer) finalize(job *postproc.Job) {
	app := f.app
	entry := buildHistoryEntry(job)
	if err := f.persistAndCommit(app.log, entry, job); err != nil {
		return
	}
	f.fireCompletionNotification(entry)
}

// persistAndCommit saves the job payload to disk, writes the history entry to
// the database, removes the job from the queue, and broadcasts the finalization
// events. Returns a non-nil error if persistence failed and the job was kept in
// the queue for recovery (the error is already logged; callers can simply return).
func (f *jobFinalizer) persistAndCommit(log *slog.Logger, entry history.Entry, job *postproc.Job) error {
	app := f.app
	var adminDir string
	app.config.WithRead(func(c *config.Config) {
		adminDir = c.General.AdminDir
	})
	histJobsDir := filepath.Join(adminDir, "history", "jobs")
	if err := os.MkdirAll(histJobsDir, 0o750); err != nil {
		log.Warn("failed to create history jobs dir", "err", err)
	}
	jobPath := filepath.Join(histJobsDir, job.Queue.ID+".json.gz")
	if err := queue.SaveJob(jobPath, job.Queue); err != nil {
		log.Error("failed to save final job state; keeping job in queue",
			"job", job.Queue.ID, "err", err)
		app.emit(Event{Type: "queue_updated"})
		return err
	}
	if app.historyRepo != nil {
		dbCtx, dbCancel := context.WithTimeout(app.ctx, 5*time.Second)
		if err := app.historyRepo.Add(dbCtx, entry); err != nil {
			log.Error("failed to add history entry; keeping job in queue for recovery",
				"job", job.Queue.ID, "err", err)
			dbCancel()
			_ = os.Remove(jobPath) // clean up the orphaned payload file
			app.emit(Event{Type: "queue_updated"})
			return err
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
	// job_finalized signals a queue→history transition so both stores
	// refresh from a single trigger and reach the new state together.
	app.emit(Event{Type: "job_finalized", NzoID: job.Queue.ID})
	return nil
}

// fireCompletionNotification sends a push notification for a finished job.
// Runs with a bounded context so a slow notification sink can't block the
// postproc worker indefinitely.
func (f *jobFinalizer) fireCompletionNotification(entry history.Entry) {
	app := f.app
	if app.notifyDispatcher == nil {
		return
	}
	evtType := notifier.PostProcessingComplete
	title := "Download completed"
	if entry.Status == "Failed" {
		evtType = notifier.PostProcessingFailed
		title = "Download failed"
	}
	notifyCtx, notifyCancel := context.WithTimeout(app.ctx, 30*time.Second)
	app.notifyDispatcher.Dispatch(notifyCtx, notifier.Event{
		Type:      evtType,
		Title:     title,
		Body:      entry.Name,
		JobName:   entry.Name,
		Timestamp: time.Now(),
	})
	notifyCancel()
}
