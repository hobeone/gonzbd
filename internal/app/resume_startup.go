package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// fileResumer is the sweep's whole view of durability.Resumer: one file in,
// what stable storage can prove about it out.
//
// A field of this type rather than *durability.Resumer so a test can count
// and interleave the per-file calls. R15 requires the sweep to be
// interruptible BETWEEN FILES, not merely between jobs, and that is a claim
// about how many files were processed — which is unobservable from outside
// unless the calls can be counted. A test that asserted only "an error came
// back" was satisfied by the store's own read failing on the cancelled
// context, and so passed with both checks deleted.
//
// durability.Resumer satisfies this.
type fileResumer interface {
	Resume(ctx context.Context, jobID string, fileIdx int32, path string) (durability.ResumeResult, error)
}

type progressFilenameReader interface {
	FileFilename(fi int) string
}

// resumeAllJobs re-derives every DOWNLOADING job's work set from what is
// actually on stable storage, and is the production caller L3 was missing.
//
// "Downloading" rather than "resident", and that word is the guard rather than
// a description of it — see the phase note below. It re-derives rather than
// seeds: the result REPLACES what Store.RestoreJobProgress restored, including
// clearing a bit whose bytes are gone (#362).
//
// durability.Resumer and the queue's seeding entry points were both built and
// tested with nothing calling either, so a restart re-downloaded every byte an
// earlier run had already fsynced. This is where the two meet.
//
// It seeds through Queue.ReplaceFromRuns rather than Queue.SeedFromRuns, and
// that is the whole of #362's fix. This is the ONLY caller that has stat'ed
// the files, so it is the only one entitled to contradict what
// Store.RestoreJobProgress derived — which it does by having had
// durability.Resumer DELETE the runs of a file shorter than they claim. Every
// other seeding path is replaying an ack that already landed and must stay
// additive; see SeedFromRuns' doc for why merging the two is a silent
// regression in one direction or the other.
//
// # Why a startup sweep, and why that is complete
//
// It runs once, synchronously, inside Start — after queue.Load has produced
// app.queue and BEFORE the downloader begins dispatching. The ordering is
// load-bearing TWICE over, and only the first half is about re-fetching:
//
//   - A seed that lands after dispatch has begun still marks the right
//     articles Done, but the request for them is already on the wire, which is
//     exactly the re-fetch this exists to prevent.
//   - The gate itself depends on it. durability.Resumer compares the file's
//     size against what its runs claim; if the assembler had already
//     re-created and pre-allocated a deleted partial, that comparison would
//     run against a file of zeros and pass. Nothing inside Resumer can notice
//     that, so moving this sweep later breaks the guarantee silently. See
//     Resumer.Resume, which says the same thing from the other side.
//
// Running only at startup is nonetheless complete. A job admitted later has no
// runs to seed from, and a job's runs cannot GAIN content while it is not
// running — only a barrier puts content into a row, and a barrier runs only
// for a job with open files. Other paths delete rows, but a delete cannot make
// a row claim more than it already did. So a job promoted hours after startup
// is still correctly seeded by the sweep that ran before it was promoted.
//
// # What it does NOT do
//
// It writes NOTHING to the durability record, and neither does anything under
// it. Resumer is a reader and a deleter: the one mutation in this whole sweep
// is discarding the runs of a file that is missing or shorter than they claim.
// The barrier is the only thing that INSERTS OR AMENDS run content — several
// other paths delete rows, and internal/durability's package doc enumerates
// them — and that asymmetry is the justification for §3.4 trusting the record
// without reading a byte of it: nothing but a completed fsync can put a claim
// INTO the record, and a delete only ever takes one away.
//
// A non-resident job in a SWEPT phase is hydrated for the duration and evicted
// again, so the residency budget docs/queue-lifecycle.md exists to bound is
// unchanged from outside. It matters because a Paused job is the case that
// needs this most and is never resident: Application.Stall leaves the job
// Paused, and the sweep skipping it is what let #362 survive in that branch.
// Startup is the moment the hydration is cheapest and safest — nothing else
// holds a manifest and no article is being dispatched.
//
// # Active and Paused, because only there is the assembler the sole writer
//
// Residency is NOT the right bound now that the seed is authoritative.
// JobPhase.IsResident is true for PhaseProcessing as well — Verifying,
// QuickCheck, Repairing, Extracting, Moving — and in those phases something
// other than the assembler owns the job's files. par2 repairs a file IN PLACE,
// unpack reads it, and the move relocates it out of the download directory
// altogether. Those bytes are correct, and they are not the bytes the runs
// recorded at download time, so re-deriving over them proves nothing: it would
// clear real progress, drop Complete and discard the assembled CRC on a file
// that is fine. A repaired file is the clear case; a moved one is the blunt
// one — the path no longer exists, Resume reports Restart, and every article
// of a fully downloaded job goes back to Outstanding.
//
// The direction is pessimistic rather than corrupting, which is why it was
// harmless while seeding was additive and stopped being harmless the moment it
// was not.
//
// It has since stopped being merely in-memory, too. Resumer.Resume DELETES the
// file's runs on the missing-file branch, so widening this bound to cover a
// Moving job would not just return its articles to Outstanding for the life of
// the process — it would erase the record that says those bytes were ever
// made durable, and the erasure survives the restart the seed exists to
// survive.
//
// Skipping those phases costs nothing this exists to buy. The seed prevents a
// re-fetch, and a job in post-processing dispatches no articles; if par2 sends
// it back to the queue, the bits Store.RestoreJobProgress restored are still
// there, because skipping leaves them alone. So the conservative direction for
// a processing job is precisely to keep the record and not re-derive it.
//
// What would break this argument, stated so a later change has to notice.
//
// PhaseActive is StatusDownloading and StatusFetching, and the second one is
// ALREADY not download-only: constants.StatusFetching is "downloading extra
// par2 files for repair", which is a repair-time status. The guard is sound
// today only because nothing assigns it — it exists in the transition table,
// the phase mapping and the API's vocabulary, and no code path sets it. That
// is a fact about the writers, not an invariant the type enforces (Job.Phase's
// own doc makes the same point about Grabbing and Checking, and notes that the
// load paths assign Status from a persisted string without validating it). So
// the hazard here is present and load-bearing on unreachability: the first
// code that starts setting StatusFetching puts a repair-time job inside the
// window this guard trusts, and must move it out of PhaseActive or bound the
// sweep on the status rather than the phase.
//
// The other way in is a non-assembler writer arriving inside PhaseActive at
// all — a DirectUnpack that wrote back into its source rather than reading it,
// or a repair moved earlier than download-complete.
func (app *Application) resumeAllJobs(ctx context.Context) error {
	if app.resumer == nil {
		return nil
	}
	if app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("app: resume sweep aborted: %w", err)
			}
			if row.View.State != job.Fetching {
				app.log.Debug("resume sweep skipped a job that is not downloading",
					"job", row.ID, "state", row.View.State)
				continue
			}
			if app.residency != nil {
				_ = app.residency.Hydrate(ctx, row.ID)
			}
			j, ok := app.dispatcher.Job(row.ID)
			if !ok {
				continue
			}
			m, err := j.Manifest()
			if err != nil {
				app.log.Debug("resume sweep skipped a non-resident job",
					"job", row.ID, "err", err)
				continue
			}
			swept, runs, fault, err := app.resumeJobFiles(ctx, row.ID, m, row.Header.Name, j.Progress())
			if err != nil {
				return err
			}
			replaceErr := j.ReplaceFromRuns(swept, runs)
			if replaceErr != nil {
				app.log.Warn("resume sweep could not fully apply a job's recomputation",
					"job", row.ID, "err", replaceErr)
			}
			if app.checkpointer != nil {
				_ = app.checkpointer.Flush(ctx)
			}
			if fault == nil && replaceErr == nil {
				app.completeStrandedFiles(ctx, row.ID, m, swept, runs)
			}
			if fault != nil {
				app.Stall(row.ID, fault)
			}
		}
	}
	if app.queue != nil {
		for _, snap := range app.queue.Snapshot() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("app: resume sweep aborted: %w", err)
			}
			if !sweptStatus(snap.Status) {
				// Not a residency check — see the phase note above. A job past
				// downloading has files the assembler no longer owns, and there
				// is no re-fetch to prevent for it either way.
				app.log.Debug("resume sweep skipped a job that is not downloading",
					"job", snap.ID, "status", snap.Status, "phase", snap.Phase())
				continue
			}
			// A Paused job is not resident, so its clone carries no manifest.
			// SnapshotJob hydrates one onto a clone, which is read-only and does
			// not change the live job's residency; the write-back below goes
			// through Queue.ReplaceFromRuns, which hydrates the live job itself
			// for the duration.
			if hydrated := app.queue.SnapshotJob(snap.ID); hydrated != nil {
				snap = hydrated
			}
			m, err := snap.Manifest()
			if err != nil {
				app.log.Debug("resume sweep skipped a non-resident job",
					"job", snap.ID, "status", snap.Status, "err", err)
				continue
			}
			swept, runs, fault, err := app.resumeJobFiles(ctx, snap, m)
			if err != nil {
				return err
			}
			replaceErr := app.queue.ReplaceFromRuns(snap.ID, swept, runs)
			if replaceErr != nil {
				app.log.Warn("resume sweep could not fully apply a job's recomputation; "+
					"if this was the persist, an eviction before the next queue save "+
					"restores the file's cleared Complete flag and assembled CRC",
					"job", snap.ID, "err", replaceErr)
			}
			if app.dispatcher != nil {
				if j, ok := app.dispatcher.Job(snap.ID); ok {
					_ = j.ReplaceFromRuns(swept, runs)
				}
			}
			if fault == nil && replaceErr == nil {
				app.completeStrandedFiles(ctx, snap.ID, m, swept, runs)
			}
			if fault != nil {
				app.Stall(snap.ID, fault)
			}
		}
	}
	return nil
}

