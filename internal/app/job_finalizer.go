package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// jobFinalizer handles the queue→history transition when the post-processor
// finishes a job: build the history entry, write history, remove the job from
// the active queue, and fire the completion notification. Extracted from
// Application (#109 Step 3).
//
// It no longer serialises the job. Retry state used to be a gzipped copy of
// the whole Job written here for every job, successful or not, and never
// deleted; it is now the NZB backup plus the per-file progress MoveToHistory
// retains for failed jobs only.
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
// (success or failure).
func (f *jobFinalizer) finalize(job *postproc.Job) {
	app := f.app
	entry := buildHistoryEntry(job)
	if err := f.persistAndCommit(app.log, entry, job); err != nil {
		return
	}
	f.fireCompletionNotification(entry)

	// Apply retention now that history has one more entry in it. Best
	// effort: a job that finished successfully must not be reported as
	// failed because an unrelated old entry could not be swept.
	//
	// Bounded like the other database work in this function, and for a
	// sharper reason: this runs on every job completion, and the query
	// behind it filters on (status, completed) with no covering index —
	// idx_history_archive_completed leads on archive. The backlog only has
	// to be scanned, not deleted, after the first sweep clears it, but an
	// unbounded context would let a slow scan hold up finalization
	// indefinitely.
	pruneCtx, pruneCancel := context.WithTimeout(app.ctx, 30*time.Second)
	defer pruneCancel()
	if _, err := app.PruneHistory(pruneCtx); err != nil {
		app.log.Warn("history retention sweep failed after finalize",
			"job", job.Queue.ID, "err", err)
	}
}

// persistAndCommit writes the history entry to the database, removes the job
// from the queue, and broadcasts the finalization events. Returns a non-nil error if persistence failed and the job was kept in
// the queue for recovery (the error is already logged; callers can simply return).
func (f *jobFinalizer) persistAndCommit(log *slog.Logger, entry history.Entry, job *postproc.Job) error { //nocover: orchestrates queue-to-history transition and error fallbacks
	app := f.app
	if store := app.queue.Store(); store != nil {
		dbCtx, dbCancel := context.WithTimeout(app.ctx, 5*time.Second)
		defer dbCancel()
		if err := store.MoveToHistory(dbCtx, job.Queue, entry); err != nil {
			log.Error("failed to add history entry; keeping job in queue for recovery",
				"job", job.Queue.ID, "err", err)
			app.emit(Event{Type: "queue_updated"})
			return err
		}
	} else if app.historyRepo != nil { //nocover: legacy non-SQLite store fallback
		dbCtx, dbCancel := context.WithTimeout(app.ctx, 5*time.Second)
		defer dbCancel()
		if err := app.historyRepo.Add(dbCtx, entry); err != nil {
			log.Error("failed to add history entry; keeping job in queue for recovery",
				"job", job.Queue.ID, "err", err)
			app.emit(Event{Type: "queue_updated"})
			return err
		}
	}
	if err := app.queue.Remove(job.Queue.ID); err != nil {
		log.Warn("failed to remove job from queue after post-proc", "job", job.Queue.ID, "err", err)
	}
	// The download is over and filed, so its Class A facts and Class B extents
	// describe a queue entry that no longer exists. They are keyed by job ID
	// with no foreign key to the queue, so without a deliberate removal they
	// accumulate one set per job ever downloaded. SQLiteStore.Prune is the
	// backstop for a crash between the Remove above and this call; it is not a
	// reason to skip this one.
	//
	// Deliberately here rather than in enqueuePostProc: post-processing can
	// send a job back for more downloading, and a job that returns to the
	// queue without its extents re-fetches every byte it already has.
	//
	// A FAILED job keeps them, in step with MoveToHistory, which retains that
	// job's job_files row — filename, articles_done, assembled_crc32 — for the
	// same reason. Retrying reuses the job ID, resolves the same filename over
	// the same partial file, and re-fetches only the articles that failed, so
	// the retained facts are what bound FinalizeFile's truncate to the whole
	// file rather than to this run's few articles. Without them durableExtent
	// returns the end offset of the re-fetched articles alone and the rest of
	// the partial is destroyed, silently: neither #342 guard fires, because
	// every article the fact log knows about IS durable.
	//
	// This is the replacement for the max_written column migration 011 carried
	// into history_job_files for exactly this case. That column and the
	// assembler's maxWritten seed are gone, and deriving the bound from the
	// retained facts keeps Class A the single authority (S5) instead of
	// reintroducing a summary that can drift from it.
	//
	// The retention is bounded the same way the job_files one is: these rows
	// are removed with the history entry itself, in history.Delete.
	if entry.Status != string(constants.StatusFailed) {
		delCtx, delCancel := context.WithTimeout(app.ctx, 5*time.Second)
		app.deleteJobDurability(delCtx, job.Queue.ID)
		delCancel()
	}
	app.forgetJobBarrierState(job.Queue.ID)
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
	defer notifyCancel()
	app.notifyDispatcher.Dispatch(notifyCtx, notifier.Event{
		Type:      evtType,
		Title:     title,
		Body:      entry.Name,
		JobName:   entry.Name,
		Timestamp: time.Now(),
	})
}
