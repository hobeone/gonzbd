package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
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

// finalizeTransitionWait caps how long a finalizer waits for another actor to
// release its job. On expiry it proceeds without the lock rather than hold a
// post-processing worker behind a slow holder.
const finalizeTransitionWait = 5 * time.Second

// errFinalizedJobRemoved reports a finalization abandoned because a RemoveJob
// took the job first (jobTransitions.markRemoved).
var errFinalizedJobRemoved = errors.New("the job was removed before it could be finalized")

// errFinalizedJobSuperseded reports a finalization abandoned because another
// instance of the job is registered under its ID: a retry owns the ID now.
var errFinalizedJobSuperseded = errors.New("a later instance of the job holds its ID")

func newJobFinalizer(app *Application) *jobFinalizer {
	return &jobFinalizer{
		app: app,
	}
}

// cancelled is called by the post-processor (OnJobCancelled) for a job its
// Cancel took out of the queue or interrupted, once no stage runs for it, and
// by enqueuePostProc for a job its hand-over refused as removed. The
// job is not finalized: it releases the job's launch claim, which
// dispatcher.Remove waits on and which persistAndCommit and Shutdown release
// on their own paths, and ends the job's post-processing admission.
//
// For a queued job this runs inside RemoveJob, which holds the job's
// transition lock. None of the calls below takes it: the one path back into
// this package is sched's cancel calling appWorkers.Abort, which takes app.mu,
// the admission release takes only postProcAdmissions.mu, and none of these is
// among the lock's sites (TestJobTransitions_LockSites).
func (f *jobFinalizer) cancelled(ppJob *postproc.Job) {
	app := f.app
	defer app.postProcAdmissions.release(ppJob.Job)
	if app.dispatcher == nil {
		return
	}
	// Cancel first, so the tick cannot relaunch the job between the yield and
	// the cancel intent. Both calls carry this instance, so a callback for a
	// removed instance leaves alone a retry registered since under the same ID.
	id := ppJob.Job.ID()
	warnUnlessGone(app.log, "postproc cancel: cancelling the job failed", id,
		app.dispatcher.CancelJob(ppJob.Job))
	warnUnlessGone(app.log, "postproc cancel: releasing the job's launch claim failed", id,
		app.dispatcher.YieldedJob(ppJob.Job))
}

// warnUnlessGone logs err at Warn unless it is nil or dispatch.ErrNotFound,
// which from an instance-bound dispatcher call means the instance is no
// longer registered and its launch claim went with it.
func warnUnlessGone(log *slog.Logger, msg, id string, err error) {
	if err == nil || errors.Is(err, dispatch.ErrNotFound) {
		return
	}
	log.Warn(msg, "job", id, "err", err)
}