// resumeJobFiles resumes each of one job's files and returns two things
// ReplaceFromRuns needs separately: WHICH files this sweep actually stat'ed,
// and the runs those files still hold afterwards.
//
// The two cannot be one value, and the reason is the case this sweep exists
// for. A file the gate DISCARDED contributes no run — that is what a discard
// means — and so is indistinguishable from a file the sweep never looked at if
// only the runs travel. The first must return every one of its articles to
// Outstanding; the second must be left exactly as it was found.
//
// ResumeResult.Restart therefore needs no branch of its own: a discarded file
// is already in `swept` with no runs beside it, which is precisely what
// ReplaceFromRuns reads as "nothing here is recorded". A guard here would be a
// branch whose two arms produce the same result.
//
// A file whose filename was never resolved is skipped and appears in NEITHER
// return. No process ever opened a path for it, so there is nothing to have
// found absent.
//
// The three failure-shaped returns are distinct outcomes and are deliberately
// not collapsed into one error: a storage fault stalls this job and the sweep
// continues, a context error aborts the sweep entirely, and neither is the
// other.
//
// A fault returns what was gathered from the files BEFORE it, not nil. Its
// caller seeds them and then stalls; see the note there for why discarding
// them turned a transient fault into a permanent loss of ground.
func (app *Application) resumeJobFiles(ctx context.Context, target, m any, rest ...any) (swept []int32, runs []durability.Run, fault *storagefault.Fault, err error) {
	var jobID, jobName string
	var progress progressFilenameReader
	switch t := target.(type) {
	case *queue.Job:
		jobID = t.ID
		jobName = t.Name
		progress = t.Progress()
	case *job.Job:
		jobID = t.ID()
		jobName = t.Name()
		progress = t.Progress()
	case string:
		jobID = t
		if len(rest) > 0 {
			if name, ok := rest[0].(string); ok {
				jobName = name
			}
		}
		if len(rest) > 1 {
			if pr, ok := rest[1].(progressFilenameReader); ok {
				progress = pr
			}
		}
	default:
		return nil, nil, nil, fmt.Errorf("app: unsupported resume target %T", target)
	}

	numFiles := 0
	if mr, ok := m.(interface{ NumFiles() int }); ok {
		numFiles = mr.NumFiles()
	}
	swept = make([]int32, 0, numFiles)
	var restarted int
	for f := range numFiles {
		// R15: checked per file rather than per job. One file's gate is a
		// single stat, so this is cheap insurance rather than the long-poll it
		// was when the fallback re-read the whole file.
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, fmt.Errorf("app: resume sweep aborted: %w", err)
		}
		// A file with no resolved filename was never registered, so no
		// process ever opened a path for it and there is nothing on disk to
		// resume against. Guessing a path here would stat an unrelated file.
		var filename string
		if progress != nil {
			filename = progress.FileFilename(f)
		}
		if filename == "" {
			continue
		}
		path := app.pipeline.jobFilePath(jobName, filename)
		//nolint:gosec // G115: file counts are far below int32
		res, rErr := app.resumer.Resume(ctx, jobID, int32(f), path)
		if rErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, nil, nil, fmt.Errorf("app: resume sweep aborted: %w", ctxErr)
			}
			return swept, runs, storagefault.Classify("resume", path, rErr), nil
		}
		if res.Restart {
			restarted++
		}
		swept = append(swept, int32(f)) //nolint:gosec // G115: file counts are far below int32
		runs = append(runs, res.Runs...)
	}
	app.log.Info("resumed a job's work set from stable storage",
		"job", jobID, "files", len(swept), "runs", len(runs),
		"files_restarted", restarted)
	return swept, runs, nil, nil
}

