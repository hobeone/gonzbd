package app

import (
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// seedDurability gives a job a row in each per-job table: one written article
// and a job_files row, standing in for a run that got some of the file onto
// disk.
func seedDurability(t *testing.T, application *Application, jobID string) {
	t.Helper()
	st := realStore(t, application)
	if err := st.Admit(t.Context(), jobID, []uint8{0}); err != nil {
		t.Fatalf("seed job_files: %v", err)
	}
	seedWritten(t, st, jobID, []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 7},
	})
}

// durabilityRowCounts reports how many written_articles and job_files rows a
// job still has.
func durabilityRowCounts(t *testing.T, application *Application, jobID string) (written, files int) {
	t.Helper()
	rows, err := application.durable.WrittenRows(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows), jobFilesCount(t, application, jobID)
}

// TestPersistAndCommit_KeepsTheRecordForAFailedJob pins what a failed job
// keeps when it leaves the queue: its written_articles and job_files rows,
// which a retry verifies against the partial file so it refetches only what
// is missing.
func TestPersistAndCommit_KeepsTheRecordForAFailedJob(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	seedDurability(t, application, job.ID())

	if nw, nf := durabilityRowCounts(t, application, job.ID()); nw != 1 || nf != 1 {
		t.Fatalf("fixture seeded %d written rows and %d job_files rows, want 1 of each", nw, nf)
	}

	entry := history.Entry{NzoID: job.ID(), Name: job.Name(), Status: string(constants.StatusFailed)}
	if err := application.TriggerPersistAndCommit(slog.Default(), entry, &postproc.Job{Job: job}); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}

	nw, nf := durabilityRowCounts(t, application, job.ID())
	if nw == 0 {
		t.Error("a failed job's written_articles rows were deleted; a retry has nothing " +
			"to verify and refetches every article")
	}
	if nf == 0 {
		t.Error("a failed job's job_files rows were deleted; a retry cannot name the files its rows belong to")
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

	nw, nf := durabilityRowCounts(t, application, job.ID())
	if nw != 0 || nf != 0 {
		t.Errorf("a completed job left %d written rows and %d job_files rows behind; nothing will "+
			"ever read them again and they accumulate one set per download", nw, nf)
	}
}
