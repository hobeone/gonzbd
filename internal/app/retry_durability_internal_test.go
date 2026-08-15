package app

import (
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// seedDurability gives a job one Class A fact and one Class B extent, standing
// in for a run that got some of the file onto disk.
func seedDurability(t *testing.T, application *Application, jobID string) {
	t.Helper()
	if err := application.factLog.Append(t.Context(), jobID, []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 7},
	}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}
	bm := durability.NewBitmap(2)
	bm.Set(0)
	if err := application.extents.Commit(t.Context(), jobID, []durability.FileExtent{
		{FileIdx: 0, Durable: bm, BytesDurable: 100, Size: 100, ModTimeNs: 1},
	}); err != nil {
		t.Fatalf("seed extents: %v", err)
	}
}

func durabilityRowCounts(t *testing.T, application *Application, jobID string) (int, int) {
	t.Helper()
	facts, err := application.factLog.ForFile(t.Context(), jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	exts, err := application.extents.Load(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return len(facts), len(exts)
}

// TestPersistAndCommit_KeepsDurabilityForAFailedJob pins the retention that
// stops a retry truncating away everything the first attempt downloaded.
//
// jobFinalizer.finalize runs for failures as well as successes, and
// persistAndCommit ended in deleteJobDurability unconditionally. MoveToHistory
// meanwhile RETAINS a failed job's job_files rows — filename, articles_done,
// assembled_crc32 — and the partial file stays on disk. So a retry resolved the
// same filename, restored the done bits, and set TotalParts from
// CountUnfinishedArticles: only the articles that failed.
//
// When the last of those landed, FinalizeFile found no stored extent and a fact
// log holding only this run's facts, so durableExtent returned the end offset
// of the re-fetched article alone. Neither #342 guard fires — missing is 0
// because that article IS durable, and unrecorded is 0 because durable.Count()
// equals len(covered) — so Truncate shrank the file to that bound. A 75 MB file
// that failed on one article came back as roughly 4.5 MB, silently.
//
// On main this was unreachable: migration 011 carried max_written into
// history_job_files precisely so a retry could not lose it. That column and the
// assembler's maxWritten seed are both gone here, so the retained facts ARE the
// replacement — which makes their retention load-bearing rather than tidy.
func TestPersistAndCommit_KeepsDurabilityForAFailedJob(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)
	seedDurability(t, application, job.ID)

	if nf, ne := durabilityRowCounts(t, application, job.ID); nf != 1 || ne != 1 {
		t.Fatalf("fixture seeded %d facts and %d extents, want 1 and 1", nf, ne)
	}

	entry := history.Entry{NzoID: job.ID, Name: job.Name, Status: string(constants.StatusFailed)}
	if err := application.TriggerPersistAndCommit(slog.Default(), entry, &postproc.Job{Queue: job}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID)
	if nf == 0 {
		t.Error("a failed job's Class A facts were deleted. A retry rebuilds the same " +
			"filename over the same partial file, so with no facts the truncate bound " +
			"collapses to the re-fetched articles and the rest of the file is destroyed")
	}
	if ne == 0 {
		t.Error("a failed job's Class B extents were deleted, so priorExtent starts " +
			"from an empty bitmap on the retry")
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
	application, job := newDurabilityTestApp(t, 1, 2)
	seedDurability(t, application, job.ID)

	entry := history.Entry{NzoID: job.ID, Name: job.Name, Status: string(constants.StatusCompleted)}
	if err := application.TriggerPersistAndCommit(slog.Default(), entry, &postproc.Job{Queue: job}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID)
	if nf != 0 || ne != 0 {
		t.Errorf("a completed job left %d facts and %d extents behind; nothing will "+
			"ever read them again and they accumulate one set per download", nf, ne)
	}
}