// jobFilePath resolves the on-disk path the assembler would have used for one
// of a job's files, from the filename the queue already recorded.
//
// It reads p.downloadDir under the same lock registerFile does, so the two
// cannot disagree about which directory a job's files live in, and it applies
// the same JoinSafe sanitisation — a resume that stat'ed a different path than
// the writer used would report every file missing.
func (p *pipeline) jobFilePath(jobName, filename string) string {
	p.mu.RLock()
	jobDir := filepath.Join(p.downloadDir, jobName)
	sanitize := p.sanitize
	p.mu.RUnlock()
	// --- No lock held below this line ---
	return fsutil.JoinSafe(jobDir, "", filename, sanitize)
}

// sweptStatus reports whether the startup resume sweep may re-derive a job's
// progress from its bytes.
//
// Downloading and Fetching are PhaseActive: the assembler is the only writer,
// so the file on disk is exactly what the record describes and a size that
// falls short of it really means bytes were lost.
//
// PAUSED is here for the same reason and was missing. A paused job is
// mid-download — nothing else owns its files, and the assembler wrote every
// byte in them — but it is not PhaseActive, so the sweep skipped it and #362
// survived in that branch: its disproven Done bits were never corrected, the
// next checkpoint re-recorded them, and the file finalized over a hole. It is
// also the branch Application.Stall leaves jobs in, which is what made
// stallLost's own "restart gonzbd to resume this job from its recorded runs"
// instruction unable to work.
//
// Everything else is excluded, and the phase note on resumeAllJobs says why at
// length: in PhaseProcessing something other than the assembler owns the files
// — par2 repairs one IN PLACE, unpack reads it, the move relocates it — so a
// recomputation over those bytes proves nothing and would clear real progress.
func sweptStatus(s constants.Status) bool {
	switch s {
	case constants.StatusDownloading, constants.StatusFetching, constants.StatusPaused:
		return true
	default:
		return false
	}
}

