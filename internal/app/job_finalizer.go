package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
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
// It holds *Application for read-only, construction-immutable dependencies
// (config, historyRepo, dispatcher, postProcComplete, ctx, log, emit,
// notifyDispatcher).
type jobFinalizer struct {
	app *Application
}

func newJobFinalizer(app *Application) *jobFinalizer {
	return &jobFinalizer{
		app: app,
	}
}

// finalize is called by the post-processor (OnJobDone) when a job is done
// (success or failure).
func (f *jobFinalizer) finalize(ppJob *postproc.Job) {
	app := f.app
	entry := buildHistoryEntry(ppJob)
	if err := f.persistAndCommit(app.log, entry, ppJob); err != nil {
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
			"job", ppJob.Job.ID(), "err", err)
	}
}

// persistAndCommit writes the history entry to the database, removes the job
// from the dispatcher, and broadcasts the finalization events. Registry and
// filesystem teardown (checkpointer prune, dispatcher removal, manifest
// unlinking, and barrier state reset) is always attempted
// regardless of history persistence success. If dispatcher.Remove returns an
// error, it is retried once. If the retry also fails, the error is logged, a
// note is surfaced on the dispatcher row via SetOperationalError, and a
// "queue_updated" event is emitted while the job remains registered for retry
// or restart handling.
// Sub-budgets within persistAndCommit are strictly partitioned against
// starvation:
//   - History write & files loop: 4s dbCtx, derived from
//     context.WithoutCancel(app.ctx).
//   - Dispatcher removal: 3s removeCtx (with an additional 3s retryCtx on
//     failure if occupyCtx is unexpired), derived from occupyCtx to retain the
//     occupancy lease token for bypass in Dispatcher.Remove.
//   - Durability check & delete: 3s delCtx, derived from
//     context.WithoutCancel(app.ctx).
//
// Within Occupy, the 12s finalCtx bounds the history write and both removal
// attempts (4s DB + 3s initial Remove + 3s retry Remove = 10s, leaving a 2s
// margin before Occupy expires). Sequentially across all phases including
// durability cleanup, the maximum execution bound is 13s (10s Occupy + 3s
// delCtx), which fits within the 15s shutdown step timeout (stepTimeout)
// under waitBounded when terminating post-processing.
//
// Because dbCtx, removeCtx, and delCtx are independently derived, a slow SQLite
// write cannot starve dispatcher removal or durability cleanup. Prune operates
// in memory. removeManifestIn unlinks the queue manifest on the filesystem
// hosting AdminDir, and takes no context: docs/durability-contract.md notes
// that a remote NFS/SMB mount can stall such a call, and nothing here bounds
// it. Note that the enclosing finalize method
// also executes completion notifications and asynchronous history pruning
// under its own 30s context outside of persistAndCommit.
//
// Returns a non-nil error if persistence failed (the error is already logged;
// callers can simply return).
func (f *jobFinalizer) persistAndCommit(log *slog.Logger, entry history.Entry, ppJob *postproc.Job) error {
	app := f.app
	if app.dispatcher != nil && ppJob != nil && ppJob.Job != nil {
		_ = app.dispatcher.Cancel(ppJob.Job.ID())
		_ = app.dispatcher.Yielded(ppJob.Job.ID())
	}

	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(app.ctx), 12*time.Second)
	defer finalCancel()

	runCommit := func(occupyCtx context.Context) error {
		mdir := manifestDir(app.config.GetGeneral().AdminDir)

		var persistErr error
		if app.historyRepo != nil && app.historyRepo.DB() != nil {
			// Gathered before the write, because Add stores the entry and this
			// progress in one transaction, and its doc says what rests on that.
			var files []history.FileProgress
			if entry.Status == string(constants.StatusFailed) && ppJob != nil && ppJob.Job != nil {
				files = retainedProgressFor(ppJob.Job, mdir, log)
			}
			// The write's deadline starts AFTER that gather, and the order is
			// load-bearing. retainedProgressFor reads and inflates a manifest
			// from disk, which on a wedged mount is unbounded; a deadline
			// started before it would be spent by the time Add ran. Add failing
			// is not recoverable here: the teardown below removes the queue row,
			// and where that removal succeeds the reclaim rule sees neither a
			// queue row nor a FAILED entry and takes durable_runs with it
			// (internal/durability/reclaim.go ruleStatement), leaving a failed
			// job with nothing to retry from. A slow read costs its own
			// progress, which Add tolerates; it must not cost the entry.
			dbCtx, dbCancel := context.WithTimeout(context.WithoutCancel(app.ctx), 4*time.Second)
			defer dbCancel()
			if err := app.historyRepo.Add(dbCtx, entry, files); err != nil {
				log.Error("failed to add history entry; registry and filesystem teardown completed but history entry failed to persist",
					"job", ppJob.Job.ID(), "err", err)
				persistErr = err
			}
		}
		if app.checkpointer != nil && ppJob != nil && ppJob.Job != nil {
			app.checkpointer.Prune(ppJob.Job.ID())
		}
		// Not fatal, unlike the reconcile path's version: this job IS in
		// history, so the next startup's dropJobAlreadyInHistory removes the
		// queue row. reclaim below leaves the manifest and rows of a job the
		// dispatcher still holds, which is what keeps that row loadable until
		// then (#376: appResidency.hydrate fails without the manifest).
		if app.dispatcher != nil && ppJob != nil && ppJob.Job != nil {
			jobID := ppJob.Job.ID()
			removeCtx, removeCancel := context.WithTimeout(occupyCtx, 3*time.Second)
			err := app.dispatcher.Remove(removeCtx, jobID)
			removeCancel()
			if err != nil && occupyCtx.Err() == nil {
				retryCtx, retryCancel := context.WithTimeout(occupyCtx, 3*time.Second)
				err = app.dispatcher.Remove(retryCtx, jobID)
				retryCancel()
			}
			if err != nil {
				log.Error("failed to remove job from dispatcher after post-proc retry; job remains in queue and history until restart, with its manifest and durability rows left in place for the next startup to reconcile",
					"job", jobID, "err", err)
				_ = app.dispatcher.SetOperationalError(jobID, "failed to remove finalized job from queue: "+err.Error())
				app.emit(Event{Type: "queue_updated"})
			}
		}

		// Unconditional: the rule decides from the queue and history as they
		// now are, so a job still queued keeps everything, a FAILED entry
		// keeps its durable_runs for a retry, and a persist that failed
		// against an existing FAILED entry keeps them for that entry.
		delCtx, delCancel := context.WithTimeout(context.WithoutCancel(app.ctx), 3*time.Second)
		defer delCancel()
		if ppJob != nil && ppJob.Job != nil {
			app.reclaim(delCtx, ppJob.Job.ID())
		}
		app.forgetJobBarrierState(ppJob.Job.ID())
		if persistErr != nil {
			app.emit(Event{Type: "queue_updated"})
			return persistErr
		}
		select {
		case app.postProcComplete <- PostProcComplete{JobID: ppJob.Job.ID()}:
		default:
		}
		// job_finalized signals a queue→history transition so both stores
		// refresh from a single trigger and reach the new state together.
		app.emit(Event{Type: "job_finalized", NzoID: ppJob.Job.ID()})
		return nil
	}

	if app.dispatcher != nil && ppJob != nil && ppJob.Job != nil {
		var runErr error
		if err := app.dispatcher.Occupy(finalCtx, ppJob.Job.ID(), func(occupyCtx context.Context) {
			runErr = runCommit(occupyCtx)
		}); err != nil {
			// If Occupy fails (e.g. ErrNotFound if already removed), fallback to running without occupy wrapper.
			log.Warn("occupy failed during finalize; proceeding with fallback teardown", "job", ppJob.Job.ID(), "err", err)
			return runCommit(finalCtx)
		}
		return runErr
	}
	return runCommit(finalCtx)
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

