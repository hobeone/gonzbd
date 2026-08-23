package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

// retryFixtureNZB renders a minimal NZB with one file of nArticles articles.
func retryFixtureNZB(nArticles int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	b.WriteString(`<file poster="p@t" date="1700000000" subject="&quot;file.bin&quot; yEnc (1/1)">` + "\n")
	b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
	for i := 1; i <= nArticles; i++ {
		fmt.Fprintf(&b, `<segment bytes="1024" number="%d">a%d@t</segment>`+"\n", i, i)
	}
	b.WriteString("</segments>\n</file>\n</nzb>\n")
	return []byte(b.String())
}

// writeRetryNZBBackup gzips raw to adminDir/nzb/<name>, which is where
// rebuildJobFromNZB looks for it.
func writeRetryNZBBackup(t *testing.T, adminDir, name string, raw []byte) {
	t.Helper()
	nzbDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		t.Fatalf("mkdir nzb: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nzbDir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write gz nzb: %v", err)
	}
}

// failJobIntoHistory runs a real job through persistAndCommit as FAILED, which
// is what retains both halves the retry then needs: history_job_files, carried
// over from job_files by MoveToHistory, and the durability rows job_finalizer
// declines to delete.
//
// Hand-seeding a history entry instead does not exercise this. It leaves
// history_job_files empty, so RestoreRetryProgress finds nothing to restore
// and reports applied=false — which correctly discards the durability rows as
// untrustworthy, and pins the opposite of what this test is for.
func failJobIntoHistory(t *testing.T, application *Application, job *queue.Job, nArticles int) {
	t.Helper()
	adminDir := application.config.GetGeneral().AdminDir
	// The backup must re-parse to the SAME shape, because
	// retainedMatchesManifest compares file count, file index order and
	// per-file article count before the retained progress is trusted.
	writeRetryNZBBackup(t, adminDir, job.ID+".nzb.gz", retryFixtureNZB(nArticles))

	entry := history.Entry{
		NzoID:     job.ID,
		Name:      job.Name,
		NzbName:   job.ID + ".nzb",
		NZBBackup: job.ID + ".nzb.gz",
		Status:    string(constants.StatusFailed),
	}
	if err := application.TriggerPersistAndCommit(slog.Default(), entry, &postproc.Job{Queue: job}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}
	if nf, ne := durabilityRowCounts(t, application, job.ID); nf != 1 || ne != 1 {
		t.Fatalf("fixture: persistAndCommit left %d facts and %d extents, want 1 and 1 "+
			"(a failed job must keep both, which is what this test then checks the retry does not undo)", nf, ne)
	}
}

// TestRetryHistoryJob_KeepsTheDurabilityRows is #422.
//
// job_finalizer.go retains a FAILED job's durability rows and states exactly
// what for: a retry reuses the job ID, resolves the same filename over the
// same partial file, and re-fetches only the articles that failed, so the
// retained rows are what bound FinalizeFile's truncate to the whole file
// rather than to this run's few articles. Without them durableExtent returns
// the end offset of the re-fetched articles alone and the rest of the partial
// is destroyed silently — neither #342 guard fires, because every article the
// fact log knows about IS durable.
//
// RetryHistoryJob then deleted them at the start of every retry, because
// history.Repository.Delete drops both tables unconditionally. The retention
// had no consumer on the only route that reaches it, and the silent data loss
// it exists to prevent was live on every retry.
//
// Nothing pinned that. TestPersistAndCommit_KeepsDurabilityForAFailedJob pins
// that persistAndCommit RETAINS the rows; nothing pinned what the retry then
// did to them, which is how the two halves diverged without a failure.
func TestRetryHistoryJob_KeepsTheDurabilityRows(t *testing.T) {
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID)
	failJobIntoHistory(t, application, job, nArticles)

	if err := application.RetryHistoryJob(t.Context(), job.ID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID)
	if nf == 0 {
		t.Error("the retry deleted the job's Class A facts. It rebuilds the same " +
			"filename over the same partial file, so with no facts the truncate " +
			"bound collapses to the re-fetched articles and the rest is destroyed")
	}
	if ne == 0 {
		t.Error("the retry deleted the job's Class B extents, so priorExtent starts " +
			"from an empty bitmap and every article is re-fetched")
	}
}

