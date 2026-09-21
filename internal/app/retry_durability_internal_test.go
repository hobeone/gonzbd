package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// seedDurability gives a job one durable run and one permanently failed
// article, standing in for a run that got some of the file onto disk and lost
// one article for good.
//
// Both tables, because the retention rules have to agree about them: the
// barrier writes durable_runs, and the checkpointer writes failed_articles.
func seedDurability(t *testing.T, application *Application, jobID string) {
	t.Helper()
	commitRuns(t, realStore(t, application), jobID, []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 7},
	})
	if _, err := application.historyRepo.DB().ExecContext(t.Context(),
		`INSERT INTO failed_articles (job_id, art_idx) VALUES (?, 1)`, jobID); err != nil {
		t.Fatalf("seed failed articles: %v", err)
	}
}

// durabilityRowCounts reports how many durable runs and failed-article rows a
// job still has.
func durabilityRowCounts(t *testing.T, application *Application, jobID string) (int, int) {
	t.Helper()
	runs, err := application.durable.ForJob(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	var failed int
	if err := application.historyRepo.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM failed_articles WHERE job_id = ?`, jobID).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	return len(runs), failed
}

// TestDropJobDurability_ReportsBothOwnersFailures pins the difference between
// this and deleteJobDurability, which calls it. They differ in two ways now:
// whether the caller is told, and job_files -- deleteJobDurability drops it
// too, and this does not, leaving RetryHistoryJob's re-seed to settle those
// rows. (A retry's retained PROGRESS comes from history_job_files, not from
// job_files -- see dropJobDurability's own comment.)
//
// The two entry points exist because their callers need opposite things. For a
// DEPARTED job the rows are garbage and there is no caller left to tell, so
// deleteJobDurability swallows -- which costs the disk permanently while no
// backstop exists (#549), not merely until one runs. For a
// job coming BACK the rows are about to be READ, and a stale one bounds
// FinalizeFile's truncate to the wrong article range — so RetryHistoryJob
// aborts on a failure here rather than requeueing (#422).
//
// Both errors are joined rather than the first winning, because the two
// deletions are independent calls. A caller deciding whether to abort is
// better served by the whole picture, and an early return would leave one
// table's rows behind for a job that is about to be re-downloaded over them.
func TestDropJobDurability_ReportsBothOwnersFailures(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	seedDurability(t, application, job.ID())

	// The success path first, so the failure assertions below cannot pass
	// against a function that never removes anything.
	if err := application.dropJobDurability(t.Context(), job.ID()); err != nil {
		t.Fatalf("dropJobDurability: %v", err)
	}
	if nr, nf := durabilityRowCounts(t, application, job.ID()); nr != 0 || nf != 0 {
		t.Fatalf("%d runs and %d failed rows survive the drop", nr, nf)
	}

	// Re-seed BEFORE the failing store goes in: the success path above emptied
	// both tables, and seedDurability writes durable_runs through
	// application.durable, which is about to start refusing run operations.
	seedDurability(t, application, job.ID())

	boom := errors.New("database is locked")
	application.durable = failingRunStore{durabilityStore: application.durable, err: boom}

	err := application.dropJobDurability(t.Context(), job.ID())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the run store's failure — RetryHistoryJob "+
			"aborts on this, and a swallowed failure requeues the job over stale rows", err)
	}
	if !strings.Contains(err.Error(), "durable runs") {
		t.Errorf("err = %q, want it to name which owner failed", err)
	}
	// The independence the doc claims, asserted rather than described: the
	// second deletion runs even though the first failed. Without this the
	// test passes against a dropJobDurability that returns on the first
	// error, which is the shape the comment above exists to rule out.
	// Queried directly rather than through durabilityRowCounts, whose run count
	// goes through application.durable -- now the refusing store.
	var nf int
	if err := application.historyRepo.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM failed_articles WHERE job_id = ?`, job.ID()).Scan(&nf); err != nil {
		t.Fatal(err)
	}
	if nf != 0 {
		t.Errorf("%d failed-article rows survive a drop whose RUN deletion failed; the "+
			"second deletion was skipped, so a retry downloads over rows nothing cleared", nf)
	}

	// Both failing at once. failingRunStore passes DiscardFailedArticles
	// through to the real store, so that failure is injected by taking the
	// table away, and it is the real delete that fails.
	if _, err := application.historyRepo.DB().ExecContext(t.Context(),
		`ALTER TABLE failed_articles RENAME TO failed_articles_hidden`); err != nil {
		t.Fatalf("hide failed_articles: %v", err)
	}
	t.Cleanup(func() {
		_, _ = application.historyRepo.DB().ExecContext(context.WithoutCancel(t.Context()),
			`ALTER TABLE failed_articles_hidden RENAME TO failed_articles`)
	})

	both := application.dropJobDurability(t.Context(), job.ID())
	if !errors.Is(both, boom) {
		t.Errorf("err = %v, want it to still wrap the run store's failure when both fail", both)
	}
	if !strings.Contains(both.Error(), "failed articles") {
		t.Errorf("err = %q, want it to name the failed-article deletion too — a caller "+
			"deciding whether to abort is told about one failure, not both", both)
	}
}

