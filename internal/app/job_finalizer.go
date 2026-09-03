package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
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
// (config, historyRepo, dispatcher, postProcComplete, ctx, log, emit,
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
			"job", job.Job.ID(), "err", err)
	}
}

// persistAndCommit writes the history entry to the database, removes the job
// from the dispatcher, and broadcasts the finalization events. Returns a non-nil error if persistence failed and the job was kept for recovery (the error is already logged; callers can simply return).
func (f *jobFinalizer) persistAndCommit(log *slog.Logger, entry history.Entry, job *postproc.Job) error { //nocover: orchestrates queue-to-history transition and error fallbacks
	app := f.app
	if app.historyRepo != nil && app.historyRepo.DB() != nil {
		dbCtx, dbCancel := context.WithTimeout(app.ctx, 5*time.Second)
		defer dbCancel()
		if err := app.historyRepo.Add(dbCtx, entry); err != nil {
			log.Error("failed to add history entry; keeping job for recovery",
				"job", job.Job.ID(), "err", err)
			app.emit(Event{Type: "queue_updated"})
			return err
		}
		if entry.Status == string(constants.StatusFailed) && job != nil && job.Job != nil {
			p := job.Job.Progress()
			m, _ := job.Job.Manifest()
			if p != nil && m != nil {
				for fi := range m.NumFiles() {
					lo, hi := m.FileRange(fi)
					artCount := hi - lo
					filename := p.FileFilename(fi)
					complete := 0
					if p.FileComplete(fi) {
						complete = 1
					}
					crc := p.FileAssembledCRC32(fi)
					fetch := int(p.FileFetchPolicy(fi))
					_, _ = app.historyRepo.DB().ExecContext(dbCtx, `
INSERT OR REPLACE INTO history_job_files
  (job_id, file_index, complete, fetch_policy, filename, assembled_crc32, article_count)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
						job.Job.ID(), fi, complete, fetch, filename, crc, artCount)
				}
			}
		}
	}
	if app.dispatcher != nil {
		dbCtx, dbCancel := context.WithTimeout(app.ctx, 5*time.Second)
		defer dbCancel()
		if err := app.dispatcher.Remove(dbCtx, job.Job.ID()); err != nil {
			log.Warn("failed to remove job from dispatcher after post-proc", "job", job.Job.ID(), "err", err)
		}
	}
	manifestPath := filepath.Join(app.config.GetGeneral().AdminDir, "queue", "manifests", job.Job.ID()+".json.gz")
	_ = os.Remove(manifestPath)

	if entry.Status != string(constants.StatusFailed) {
		delCtx, delCancel := context.WithTimeout(app.ctx, 5*time.Second)
		app.deleteJobDurability(delCtx, job.Job.ID())
		delCancel()
	}
	app.forgetJobBarrierState(job.Job.ID())
	select {
	case app.postProcComplete <- PostProcComplete{JobID: job.Job.ID()}:
	default:
	}
	// job_finalized signals a queue→history transition so both stores
	// refresh from a single trigger and reach the new state together.
	app.emit(Event{Type: "job_finalized", NzoID: job.Job.ID()})
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
