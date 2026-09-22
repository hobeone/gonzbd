package app

import (
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// seedDurability gives a job a row in each per-job table: one durable run, one
// permanently failed article, and a job_files row, standing in for a run that
// got some of the file onto disk and lost one article for good.
func seedDurability(t *testing.T, application *Application, jobID string) {
	t.Helper()
	st := realStore(t, application)
	commitRuns(t, st, jobID, []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 7},
	})
	if err := st.Admit(t.Context(), jobID, []uint8{0}); err != nil {
		t.Fatalf("seed job_files: %v", err)
	}
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

// TestPersistAndCommit_KeepsOnlyRunsForAFailedJob pins what a failed job keeps
// when it leaves the queue. A retry rebuilds the same filename over the same
// partial file and bounds FinalizeFile's truncate by the retained runs;
// without them the bound collapses to the re-fetched articles and the rest of
// the file is destroyed (#422). Nothing else is kept.
func TestPersistAndCommit_KeepsOnlyRunsForAFailedJob(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	seedDurability(t, application, job.ID())

	if nf, ne := durabilityRowCounts(t, application, job.ID()); nf != 1 || ne != 1 || jobFilesCount(t, application, job.ID()) != 1 {
		t.Fatalf("fixture seeded %d runs and %d failed rows (and %d job_files rows), want 1 of each",
			nf, ne, jobFilesCount(t, application, job.ID()))
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
	// Only the runs. A retry exists to re-attempt the failed articles, and it
	// restores file progress from history_job_files, not from job_files.
	if ne != 0 {
		t.Errorf("a failed job kept %d failed-article rows; a retry would read them "+
			"back as failed and never re-attempt those articles", ne)
	}
	if n := jobFilesCount(t, application, job.ID()); n != 0 {
		t.Errorf("a failed job kept %d job_files rows; nothing reads them once it has left the queue", n)
	}
}

// TestPersistAndCommit_DropsDurabilityForACompletedJob is the other half, and
// it is what keeps the retention bounded: a completed job has nothing to
// retry, so its rows would accumulate one set per download ever performed.
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
	if n := jobFilesCount(t, application, job.ID()); n != 0 {
		t.Errorf("a completed job left %d job_files rows behind", n)
	}
}
