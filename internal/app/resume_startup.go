package app

import (
	"context"
	"fmt"
	"path/filepath"

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
// back" was satisfied by ExtentStore.Load failing on the cancelled context,
// and so passed with both checks deleted.
//
// durability.Resumer satisfies this and its signature is unchanged.
type fileResumer interface {
	Resume(ctx context.Context, jobID string, fileIdx int32, path string, firstArtIdx int32, artCount int) (durability.ResumeResult, error)
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
// It seeds through Queue.ReplaceFromResume rather than Queue.SeedFromExtents,
// and that is the whole of #362's fix. This is the ONLY caller that has just
// read the files' bytes, so it is the only one entitled to contradict what
// Store.RestoreJobProgress restored from job_files.articles_done. Every other
// seeding path is replaying an ack that already landed and must stay additive;
// see SeedFromExtents' doc for why merging the two is a silent regression in
// one direction or the other.
//
// # Why a startup sweep, and why that is complete
//
// It runs once, synchronously, inside Start — after queue.Load has produced
// app.queue and BEFORE the downloader begins dispatching. The ordering is the
// whole point: a seed that lands after dispatch has begun still marks the
// right articles Done, but the request for them is already on the wire, which
// is exactly the re-fetch this exists to prevent.
//
// Running only at startup is nonetheless complete. A job admitted later has no
// committed extents to seed from, and a job's extents cannot change while it
// is not running — only a barrier commits Class B, and a barrier runs only for
// a job with open files. So a job promoted hours after startup is still
// correctly seeded by the sweep that ran before it was promoted.
//
// # What it does NOT do
//
// It commits no extent ITSELF, but it is no longer true that the barrier is
// the only writer of Class B: durability.Resumer writes a RECOMPUTED result
// back over the record it disproved, from inside Resume.
//
// The rule that used to sit here — a resume "has no licence to mint" a
// committed extent without performing the fsync — mis-stated the cost of
// omission as "paying the verification again on the next restart". There is no
// second verification. Nothing clears a bit in the store, priorExtent ORs the
// stored bitmap as its base, so the next checkpoint re-commits a disproven bit
// with a fresh stamp that validates, and the next start's fast path adopts it
// without reading a byte. The bit does not cost rework; it comes back.
//
// An ADOPTED result still writes nothing, because it came from the stored row
// in the first place.
//
// A non-resident job is skipped rather than hydrated: ReplaceFromResume
// installs bits into the LIVE job's JobProgress, which requires a resident
// manifest, and hydrating the whole queue at startup would blow the residency
// budget docs/queue-lifecycle.md exists to bound. See the note on residency in
// resumeJobFiles for what that leaves uncovered.
//
// # Only PhaseActive, because only there is the assembler the sole writer
//
// Residency is NOT the right bound now that the seed is authoritative.
// JobPhase.IsResident is true for PhaseProcessing as well — Verifying,
// QuickCheck, Repairing, Extracting, Moving — and in those phases something
// other than the assembler owns the job's files. par2 repairs a file IN PLACE,
// unpack reads it, and the move relocates it out of the download directory
// altogether. Those bytes are correct, and they are not the bytes the fact log
// recorded at download time, so a recomputation over them proves nothing: it
// would clear real progress, drop Complete and discard the assembled CRC on a
// file that is fine. A repaired file is the clear case; a moved one is the
// blunt one — the path no longer exists, Resume reports Restart, and every
// article of a fully downloaded job goes back to Outstanding.
//
// The direction is pessimistic rather than corrupting, which is why it was
// harmless while seeding was additive and stopped being harmless the moment it
// was not.
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
		if phase := snap.Phase(); phase != queue.PhaseActive {
			// Not a residency check — see the phase note above. A job past
			// downloading has files the assembler no longer owns, and there
			// is no re-fetch to prevent for it either way.
			app.log.Debug("resume sweep skipped a job that is not downloading",
				"job", snap.ID, "status", snap.Status, "phase", phase)
			continue
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
		exts, fault, err := app.resumeJobFiles(ctx, snap, m)
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
		// which evicts its manifest, and ReplaceFromResume needs a resident one.
		//
		// That eviction used to strand the correction. ReplaceFromResume only
		// mutated the in-memory JobProgress and set a dirty flag, so a Stall
		// here discarded a cleared bit before any save, and the next promotion
		// re-read the pre-correction row. ReplaceFromResume now persists a
		// clearing correction before it returns, which is what makes this
		// ordering safe rather than merely necessary.
		//
		// A fault also bounds what "authoritative" may mean here. exts holds
		// only the files this sweep actually resumed, and ReplaceFromResume
		// touches only those — a file the fault stopped it from reaching is
		// omitted, and omission is silence rather than a finding of absence.
		// Clearing on behalf of a file nobody read would turn one unreadable
		// mount into a full re-download of the job.
		if err := app.queue.ReplaceFromResume(snap.ID, exts); err != nil {
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

// resumeJobFiles resumes each of one job's files and converts the results into
// the extents ReplaceFromResume consumes.
//
// Only FileIdx and Durable are set, because those are the only two fields
// ReplaceFromResume reads. ResumeResult carries no Size or ModTimeNs, so filling
// the rest would manufacture a record asserting a stat nobody performed — and
// the value never reaches ExtentStore in any case (see resumeAllJobs).
//
// ResumeResult.Restart needs no branch of its own: Resume returns an empty
// bitmap alongside it, and an empty bitmap proves nothing — which under
// ReplaceFromResume returns every one of the file's articles to Outstanding,
// exactly what a missing file means. A guard here would be a branch whose two
// arms produce the same result, which is untestable by construction and
// precisely the kind of inert code this branch keeps finding.
//
// A file whose filename was never resolved is skipped and so contributes no
// extent at all, which is deliberately NOT the same thing: no process ever
// opened a path for it, so there is nothing to have proved absent, and
// ReplaceFromResume leaves a file it is not given alone.
//
// The three returns are distinct outcomes and are deliberately not collapsed
// into one error: a storage fault stalls this job and the sweep continues, a
// context error aborts the sweep entirely, and neither is the other.
//
// A fault returns the extents gathered from the files BEFORE it, not nil. Its
// caller seeds them and then stalls; see the note there for why discarding
// them turned a transient fault into a permanent loss of ground.
func (app *Application) resumeJobFiles(ctx context.Context, snap *queue.Job, m *queue.Manifest) ([]durability.FileExtent, *storagefault.Fault, error) {
	nFiles := m.NumFiles()
	exts := make([]durability.FileExtent, 0, nFiles)
	progress := snap.Progress()
	var recomputed, seeded int
	for f := range nFiles {
		// R15: checked per file rather than per job, because one file's
		// recomputation is the long operation — a shutdown arriving during a
		// multi-gigabyte CRC walk must not wait for it to finish.
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("app: resume sweep aborted: %w", err)
		}
		// A file with no resolved filename was never registered, so no
		// process ever opened a path for it and there is nothing on disk to
		// resume against. Guessing a path here would stat an unrelated file.
		filename := progress.FileFilename(f)
		if filename == "" {
			continue
		}
		path := app.pipeline.jobFilePath(snap.Name, filename)
		lo, hi := m.FileRange(f)
		//nolint:gosec // G115: file counts are far below int32
		res, err := app.resumer.Resume(ctx, snap.ID, int32(f), path, int32(lo), hi-lo)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, nil, fmt.Errorf("app: resume sweep aborted: %w", ctxErr)
			}
			return exts, storagefault.Classify("resume", path, err), nil
		}
		if res.Recomputed {
			recomputed++
		}
		seeded += res.Durable.Count()
		exts = append(exts, durability.FileExtent{
			FileIdx: int32(f), //nolint:gosec // G115: file counts are far below int32
			Durable: res.Durable,
		})
	}
	app.log.Info("resumed a job's work set from stable storage",
		"job", snap.ID, "files", len(exts), "articles_seeded", seeded,
		"files_recomputed", recomputed)
	return exts, nil, nil
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
