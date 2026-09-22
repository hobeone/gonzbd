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
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
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
func failJobIntoHistory(t *testing.T, application *Application, job *job.Job, nArticles int) {
	t.Helper()
	adminDir := application.config.GetGeneral().AdminDir
	// The backup must re-parse to the SAME shape, because
	// retainedMatchesManifest compares file count, file index order and
	// per-file article count before the retained progress is trusted.
	writeRetryNZBBackup(t, adminDir, job.ID()+".nzb.gz", retryFixtureNZB(nArticles))

	entry := history.Entry{
		NzoID:     job.ID(),
		Name:      job.Name(),
		NzbName:   job.ID() + ".nzb",
		NZBBackup: job.ID() + ".nzb.gz",
		Status:    string(constants.StatusFailed),
	}
	if err := application.TriggerPersistAndCommit(slog.Default(), entry, &postproc.Job{Job: job}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}
	if nf, ne := durabilityRowCounts(t, application, job.ID()); nf != 1 || ne != 0 {
		t.Fatalf("fixture: persistAndCommit left %d runs and %d failed rows, want 1 and 0 "+
			"(a failed job keeps its runs and nothing else)", nf, ne)
	}
}

// TestRetryHistoryJob_KeepsTheDurabilityRows is #422: a retry reuses the job
// ID, resolves the same filename over the same partial file and re-fetches
// only the articles that failed, so the retained runs are what bound
// FinalizeFile's truncate to the whole file rather than to this run's few
// articles. TestPersistAndCommit_KeepsOnlyRunsForAFailedJob pins that the
// failed departure keeps them; this pins that the retry does not then drop
// them.
func TestRetryHistoryJob_KeepsTheDurabilityRows(t *testing.T) {
	t.Parallel()
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID())
	failJobIntoHistory(t, application, job, nArticles)

	if err := application.RetryHistoryJob(t.Context(), job.ID()); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	nf, _ := durabilityRowCounts(t, application, job.ID())
	if nf == 0 {
		t.Error("the retry deleted the job's durable runs. It rebuilds the same " +
			"filename over the same partial file, so with no runs the truncate " +
			"bound collapses to the re-fetched articles and the rest is destroyed")
	}
}

// TestRetryHistoryJob_ClearsTheFailedArticlesItJustReset pins that a retry
// re-attempts the articles that failed, even when failed_articles rows
// outlived the failed departure that should have reclaimed them — a reclaim
// that failed, or a checkpoint flush that wrote after it (#561).
//
// failed_articles records a decision not to fetch, and a retry exists to
// revisit it. Job.ResetForRetry clears the failed bits in memory, but the next
// hydration re-derives per-article state from durable_runs and failed_articles
// and re-marks exactly those articles Failed+Done, so a surviving row undoes
// the reset.
//
// The fixture must reach the progressApplied branch, whose runs survive: the
// surviving run asserted below is the proof, because the other branch
// discards them.
func TestRetryHistoryJob_ClearsTheFailedArticlesItJustReset(t *testing.T) {
	t.Parallel()
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	// Article 0 is covered by a durable run; article 1 is permanently failed.
	seedDurability(t, application, job.ID())
	failJobIntoHistory(t, application, job, nArticles)
	// The stray row: what a late flush writes after the departure's reclaim.
	if _, err := application.historyRepo.DB().ExecContext(t.Context(),
		`INSERT INTO failed_articles (job_id, art_idx) VALUES (?, 1)`, job.ID()); err != nil {
		t.Fatal(err)
	}

	if err := application.RetryHistoryJob(t.Context(), job.ID()); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID())
	if nf == 0 {
		t.Fatal("fixture: the retry took the !progressApplied branch, which discards " +
			"the runs. This test pins the OTHER branch, so the assertions below would " +
			"pass for the wrong reason")
	}
	if ne != 0 {
		t.Errorf("%d failed-article rows survive the retry. Job.ResetForRetry cleared "+
			"the matching bits in memory, so the next PromoteNext re-derives them from "+
			"these rows and re-marks the articles Failed+Done — the retry never "+
			"re-attempts the articles it exists to re-attempt", ne)
	}

	// The rows are the mechanism; the outcome is what matters. In dispatcher,
	// look for the failed article among the work that can be offered.
	j, ok := application.dispatcher.Job(job.ID())
	if !ok {
		t.Fatal("job not in dispatcher")
	}
	var outstanding []int32
	j.ForEachUnfinishedArticle(func(_ int, artIdx int32, _ string, _ int, _ int, _ string) bool {
		outstanding = append(outstanding, artIdx)
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
	t.Parallel()
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID())
	failJobIntoHistory(t, application, job, nArticles)

	// Swap the backup for one of a different shape, so the re-parsed manifest
	// no longer matches the retained progress.
	adminDir := application.config.GetGeneral().AdminDir
	writeRetryNZBBackup(t, adminDir, job.ID()+".nzb.gz", retryFixtureNZB(nArticles+2))

	if err := application.RetryHistoryJob(t.Context(), job.ID()); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID())
	if nf != 0 || ne != 0 {
		t.Errorf("the retry kept %d runs and %d failed rows against a manifest whose shape "+
			"changed. They are keyed on article index, so they now describe articles "+
			"that are somewhere else, and a stale row bounds the truncate", nf, ne)
	}
}

