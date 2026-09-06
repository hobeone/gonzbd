package app

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestRestoreResolution_KeepsRunsWhenFailedArticleScanFails pins that a scan
// error while reading failed_articles does not discard the durable runs that
// were already read from durable_runs.
//
// The failure this guards is a control-flow one: returning from the scan-error
// branch exits restoreResolution before ApplyResolution is ever called, so a
// single unreadable failed_articles row costs the job its entire durable
// resolution and every recorded byte range is re-fetched.
func TestRestoreResolution_KeepsRunsWhenFailedArticleScanFails(t *testing.T) {
	db := newResolutionTestDB(t)

	j := job.New("j1", "test", job.Policy{})
	m := job.NewManifest([]job.JobFile{{
		Subject: "f.rar",
		Bytes:   300,
		Articles: []job.JobArticle{
			{ID: "a0", Bytes: 100, Number: 1},
			{ID: "a1", Bytes: 100, Number: 2},
			{ID: "a2", Bytes: 100, Number: 3},
		},
	}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	before := j.RemainingBytes()
	if before == 0 {
		t.Fatal("setup: no remaining bytes to reduce")
	}

	// Two articles are durably on disk, and failed_articles holds a row whose
	// art_idx cannot be scanned into an int32.
	if _, err := db.Exec(`INSERT INTO durable_runs (job_id, first_art_idx, last_art_idx) VALUES ('j1', 0, 1)`); err != nil {
		t.Fatalf("insert durable_runs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO failed_articles (job_id, art_idx) VALUES ('j1', 'not-an-int')`); err != nil {
		t.Fatalf("insert failed_articles: %v", err)
	}

	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, t.TempDir(), db, slog.New(slog.DiscardHandler))
	r.restoreResolution(context.Background(), j)

	after := j.RemainingBytes()
	if after >= before {
		t.Fatalf("RemainingBytes = %d, want < %d — the durable runs read before the "+
			"failed_articles scan error were discarded, so the job will re-fetch bytes "+
			"already on disk", after, before)
	}
}

func newResolutionTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/res.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE durable_runs (job_id TEXT, first_art_idx INTEGER, last_art_idx INTEGER)`,
		`CREATE TABLE failed_articles (job_id TEXT, art_idx)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
}