// completeStrandedFiles finishes the finalize a crash interrupted, for every
// file of one job that a restart would otherwise leave wedged.
//
// # The state it repairs
//
// The two facts about a finished file survive a crash by different mechanisms,
// and only one of them is transactional. Article resolution survives through
// durable_runs, written in the barrier's commit and replayed above by
// ReplaceFromRuns. The file's Complete flag survives only through
// job_files.complete, written by the NEXT queue save. A crash between them
// leaves a file with every article resolved, no Complete flag, and nothing
// able to re-complete it: completion fires from partsWritten == TotalParts
// inside the assembler, that counter moves only when the assembler is handed
// an article to write, and every one of this file's articles is Done so none
// is dispatched. The job is then not dispatchable, not complete and not
// failed, across every restart.
//
// The in-process form of the same interruption — a stall between "bytes
// correct" and "file marked complete" — is already handled, by
// noteUndeliveredCompletion and the stall re-evaluation. That note is in
// memory, which is exactly why it does not cover the crash.
//
// # Why it is a pass here rather than a derived flag
//
// Deriving Complete on load from the article bits is the tempting fix and it
// is wrong. Complete does not mean "every article resolved" — it means the
// finalize RAN, including the truncate to the durable bound. Barrier.Run acks
// articles without truncating and only FinalizeFile truncates, so the bits
// under-determine the flag, and a derived Complete would send untrimmed,
// pre-allocated files into post-processing. Doing the trim here is what earns
// the flag rather than assuming it.
//
// # Ordering
//
// After ReplaceFromRuns, necessarily: that is what installs the Done bits this
// reads, so before it every file looks unresolved. After §3.4's gate for a
// second reason — a file the gate found short has had its runs DELETED, so it
// is no longer complete and must not be trimmed to a bound derived from runs
// that are gone.
//
// Skipped entirely when the sweep raised a storage fault. The job is about to
// be stalled, the device has already refused one read, and a truncate is the
// one irreversible thing this whole sweep does. A file left stranded is no
// worse off than before this pass existed; a file trimmed against a failing
// mount is not recoverable by trying again later.
func (app *Application) completeStrandedFiles(ctx context.Context, jobID string, m any, swept []int32, runs []durability.Run) {
	var jobName string
	var progress any
	if app.queue != nil {
		if snap := app.queue.SnapshotJob(jobID); snap != nil {
			jobName = snap.Name
			progress = snap.Progress()
		}
	} else if app.dispatcher != nil {
		if j, ok := app.dispatcher.Job(jobID); ok {
			jobName = j.Name()
			progress = j.Progress()
		}
	}
	if progress == nil {
		return
	}
	for _, f := range swept {
		fi := int(f)
		if !strandedComplete(progress, m, fi) {
			continue
		}
		var filename string
		if pr, ok := progress.(progressFilenameReader); ok {
			filename = pr.FileFilename(fi)
		}
		if filename == "" {
			continue
		}
		path := app.pipeline.jobFilePath(jobName, filename)
		bound, err := durability.TrimToRuns(path, runsForFile(runs, f))
		if err != nil {
			// Logged and skipped rather than stalled. The file keeps the
			// state it arrived in, which is the state every start before this
			// pass existed left it in, so the failure costs the repair and
			// nothing else. Stalling a job over a repair that is itself
			// optional would turn a recoverable wedge into a paused job.
			app.log.Warn("could not finish a finalize a crash interrupted; the file stays "+
				"incomplete and the job will not progress until this succeeds",
				"job", jobID, "fileidx", fi, "path", path, "err", err)
			continue
		}
		app.log.Info("finished a finalize a crash interrupted",
			"job", jobID, "fileidx", fi, "trimmed_to", bound)
		if err := app.completeFinalizedFile(ctx, FileComplete{JobID: jobID, FileIdx: fi}); err != nil {
			app.log.Warn("trimmed a stranded file but could not record its completion",
				"job", jobID, "fileidx", fi, "err", err)
		}
	}
}