// failingDeleteRunStore delegates everything to the real store except
// DiscardRuns.
//
// Embedding the interface rather than reimplementing it keeps the stub honest:
// if the retry path grows a call to some other store method, the real one
// answers it and the test keeps testing what it says it tests.
type failingDeleteRunStore struct {
	durabilityStore
	err error
}

func (f failingDeleteRunStore) DiscardRuns(context.Context, string) error { return f.err }

// TestRetryHistoryJob_AbortsWhenStaleRowsCannotBeDropped is the other half of
// the shape-mismatch gate, and the reason the gate is worth anything.
//
// Deciding to drop the stale rows is not the same as dropping them. A retry
// that logged the failure and carried on would enqueue the job with the stale
// rows still in place — the exact state the mismatch branch exists to prevent,
// reached silently. The job is back in the queue at once, so the reclaim rule
// never reaches those rows either: they bound FinalizeFile's truncate to
// articles that are somewhere else.
//
// So the discard is fatal. The abort is clean
// because it precedes every commit: the history entry is untouched and no job
// enters the queue, which is what the second assertion pins.
func TestRetryHistoryJob_AbortsWhenStaleRowsCannotBeDropped(t *testing.T) {
	t.Parallel()
	const nArticles = 3
	application, job := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, job.ID())
	failJobIntoHistory(t, application, job, nArticles)

	// A different shape, so the retry takes the !progressApplied branch.
	adminDir := application.config.GetGeneral().AdminDir
	writeRetryNZBBackup(t, adminDir, job.ID()+".nzb.gz", retryFixtureNZB(nArticles+2))

	wantErr := errors.New("disk on fire")
	application.durable = failingDeleteRunStore{durabilityStore: application.durable, err: wantErr}

	// Assert the pre-state rather than assume it. If the job were somehow
	// still queued here, the "not queued" assertion below would pass for the
	// wrong reason and pin nothing.
	if _, queued := application.dispatcher.Job(job.ID()); queued {
		t.Fatalf("fixture: job %s is still in the dispatcher before the retry, so the "+
			"post-abort queue assertion would be vacuous", job.ID())
	}

	err := application.RetryHistoryJob(t.Context(), job.ID())
	if err == nil {
		t.Fatal("the retry reported success while the stale durability rows it decided " +
			"to drop are still in place; a stale row bounds the completion truncate to " +
			"the wrong articles and silently destroys the rest of the partial")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error does not wrap the cause: got %v, want it to wrap %v", err, wantErr)
	}
	if _, queued := application.dispatcher.Job(job.ID()); queued {
		t.Error("the retry aborted but still enqueued the job, so the download proceeds " +
			"against durability rows the abort declared untrustworthy")
	}
}