// retainedProgressFor renders the per-file progress a failed job's history entry
// carries, so a retry can resume its files instead of re-fetching them.
//
// The manifest is what turns a file index into an article count, and it is read
// from disk when the job is no longer resident. Returning nil is a real answer
// rather than a failure: without a manifest there is no article count to state,
// and a row asserting the wrong one is rejected wholesale by
// retainedMatchesManifest at retry time — costing the retry every file's
// progress instead of one file's.
func retainedProgressFor(j *job.Job, mdir string, log *slog.Logger) []history.FileProgress {
	p := j.Progress()
	m, mErr := j.Manifest()
	if mErr != nil && errors.Is(mErr, job.ErrNotResident) {
		if f, oErr := openManifestIn(mdir, j.ID()); oErr == nil {
			if diskM, dErr := decodeManifest(f); dErr == nil {
				m, mErr = diskM, nil
			}
		}
	}
	if mErr != nil {
		log.Error("failed to load manifest for failed job files; retained file progress not recorded",
			"job", j.ID(), "err", mErr)
		return nil
	}
	if p == nil || m == nil {
		return nil
	}
	files := make([]history.FileProgress, 0, m.NumFiles())
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		files = append(files, history.FileProgress{
			FileIndex:      fi,
			Complete:       p.FileComplete(fi),
			FetchPolicy:    uint8(p.FileFetchPolicy(fi)),
			Filename:       p.FileFilename(fi),
			AssembledCRC32: p.FileAssembledCRC32(fi),
			ArticleCount:   hi - lo,
		})
	}
	return files
}