// strandedComplete reports whether one file has every article resolved and no
// Complete flag — the state a crash between the barrier's commit and the
// following queue save leaves behind.
//
// FetchAlways only, matching Job.IsComplete: a deferred or discarded par2
// recovery volume is never dispatched, so "every article resolved" is
// vacuously true of it and completing it would claim a file nobody fetched.
//
// A file with NO articles is excluded for the same reason and it is not a
// hypothetical guard: the loop below is vacuously true over an empty range,
// so without this an empty file range would be reported stranded on every
// start.
//
// "Resolved" is the Done bit, which covers a permanently failed article as
// well as a successful one. That matches what completion means everywhere
// else — the assembler's partsWritten counts a permanent failure toward
// TotalParts — so a file whose last article will never arrive is complete,
// short, and par2's problem.
func strandedComplete(prog, mf any, fi int) bool {
	var numFiles int
	var fileRange func(int) (int, int)
	switch m := mf.(type) {
	case *job.Manifest:
		numFiles = m.NumFiles()
		fileRange = m.FileRange
	case *queue.Manifest:
		numFiles = m.NumFiles()
		fileRange = m.FileRange
	default:
		return false
	}

	if fi < 0 || fi >= numFiles {
		return false
	}

	var fetchAlways bool
	var fileComplete bool
	var articleDone func(int) bool
	switch p := prog.(type) {
	case *job.JobProgress:
		fetchAlways = p.FileFetchPolicy(fi) == job.FetchAlways
		fileComplete = p.FileComplete(fi)
		articleDone = p.ArticleDone
	case *queue.JobProgress:
		fetchAlways = p.FileFetchPolicy(fi) == queue.FetchAlways
		fileComplete = p.FileComplete(fi)
		articleDone = p.ArticleDone
	default:
		return false
	}

	if !fetchAlways || fileComplete {
		return false
	}

	lo, hi := fileRange(fi)
	if hi <= lo {
		return false
	}
	for i := lo; i < hi; i++ {
		if !articleDone(i) {
			return false
		}
	}
	return true
}

// runsForFile selects one file's runs out of a whole job's.
//
// The sweep gathers every file's runs into one slice because ReplaceFromRuns
// takes them that way, and filtering here rather than re-reading the store
// keeps this pass to the reads the sweep has already paid for.
func runsForFile(runs []durability.Run, fileIdx int32) []durability.Run {
	out := make([]durability.Run, 0, len(runs))
	for _, r := range runs {
		if r.FileIdx == fileIdx {
			out = append(out, r)
		}
	}
	return out
}