// TestRetryHistoryJob_DiscardsRowsWhenTheManifestShapeChanged is the other half
// of the retention, and the reason keeping the rows is safe at all.
//
// The rows are keyed on (job_id, art_idx) and a retry re-parses the NZB backup,
// so the numbering is re-derived rather than carried. If it comes back a
// different shape, a retained row names an article that is no longer at that
// index. RestoreRetryProgress already refuses the per-file overlay on a shape
// mismatch — retainedMatchesManifest compares file count, index order and
// per-file article count — but refusing is all it does: it deletes nothing.
//
// So naming the check is not enough. Keeping the durability rows unconditionally
// would let them outlive exactly the renumbering that invalidates them, which is
// worse than the bug being fixed: a stale row is authoritative for a truncate
// bound, where a missing one only costs a re-fetch.
func TestRetryHistoryJob_DiscardsRowsWhenTheManifestShapeChanged(t *testing.T) {
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID)
	failJobIntoHistory(t, application, job, nArticles)

	// Swap the backup for one of a different shape, so the re-parsed manifest
	// no longer matches the retained progress.
	adminDir := application.config.GetGeneral().AdminDir
	writeRetryNZBBackup(t, adminDir, job.ID+".nzb.gz", retryFixtureNZB(nArticles+2))

	if err := application.RetryHistoryJob(t.Context(), job.ID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID)
	if nf != 0 || ne != 0 {
		t.Errorf("the retry kept %d facts and %d extents against a manifest whose shape "+
			"changed. They are keyed on article index, so they now describe articles "+
			"that are somewhere else, and a stale row bounds the truncate", nf, ne)
	}
}

// failingDeleteFactLog delegates everything to a real FactLog except DeleteJob.
//
// Embedding the interface rather than reimplementing it keeps the stub honest:
// if the retry path grows a call to some other FactLog method, the real one
// answers it and the test keeps testing what it says it tests.
type failingDeleteFactLog struct {
	durability.FactLog
	err error
}

func (f failingDeleteFactLog) DeleteJob(context.Context, string) error { return f.err }

// TestRetryHistoryJob_AbortsWhenStaleRowsCannotBeDropped is the other half of
// the shape-mismatch gate, and the reason the gate is worth anything.
//
// Deciding to drop the stale rows is not the same as dropping them.
// deleteJobDurability logs its failures and returns nothing, so a retry that
// used it would enqueue the job with the stale rows still in place — the exact
// state the mismatch branch exists to prevent, reached silently.
//
// Nothing else catches it. SQLiteStore.pruneDurabilityRows is the backstop for
// orphaned rows, but it deliberately skips any job_id still present in `jobs`,
// and queue.Add puts this job back there immediately. A row that survives the
// deletion survives for the life of the job and bounds FinalizeFile's truncate
// to articles that are somewhere else.
//
// So the deletion is fatal here, alone among its callers. The abort is clean
// because it precedes every commit: the history entry is untouched and no job
// enters the queue, which is what the second assertion pins.
func TestRetryHistoryJob_AbortsWhenStaleRowsCannotBeDropped(t *testing.T) {
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID)
	failJobIntoHistory(t, application, job, nArticles)

	// A different shape, so the retry takes the !progressApplied branch.
	adminDir := application.config.GetGeneral().AdminDir
	writeRetryNZBBackup(t, adminDir, job.ID+".nzb.gz", retryFixtureNZB(nArticles+2))

	wantErr := errors.New("disk on fire")
	application.factLog = failingDeleteFactLog{FactLog: application.factLog, err: wantErr}

	// Assert the pre-state rather than assume it. If the job were somehow
	// still queued here, the "not queued" assertion below would pass for the
	// wrong reason and pin nothing.
	if snap := application.queue.SnapshotJob(job.ID); snap != nil {
		t.Fatalf("fixture: job %s is still in the queue before the retry, so the "+
			"post-abort queue assertion would be vacuous", job.ID)
	}

	err := application.RetryHistoryJob(t.Context(), job.ID)
	if err == nil {
		t.Fatal("the retry reported success while the stale durability rows it decided " +
			"to drop are still in place; a stale row bounds the completion truncate to " +
			"the wrong articles and silently destroys the rest of the partial")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error does not wrap the cause: got %v, want it to wrap %v", err, wantErr)
	}
	if snap := application.queue.SnapshotJob(job.ID); snap != nil {
		t.Error("the retry aborted but still enqueued the job, so the download proceeds " +
			"against durability rows the abort declared untrustworthy")
	}
}
