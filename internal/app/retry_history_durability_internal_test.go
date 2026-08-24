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
	"slices"
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
		t.Fatalf("fixture: persistAndCommit left %d runs and %d failed rows, want 1 and 1 "+
			"(a failed job must keep both, which is what this test then checks the retry does not undo)", nf, ne)
	}
}

// TestRetryHistoryJob_KeepsTheDurabilityRows is #422.
//
// job_finalizer.go retains a FAILED job's durability rows and states exactly
// what for: a retry reuses the job ID, resolves the same filename over the
// same partial file, and re-fetches only the articles that failed, so the
// retained rows are what bound FinalizeFile's truncate to the whole file
// rather than to this run's few articles. Without them the bound is the end
// offset of the re-fetched articles alone and the rest of the partial is
// destroyed silently.
//
// RetryHistoryJob then deleted them at the start of every retry, because
// history.Repository.Delete drops both tables unconditionally. The retention
// had no consumer on the only route that reaches it, and the silent data loss
// it exists to prevent was live on every retry.
//
// Nothing pinned that. TestPersistAndCommit_KeepsDurabilityForAFailedJob pins
// that persistAndCommit RETAINS the rows; nothing pinned what the retry then
// did to them, which is how the two halves diverged without a failure.
//
// Scoped to durable_runs. failed_articles is retained by the same finalizer
// and must NOT survive the retry, for the opposite-facing reason — see
// TestRetryHistoryJob_ClearsTheFailedArticlesItJustReset.
func TestRetryHistoryJob_KeepsTheDurabilityRows(t *testing.T) {
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID)
	failJobIntoHistory(t, application, job, nArticles)

	if err := application.RetryHistoryJob(t.Context(), job.ID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	nf, _ := durabilityRowCounts(t, application, job.ID)
	if nf == 0 {
		t.Error("the retry deleted the job's durable runs. It rebuilds the same " +
			"filename over the same partial file, so with no runs the truncate " +
			"bound collapses to the re-fetched articles and the rest is destroyed")
	}
}

// TestRetryHistoryJob_ClearsTheFailedArticlesItJustReset is the half of the
// retention that runs the OTHER way, and it is the third of Step 10's three
// reversal sites.
//
// The two tables are retained for opposite-facing reasons and the test above
// only covers one of them. durable_runs describes bytes that are still on
// disk, so a retry over the same partial file needs them. failed_articles
// describes a DECISION not to fetch — and a retry exists precisely to revisit
// that decision. Job.ResetForRetry clears every failed bit in memory; nothing
// on this path cleared the rows behind them.
//
// The undo is immediate rather than restart-only. Queue.Add writes no
// resolution, and PromoteNext unconditionally calls Store.RestoreJobProgress,
// which re-derives the per-article state from durable_runs and
// failed_articles and re-marks exactly those articles Failed+Done. So the very
// next promotion restores the state the reset just cleared and the retry never
// re-attempts the articles it was asked to.
//
// It is a regression rather than a latent gap: job_files.articles_done was
// re-serialised wholesale by the insert Add performs, so the reset corrected
// the stored copy as a side effect. A separate table has no wholesale rewrite.
//
// The fixture must reach the progressApplied branch — the other one already
// clears both tables through dropJobDurability, and pinning it would pin the
// opposite of the defect. failJobIntoHistory routes through persistAndCommit
// so history_job_files is populated and RestoreRetryProgress reports applied;
// the surviving durable run asserted below is the observable proof, because
// the !progressApplied branch would have dropped it.
func TestRetryHistoryJob_ClearsTheFailedArticlesItJustReset(t *testing.T) {
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	// Article 0 is covered by a durable run; article 1 is permanently failed.
	seedDurability(t, application, job.ID)
	failJobIntoHistory(t, application, job, nArticles)

	if err := application.RetryHistoryJob(t.Context(), job.ID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID)
	if nf == 0 {
		t.Fatal("fixture: the retry took the !progressApplied branch, which drops both " +
			"tables through dropJobDurability. This test pins the OTHER branch, so " +
			"the assertions below would pass for the wrong reason")
	}
	if ne != 0 {
		t.Errorf("%d failed-article rows survive the retry. Job.ResetForRetry cleared "+
			"the matching bits in memory, so the next PromoteNext re-derives them from "+
			"these rows and re-marks the articles Failed+Done — the retry never "+
			"re-attempts the articles it exists to re-attempt", ne)
	}

	// The rows are the mechanism; the outcome is what matters. Promote the
	// job so RestoreJobProgress actually runs, then look for the failed
	// article among the work the dispatcher would be offered.
	application.queue.PromoteNext(t.Context())
	var outstanding []int32
	application.queue.ForEachUnfinishedArticle(func(a queue.UnfinishedArticle) bool {
		if a.JobID == job.ID {
			outstanding = append(outstanding, a.ArtIdx)
		}
		return true
	})
	if !slices.Contains(outstanding, 1) {
		t.Errorf("article 1 is not Outstanding after the retry was promoted "+
			"(outstanding = %v). It is the article the retry was asked to re-attempt, "+
			"and RestoreJobProgress has put it back to Failed+Done from a row the "+
			"reset should have removed", outstanding)
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
		t.Errorf("the retry kept %d runs and %d failed rows against a manifest whose shape "+
			"changed. They are keyed on article index, so they now describe articles "+
			"that are somewhere else, and a stale row bounds the truncate", nf, ne)
	}
}

// failingDeleteRunStore delegates everything to a real RunStore except
// DeleteJob.
//
// Embedding the interface rather than reimplementing it keeps the stub honest:
// if the retry path grows a call to some other RunStore method, the real one
// answers it and the test keeps testing what it says it tests.
type failingDeleteRunStore struct {
	durability.RunStore
	err error
}

func (f failingDeleteRunStore) DeleteJob(context.Context, string) error { return f.err }

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
	application.runs = failingDeleteRunStore{RunStore: application.runs, err: wantErr}

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
