package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
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
			// Not resident. Nothing here can install bits into it, and that
			// is reported rather than silently skipped so the gap is visible
			// in a log rather than only in this comment.
			//
			// No reachable state gets here any more, and that is worth writing
			// down so nobody spends an afternoon building a fixture for it.
			// The phase bound above already excluded everything that used to
			// arrive non-resident, and a PhaseActive job is resident on every
			// route into that phase: promotion attaches the manifest before it
			// sets StatusDownloading; SetStatus re-hydrates on the way into a
			// resident status, or fails and leaves the status alone; and
			// SQLiteStore.Get REMOVES a job with files whose manifest file is
			// missing rather than returning it non-resident, so the
			// crash-with-a-lost-manifest case never reaches the queue at all.
			// Verified by replacing this arm with a panic: nothing in
			// ./internal/app reaches it.
			//
			// It stays because the alternative is a nil dereference if any of
			// those three changes, and because Snapshot deliberately does not
			// hydrate — "has a resident status" and "has a manifest on this
			// clone" are two facts, not one. Read the absence of a test as the
			// absence of a state to write one against, not as permission to
			// delete the guard.
			app.log.Debug("resume sweep skipped a non-resident job",
				"job", snap.ID, "status", snap.Status, "err", err)
			continue
		}
		swept, runs, fault, err := app.resumeJobFiles(ctx, snap, m)
		if err != nil {
			return err
		}
		// Seeded BEFORE the stall below, and unconditionally, because a fault
		// on one file says nothing about the files already resumed.
		//
		// This used to return early on the first fault and discard them, and
		// the cost was permanent rather than transient: the stall pauses the
		// job, a paused job is not resident, and a non-resident job is skipped
		// by every future sweep — which only runs at startup anyway. So an NFS
		// flap on file 7 of 20 threw away the seed for all 20 and no later
		// retry could recover it. A transient fault turning into a permanent
		// loss of ground is the exact failure class this task exists to
		// prevent.
		//
		// The order is load-bearing the other way too: Stall pauses the job,
		// which evicts its manifest, and ReplaceFromRuns needs a resident one.
		//
		// That eviction used to strand the correction. The replace only
		// mutated the in-memory JobProgress and set a dirty flag, so a Stall
		// here discarded a cleared bit before any save, and the next promotion
		// re-read the pre-correction row. ReplaceFromRuns now persists a
		// clearing correction before it returns, which is what makes this
		// ordering safe rather than merely necessary.
		//
		// A fault also bounds what "authoritative" may mean here, and `swept`
		// is what carries that bound: it names only the files this sweep
		// actually stat'ed, and ReplaceFromRuns touches only those. A file the
		// fault stopped it from reaching is omitted, and omission is silence
		// rather than a finding of absence — clearing on behalf of a file
		// nobody read would turn one unreadable mount into a full re-download
		// of the job.
		//
		// The file list has to travel SEPARATELY from the runs, and this is
		// why: a file whose runs the gate discarded contributes no run at all,
		// so the runs alone cannot distinguish "I looked and found nothing"
		// from "I never looked".
		if err := app.queue.ReplaceFromRuns(snap.ID, swept, runs); err != nil {
			// Not fatal to startup, but the two failures behind this differ
			// and the message must not flatten them.
			//
			// A seeding failure costs a re-fetch: the job was not told what it
			// already has, which is the safe direction under S3.
			//
			// A PERSIST failure is the other direction. The correction is
			// applied in memory, so this process behaves correctly, but the
			// stored row still records the disproven article as done. If the
			// job is evicted before the next successful save, the next
			// promotion re-reads that row and the article comes back Done
			// without its bytes. An earlier version of this comment claimed a
			// re-fetch either way, which is true only of the first case.
			app.log.Warn("resume sweep could not fully apply a job's recomputation; "+
				"if this was the persist, an eviction before the next queue save "+
				"restores the disproven articles",
				"job", snap.ID, "err", err)
		}
		if fault != nil {
			// A1: a failure to READ the disk says nothing about any article.
			// The job stalls with a surfaced reason and every article stays
			// Outstanding; marking one permanently failed here would burn its
			// retry budget and degrade the job's reported health over a
			// condition of the device.
			//
			// Stalled even when the fault classifies permanent, which is
			// deliberately NOT what Barrier.routeFault does. The two answer
			// different questions: the barrier asks "is this condition
			// recoverable", while startup asks "is there work to protect by
			// failing". Here there is none — nothing has been downloaded in
			// this process — so failing would send a job to history and
			// discard the bytes an earlier run left on disk, over an EACCES
			// or EROFS on a mount that has not finished coming up at boot and
			// that the operator clears in seconds.
			app.Stall(snap.ID, fault)
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
func (app *Application) resumeJobFiles(ctx context.Context, snap *queue.Job, m *queue.Manifest) (swept []int32, runs []durability.Run, fault *storagefault.Fault, err error) {
	nFiles := m.NumFiles()
	swept = make([]int32, 0, nFiles)
	progress := snap.Progress()
	var restarted int
	for f := range nFiles {
		// R15: checked per file rather than per job. One file's gate is a
		// single stat, so this is cheap insurance rather than the long-poll it
		// was when the fallback re-read the whole file.
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, fmt.Errorf("app: resume sweep aborted: %w", err)
		}
		// A file with no resolved filename was never registered, so no
		// process ever opened a path for it and there is nothing on disk to
		// resume against. Guessing a path here would stat an unrelated file.
		filename := progress.FileFilename(f)
		if filename == "" {
			continue
		}
		path := app.pipeline.jobFilePath(snap.Name, filename)
		//nolint:gosec // G115: file counts are far below int32
		res, rErr := app.resumer.Resume(ctx, snap.ID, int32(f), path)
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
		"job", snap.ID, "files", len(swept), "runs", len(runs),
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
