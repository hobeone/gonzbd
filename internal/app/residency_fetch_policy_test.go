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
