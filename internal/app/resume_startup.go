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

// resumeAllJobs seeds every resident job's work set from what is actually on
// stable storage, and is the production caller L3 was missing.
//
// durability.Resumer and Queue.SeedFromExtents were both built and tested with
// nothing calling either, so a restart re-downloaded every byte an earlier run
// had already fsynced. This is where the two meet.
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
// It never commits an extent. The barrier is the only writer of Class B,
// because a committed extent asserts that a completed fsync stands behind it,
// and a resume proves what is on disk without performing that fsync. Paying
// the verification again on the next restart is bounded rework and is the
// correct cost.
//
// A non-resident job is skipped rather than hydrated: SeedFromExtents installs
// bits into the LIVE job's JobProgress, which requires a resident manifest,
// and hydrating the whole queue at startup would blow the residency budget
// docs/queue-lifecycle.md exists to bound. See the note on residency in
// resumeJobFiles for what that leaves uncovered.
func (app *Application) resumeAllJobs(ctx context.Context) error {
	if app.resumer == nil {
		return nil
	}
	for _, snap := range app.queue.Snapshot() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("app: resume sweep aborted: %w", err)
		}
		m, err := snap.Manifest()
		if err != nil {
			// Not resident. Nothing here can install bits into it, and that
			// is reported rather than silently skipped so the gap is visible
			// in a log rather than only in this comment.
			app.log.Debug("resume sweep skipped a non-resident job",
				"job", snap.ID, "status", snap.Status, "err", err)
			continue
		}
		exts, fault, err := app.resumeJobFiles(ctx, snap, m)
		if err != nil {
			return err
		}
		if fault != nil {
			// A1: a failure to READ the disk says nothing about any article.
			// The job stalls with a surfaced reason and every article stays
			// Outstanding; marking one permanently failed here would burn its
			// retry budget and degrade the job's reported health over a
			// condition of the device.
			//
			// Stalled even when the fault classifies permanent. At this point
			// nothing has been downloaded in this process, so there is no
			// work to protect by failing the job outright — while an EACCES
			// or EROFS on a mount that has not finished coming up at boot is
			// both common and cleared by the operator in seconds. Failing
			// would send a recoverable job to history and discard the bytes
			// an earlier run left on disk.
			app.Stall(snap.ID, fault)
			continue
		}
		if err := app.queue.SeedFromExtents(snap.ID, exts); err != nil {
			// Not fatal to startup: the job simply re-fetches what it could
			// not be told it already has.
			app.log.Warn("resume sweep could not seed a job's work set; it will re-fetch",
				"job", snap.ID, "err", err)
		}
	}
	return nil
}

// resumeJobFiles resumes each of one job's files and converts the results into
// the extents SeedFromExtents consumes.
//
// Only FileIdx and Durable are set, because those are the only two fields
// SeedFromExtents reads. ResumeResult carries no Size or ModTimeNs, so filling
// the rest would manufacture a record asserting a stat nobody performed — and
// the value never reaches ExtentStore in any case (see resumeAllJobs).
//
// ResumeResult.Restart needs no branch of its own: Resume returns an empty
// bitmap alongside it, and an empty bitmap seeds nothing. A guard here would
// be a branch whose two arms produce the same result, which is untestable by
// construction and precisely the kind of inert code this branch keeps finding.
//
// The three returns are distinct outcomes and are deliberately not collapsed
// into one error: a storage fault stalls this job and the sweep continues, a
// context error aborts the sweep entirely, and neither is the other.
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
			return nil, storagefault.Classify("resume", path, err), nil
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
