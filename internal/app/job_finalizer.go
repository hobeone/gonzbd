package app

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
// from the dispatcher, and broadcasts the finalization events. Registry and
// filesystem teardown (checkpointer prune, dispatcher removal, manifest
// unlinking, and barrier state reset) is always attempted
// regardless of history persistence success. If dispatcher.Remove returns an
// error, the error is logged while the job remains registered for retry or
// caller handling.
// Sub-budgets within persistAndCommit are strictly partitioned against
// starvation:
//   - History write & files loop: 4s dbCtx, derived from
//     context.WithoutCancel(app.ctx).
//   - Dispatcher removal: 3s removeCtx, derived from occupyCtx to retain the
//     occupancy lease token for bypass in Dispatcher.Remove.
//   - Durability check & delete: 3s delCtx, derived from
//     context.WithoutCancel(app.ctx).
//
// Because dbCtx, removeCtx, and delCtx are independently derived, a slow SQLite
// write cannot starve dispatcher removal or durability cleanup. Untimed local
// filesystem and checkpointer operations (Prune, os.Remove) complete promptly
// in memory or on the local filesystem. Note that the enclosing finalize method
// also executes completion notifications and asynchronous history pruning
// under its own 30s context outside of persistAndCommit.
//
// Durability rows for a failed job are owned by the retry path and must never
// be deleted here. Furthermore, if history persistence fails against a
// pre-existing failed history entry (such as after a crash between commit and
// Dispatcher.Remove), those durability rows belong to the existing failed entry
// and are preserved for retry.
//
// Returns a non-nil error if persistence failed (the error is already logged;
// callers can simply return).
func (f *jobFinalizer) persistAndCommit(log *slog.Logger, entry history.Entry, ppJob *postproc.Job) error {
	app := f.app
	if app.dispatcher != nil && ppJob != nil && ppJob.Job != nil {
		_ = app.dispatcher.Cancel(ppJob.Job.ID())
		_ = app.dispatcher.Yielded(ppJob.Job.ID())
	}

	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(app.ctx), 10*time.Second)
	defer finalCancel()

	runCommit := func(occupyCtx context.Context) error {
		var persistErr error
		if app.historyRepo != nil && app.historyRepo.DB() != nil {
			dbCtx, dbCancel := context.WithTimeout(context.WithoutCancel(app.ctx), 4*time.Second)
			defer dbCancel()
			if err := app.historyRepo.Add(dbCtx, entry); err != nil {
				log.Error("failed to add history entry; registry and filesystem teardown completed but history entry failed to persist",
					"job", ppJob.Job.ID(), "err", err)
				persistErr = err
			} else if entry.Status == string(constants.StatusFailed) && ppJob != nil && ppJob.Job != nil {
				p := ppJob.Job.Progress()
				m, mErr := ppJob.Job.Manifest()
				if mErr != nil && errors.Is(mErr, job.ErrNotResident) {
					manifestPath := filepath.Join(app.config.GetGeneral().AdminDir, "queue", "manifests", ppJob.Job.ID()+".json.gz")
					if diskM, err := readManifestFile(manifestPath); err == nil {
						m = diskM
						mErr = nil
					}
				}
				if mErr != nil {
					log.Error("failed to load manifest for failed job files; history_job_files not populated",
						"job", ppJob.Job.ID(), "err", mErr)
				} else if p != nil && m != nil {
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
							ppJob.Job.ID(), fi, complete, fetch, filename, crc, artCount)
					}
				}
			}
		}
		if app.checkpointer != nil && ppJob != nil && ppJob.Job != nil {
			app.checkpointer.Prune(ppJob.Job.ID())
		}
		if app.dispatcher != nil && ppJob != nil && ppJob.Job != nil {
			removeCtx, removeCancel := context.WithTimeout(occupyCtx, 3*time.Second)
			defer removeCancel()
			if err := app.dispatcher.Remove(removeCtx, ppJob.Job.ID()); err != nil {
				log.Warn("failed to remove job from dispatcher after post-proc", "job", ppJob.Job.ID(), "err", err)
			}
		}
		manifestPath := filepath.Join(app.config.GetGeneral().AdminDir, "queue", "manifests", ppJob.Job.ID()+".json.gz")
		_ = os.Remove(manifestPath)

		delCtx, delCancel := context.WithTimeout(context.WithoutCancel(app.ctx), 3*time.Second)
		defer delCancel()
		shouldDeleteDurability := entry.Status != string(constants.StatusFailed)
		if shouldDeleteDurability && persistErr != nil && app.historyRepo != nil && app.historyRepo.DB() != nil {
			existing, err := app.historyRepo.Get(delCtx, ppJob.Job.ID())
			if err == nil {
				if existing.Status == string(constants.StatusFailed) {
					shouldDeleteDurability = false
				}
			} else if !errors.Is(err, history.ErrNotFound) {
				log.Warn("history lookup failed during finalize conflict check; preserving durability rows on doubt",
					"job", ppJob.Job.ID(), "err", err)
				shouldDeleteDurability = false
			}
		}
		if shouldDeleteDurability {
			app.deleteJobDurability(delCtx, ppJob.Job.ID())
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

func readManifestFile(path string) (*job.Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	var m job.Manifest
	if err := json.NewDecoder(zr).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