// TestPersistAndCommit_KeepsDurabilityForAFailedJob pins the retention that
// stops a retry truncating away everything the first attempt downloaded.
//
// jobFinalizer.finalize runs for failures as well as successes, and
// persistAndCommit ended in deleteJobDurability unconditionally. MoveToHistory
// meanwhile RETAINS a failed job's job_files rows — filename, complete,
// assembled_crc32 — and the partial file stays on disk. So a retry resolved the
// same filename, restored the done bits, and set TotalParts from
// CountUnfinishedArticles: only the articles that failed.
//
// When the last of those landed, FinalizeFile found nothing recorded for the
// file beyond this run's own articles, so the truncate bound was the end
// offset of the re-fetched article alone and Truncate shrank the file to it. A
// 75 MB file that failed on one article came back as roughly 4.5 MB, silently.
//
// On main this was unreachable: migration 011 carried max_written into
// history_job_files precisely so a retry could not lose it. That column and the
// assembler's maxWritten seed are both gone here, so the retained RUNS are the
// replacement — which makes their retention load-bearing rather than tidy.
func TestPersistAndCommit_KeepsDurabilityForAFailedJob(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	seedDurability(t, application, job.ID())

	if nf, ne := durabilityRowCounts(t, application, job.ID()); nf != 1 || ne != 1 {
		t.Fatalf("fixture seeded %d runs and %d failed rows, want 1 and 1", nf, ne)
	}

	entry := history.Entry{NzoID: job.ID(), Name: job.Name(), Status: string(constants.StatusFailed)}
	if err := application.TriggerPersistAndCommit(slog.Default(), entry, &postproc.Job{Job: job}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID())
	if nf == 0 {
		t.Error("a failed job's durable runs were deleted. A retry rebuilds the same " +
			"filename over the same partial file, so with no runs the truncate bound " +
			"collapses to the re-fetched articles and the rest of the file is destroyed")
	}
	if ne == 0 {
		t.Error("a failed job's failed-article rows were deleted, so the retry " +
			"re-attempts articles that already failed permanently")
	}
}

// TestPersistAndCommit_DropsDurabilityForACompletedJob is the other half, and
// it is what keeps the retention bounded.
//
// A completed job has nothing to retry, so its rows would accumulate one set
// per download ever performed. MoveToHistory applies exactly this rule to
// job_files; this keeps the two in step, so "retained for retry" means the same
// thing on both sides.
func TestPersistAndCommit_DropsDurabilityForACompletedJob(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	seedDurability(t, application, job.ID())

	entry := history.Entry{NzoID: job.ID(), Name: job.Name(), Status: string(constants.StatusCompleted)}
	if err := application.TriggerPersistAndCommit(slog.Default(), entry, &postproc.Job{Job: job}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID())
	if nf != 0 || ne != 0 {
		t.Errorf("a completed job left %d runs and %d failed rows behind; nothing will "+
			"ever read them again and they accumulate one set per download", nf, ne)
	}
}
