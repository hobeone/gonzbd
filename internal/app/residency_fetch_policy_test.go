package app

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestRestoreJobFiles_RestoresNonDefaultFetchPolicy pins that hydrating a
// resident job over default progress applies the persisted fetch_policy, not
// just filename/complete/crc. Every existing test that inserts job_files rows
// writes FetchAlways, which is why none of them would catch a dropped
// RestoreFetchPolicy call (see the plan's Task 1 note on this).
//
// The job must hydrate over DEFAULT progress: Job.Evict nils only the
// manifest and leaves JobProgress intact, so evicting and re-hydrating an
// already-live job would already carry the right policy in memory and this
// test would pass even with the restore neutered.
func TestRestoreJobFiles_RestoresNonDefaultFetchPolicy(t *testing.T) {
	db, err := history.Open(t.Context(), t.TempDir()+"/history.db")
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	j := job.New("job-1", "test", job.Policy{})
	m := job.NewManifest([]job.JobFile{
		{Subject: "payload.rar", Bytes: 100, Articles: []job.JobArticle{{ID: "a0", Bytes: 100, Number: 1}}},
		{Subject: "recovery.vol000+01.par2", Bytes: 50, Articles: []job.JobArticle{{ID: "a1", Bytes: 50, Number: 1}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if j.Progress().FileFetchPolicy(1) != job.FetchAlways {
		t.Fatal("precondition: freshly attached progress must start FetchAlways")
	}

	if _, err := repo.DB().Exec(
		`INSERT INTO job_files (job_id, file_index, complete, filename, assembled_crc32, fetch_policy)
		 VALUES (?, 0, 0, '', 0, ?), (?, 1, 0, '', 0, ?)`,
		"job-1", int(job.FetchAlways), "job-1", int(job.FetchIfNeeded)); err != nil {
		t.Fatalf("insert job_files: %v", err)
	}

	r := newAppResidency(func(id string) (*job.Job, bool) {
		if id == "job-1" {
			return j, true
		}
		return nil, false
	}, t.TempDir(), repo.DB(), slog.New(slog.DiscardHandler))

	r.restoreJobFiles(context.Background(), j)

	if got := j.Progress().FileFetchPolicy(1); got != job.FetchIfNeeded {
		t.Errorf("FileFetchPolicy(1) = %v after restoreJobFiles, want FetchIfNeeded (0x%x) — "+
			"the persisted policy was not applied", got, job.FetchIfNeeded)
	}
}

// TestRestoreJobFiles_QueryErrorLeavesJobUntouched pins that a failed
// job_files query is logged and returns without panicking or mutating the
// job — a closed *sql.DB is a stand-in for the real failures here (a
// dropped connection, a busy timeout).
func TestRestoreJobFiles_QueryErrorLeavesJobUntouched(t *testing.T) {
	db, err := history.Open(t.Context(), t.TempDir()+"/history.db")
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)
	if err := repo.DB().Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	j := job.New("job-closed-db", "test", job.Policy{})
	m := job.NewManifest([]job.JobFile{
		{Subject: "payload.rar", Bytes: 100, Articles: []job.JobArticle{{ID: "a0", Bytes: 100, Number: 1}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, t.TempDir(), repo.DB(), slog.New(slog.DiscardHandler))
	r.restoreJobFiles(context.Background(), j) // must not panic

	if got := j.Progress().FileFetchPolicy(0); got != job.FetchAlways {
		t.Errorf("FileFetchPolicy(0) = %v after a failed query, want unchanged FetchAlways", got)
	}
}

// TestRestoreJobFiles_NilProgressReturnsEarly pins that a job with no
// attached content (Progress() == nil) is left alone rather than panicking
// on a nil progress dereference inside the restore loop.
func TestRestoreJobFiles_NilProgressReturnsEarly(t *testing.T) {
	db, err := history.Open(t.Context(), t.TempDir()+"/history.db")
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	j := job.New("job-no-content", "test", job.Policy{})
	if j.Progress() != nil {
		t.Fatal("precondition: an unattached job must report nil progress")
	}

	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, t.TempDir(), repo.DB(), slog.New(slog.DiscardHandler))
	r.restoreJobFiles(context.Background(), j) // must not panic
}

// TestRestoreJobFiles_ScanErrorSkipsRowAndContinues pins that a job_files
// row whose fetch_policy cannot be scanned into an int is skipped rather
// than aborting the whole restore — a later well-formed row for a different
// file must still be applied.
func TestRestoreJobFiles_ScanErrorSkipsRowAndContinues(t *testing.T) {
	db, err := history.Open(t.Context(), t.TempDir()+"/history.db")
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	j := job.New("job-bad-scan", "test", job.Policy{})
	m := job.NewManifest([]job.JobFile{
		{Subject: "payload.rar", Bytes: 100, Articles: []job.JobArticle{{ID: "a0", Bytes: 100, Number: 1}}},
		{Subject: "recovery.vol000+01.par2", Bytes: 50, Articles: []job.JobArticle{{ID: "a1", Bytes: 50, Number: 1}}},
	})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	// 1.5 satisfies the fetch_policy BETWEEN 0 AND 2 CHECK (SQLite's INTEGER
	// affinity stores it as a REAL, which still compares true against the
	// CHECK), but database/sql refuses to Scan a non-integral float64 into
	// an *int — the scan error this test needs without violating the CHECK.
	if _, err := repo.DB().Exec(
		`INSERT INTO job_files (job_id, file_index, complete, filename, assembled_crc32, fetch_policy)
		 VALUES (?, 0, 0, '', 0, 1.5), (?, 1, 0, '', 0, ?)`,
		"job-bad-scan", "job-bad-scan", int(job.FetchIfNeeded)); err != nil {
		t.Fatalf("insert job_files: %v", err)
	}

	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, t.TempDir(), repo.DB(), slog.New(slog.DiscardHandler))
	r.restoreJobFiles(context.Background(), j)

	if got := j.Progress().FileFetchPolicy(0); got != job.FetchAlways {
		t.Errorf("FileFetchPolicy(0) = %v after an unscannable row, want unchanged FetchAlways", got)
	}
	if got := j.Progress().FileFetchPolicy(1); got != job.FetchIfNeeded {
		t.Errorf("FileFetchPolicy(1) = %v, want FetchIfNeeded — a scan error on one row "+
			"must not stop the loop from applying a later well-formed row", got)
	}
}