// finalize is called by the post-processor (OnJobDone) when a job is done
// (success or failure).
//
// It ends the job's post-processing admission on return. The failure reasons
// the admission noted, those that did not become the run's FailMsg, are added
// to the history entry's stage log as warnings, and so is heldVolumesRetryNote
// for a job it will retry. They do not change the entry's status, which
// reflects what the stages did (postProcAdmissions).
func (f *jobFinalizer) finalize(ppJob *postproc.Job) {
	app := f.app
	defer app.postProcAdmissions.release(ppJob.Job)
	if app.finalizeHook != nil {
		app.finalizeHook(ppJob)
	}
	notes := app.postProcAdmissions.notes(ppJob.Job)
	// Decided before persistAndCommit tears the job down. A ParError files the
	// entry Failed (buildHistoryEntry), which is what a retry requires of it.
	retry := heldVolumesMightRepair(ppJob)
	var extra []string
	if retry {
		extra = append(extra, heldVolumesRetryNote)
	}
	entry := buildHistoryEntry(withFailureNotes(ppJob, notes, extra...))
	if err := f.persistAndCommit(app.log, entry, ppJob); err != nil {
		return
	}
	// After persistAndCommit has returned, so its transition lock is released
	// and the retry it filed the entry for can claim the ID.
	if !retry || !f.retryWithHeldVolumes(ppJob.Job.ID()) {
		f.fireCompletionNotification(entry)
	}

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

// heldVolumesMightRepair reports whether a finished run failed par2 while its
// job still held recovery volumes back (Job.HasDeferredPar2), which the
// finalizer then retries the job to fetch (#651). The Assessing verdict holds
// them when nothing delivered matched the par2 index (outcomeUnknown): a
// Layout B post, whose extracted_repair then has nothing to repair a damaged
// extraction with, or an obfuscated file damaged inside its first 16 KB, which
// repair then has nothing for either.
//
// ParError also covers failures the volumes cannot fix, such as a containment
// violation. Those cost one retry. The retry does not trigger another: it
// releases every volume its rebuilt job holds before seeding job_files, which
// hydration restores the policy from, and nothing else sets a volume to
// FetchIfNeeded. The policy field is written in four places
// (`git grep -nE '\.Fetch\s*=[^=]' -- '*.go' ':!*_test.go'` returns 4 lines):
// the policy setter, the release (to FetchAlways), the discard (to
// FetchNever), and construction, which starts every file at FetchAlways. The
// setter is called from ingest, with FetchIfNeeded, and from hydration
// (`git grep -nE 'SetFileFetchPolicy\(|RestoreFetchPolicy\(' -- '*.go' ':!*_test.go'`
// returns 5 lines: those two calls, the two declarations, and the restore
// delegating to the setter). So its failure is filed as final. A user's
// retry of that entry is rebuilt by ingest again, and so gets one automatic
// retry of its own.
func heldVolumesMightRepair(ppJob *postproc.Job) bool {
	return ppJob.ParError && ppJob.Job.HasDeferredPar2()
}

// heldVolumesRetryNote is the warning a job heldVolumesMightRepair selects
// carries in its history entry. The entry is deleted if the automatic retry
// starts, so the note is read only beside a retry that could not start, such
// as one a shutdown catches: by then app.ctx is cancelled and the assembler
// stopped (Application.Shutdown), so retryHistoryJob fails.
//
// What the note tells the user to do heals the job. A retry rebuilds it
// through BuildIngestJob, which holds the recovery volumes back again
// (`git grep -n 'SetFileFetchPolicy[(]fi, job\.FetchIfNeeded)' -- '*.go' ':!*_test.go'`
// returns 1 line, internal/app/ingest.go:151, while downloads.on_demand_par2
// is on). Its post-processing then fails par2 with those volumes held, and
// this finalizer retries it with them released. With on_demand_par2 off the
// rebuilt job fetches every volume at once.
const heldVolumesRetryNote = "par2 repair failed while the recovery volumes were held back; " +
	"a retry of this job fetches them"

// retryWithHeldVolumes retries the job filed Failed under jobID with every
// recovery volume the rebuilt job holds released, and reports whether the
// retry was queued. One that could not be is logged with the reason, and the
// failure stands as filed.
func (f *jobFinalizer) retryWithHeldVolumes(jobID string) bool {
	app := f.app
	err := app.retryHistoryJob(app.ctx, jobID, func(j *job.Job) error {
		_, err := app.releaseRecoveryVolumes(j,
			"post-processing failed par2 repair while the recovery volumes were held back")
		return err
	})
	if err != nil {
		app.log.Warn("finalize: par2 repair failed and could not retry the job with its held recovery volumes; "+
			"it stays failed, and you can retry it to fetch its recovery volumes",
			"job", jobID, "err", err)
		return false
	}
	app.log.Info("finalize: par2 repair failed while recovery volumes were held back; retrying the job to fetch them",
		"job", jobID)
	return true
}

// persistAndCommit writes the history entry to the database, removes the job
// from the dispatcher, and broadcasts the finalization events. Registry and
// filesystem teardown (checkpointer prune, dispatcher removal, manifest
// unlinking, and barrier state reset) is attempted regardless of history
// persistence success, and skipped with the rest when a RemoveJob took the job
// first (errFinalizedJobRemoved). If dispatcher.RemoveJob returns an
// error other than ErrNotFound, it is retried once. If the retry also fails, the error is logged, a
// note is surfaced on the dispatcher row via SetOperationalError, and a
// "queue_updated" event is emitted while the job remains registered for retry
// or restart handling.
// Sub-budgets within persistAndCommit are strictly partitioned against
// starvation:
//   - History write & files loop: 4s dbCtx, derived from
//     context.WithoutCancel(app.ctx).
//   - Dispatcher removal: 3s removeCtx (with an additional 3s retryCtx on
//     failure if occupyCtx is unexpired), derived from occupyCtx to retain the
//     occupancy lease token for bypass in Dispatcher.RemoveJob.
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
// The job's transition lock is waited for before finalCtx starts, for at most
// finalizeTransitionWait and only until app.ctx ends, so it is outside that
// 13s: Shutdown cancels app.ctx before it stops post-processing.
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
// Returns a non-nil error if persistence failed, errFinalizedJobRemoved if
// a RemoveJob took this job instance before it was committed, or
// errFinalizedJobSuperseded if a later instance holds the job's ID; the last
// two skip the teardown too. Each is already logged; callers can simply
// return.
func (f *jobFinalizer) persistAndCommit(log *slog.Logger, entry history.Entry, ppJob *postproc.Job) error {
	app := f.app
	if app.dispatcher != nil {
		id := ppJob.Job.ID()
		warnUnlessGone(log, "finalize: cancelling the job failed", id,
			app.dispatcher.CancelJob(ppJob.Job))
		warnUnlessGone(log, "finalize: releasing the job's launch claim failed", id,
			app.dispatcher.YieldedJob(ppJob.Job))
	}
	// Taken after YieldedJob, which clears post-processing's launch claim on the
	// job: a RemoveJob holding this lock waits on that claim in
	// dispatcher.Remove.
	waitCtx, waitCancel := context.WithTimeout(app.ctx, finalizeTransitionWait)
	claim, err := app.transitions.acquire(waitCtx, ppJob.Job.ID())
	waitCancel()
	if err != nil {
		log.Warn("finalize did not get the job's transition lock in time; proceeding without it",
			"job", ppJob.Job.ID(), "err", err)
	} else {
		defer claim.release()
	}
	// A RemoveJob took this instance and has not given it back, and is
	// tearing down or has torn down what this would commit. Only its mark
	// says so: a job merely gone from the dispatcher may be a never-run
	// job the tick evicted after the Cancel above, and that one is still
	// filed, through OccupyJob's fallback below.
	if app.transitions.wasRemoved(ppJob.Job) {
		log.Info("finalize: the job was removed while this waited for it; not filing it",
			"job", ppJob.Job.ID())
		return errFinalizedJobRemoved
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
			if entry.Status == string(constants.StatusFailed) {
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
		if app.checkpointer != nil {
			app.checkpointer.Prune(ppJob.Job)
		}
		// Not fatal, unlike the reconcile path's version: this job IS in
		// history, so the next startup's dropJobAlreadyInHistory removes the
		// queue row. reclaim below leaves the manifest and rows of a job the
		// dispatcher still holds, which is what keeps that row loadable until
		// then (#376: appResidency.hydrate fails without the manifest).
		//
		// dispatcher.RemoveJob rather than Remove by ID: on the fallback below this
		// instance is not occupied, and without the transition lock a retry
		// can register under the ID meanwhile. ErrNotFound means this
		// instance is not registered, so there is nothing to remove, retry or
		// mark.
		if app.dispatcher != nil {
			jobID := ppJob.Job.ID()
			removeCtx, removeCancel := context.WithTimeout(occupyCtx, 3*time.Second)
			err := app.dispatcher.RemoveJob(removeCtx, ppJob.Job)
			removeCancel()
			if err != nil && !errors.Is(err, dispatch.ErrNotFound) && occupyCtx.Err() == nil {
				retryCtx, retryCancel := context.WithTimeout(occupyCtx, 3*time.Second)
				err = app.dispatcher.RemoveJob(retryCtx, ppJob.Job)
				retryCancel()
			}
			if err != nil && !errors.Is(err, dispatch.ErrNotFound) {
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
		app.reclaim(delCtx, ppJob.Job.ID())
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

	// OccupyJob runs the commit only while this instance is registered, and
	// no other instance can be registered under its ID until the commit's own
	// dispatcher.RemoveJob ends. It fails when the instance is not registered, when a
	// removal of it is in progress, or when a later instance holds the ID.
	//
	// The last must stop the finalizer: filing would put this run in history
	// under the retry's ID. `git grep -n 'dispatcher\.Add(' -- 'internal/app/*.go' ':!*_test.go'`
	// finds 2 production registrations. retryHistoryJob's, the body of
	// RetryHistoryJob and of retryWithHeldVolumes, reuses an ID through the
	// FetchOptions.JobID it sets, and takes the transition lock;
	// AddJob's jobs are built by BuildIngestJob, which mints a newJobID when
	// no JobID is set. So while this holds the lock the answer cannot change
	// underneath. Without it, a retry can register during the fallback: this
	// run's history write still goes ahead, and dispatcher.RemoveJob is what
	// leaves the retry registered.
	if app.dispatcher != nil {
		var runErr error
		if err := app.dispatcher.OccupyJob(finalCtx, ppJob.Job, func(occupyCtx context.Context) {
			runErr = runCommit(occupyCtx)
		}); err != nil {
			if cur, ok := app.dispatcher.Job(ppJob.Job.ID()); ok && cur != ppJob.Job {
				log.Warn("finalize: a later instance of the job holds its ID; not filing this run",
					"job", ppJob.Job.ID())
				return errFinalizedJobSuperseded
			}
			// This instance is not registered, such as a never-run job the
			// tick evicted after the Cancel above, or a removal of it is in
			// progress. It is still filed, without the occupancy.
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
