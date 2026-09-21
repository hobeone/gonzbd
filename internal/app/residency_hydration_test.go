package app

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
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
	if _, err := db.Exec(`INSERT INTO durable_runs (job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32) VALUES ('j1', 0, 0, 1, 0, 200, 0)`); err != nil {
		t.Fatalf("insert durable_runs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO failed_articles (job_id, art_idx) VALUES ('j1', 'not-an-int')`); err != nil {
		t.Fatalf("insert failed_articles: %v", err)
	}

	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, t.TempDir(), durability.NewStore(db), slog.New(slog.DiscardHandler))
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
	hdb, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "res.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = hdb.Close() })
	return history.NewRepository(hdb).DB()
}

// TestRestoreResolution_AppliesNothingWhenARunCannotBeScanned pins the other
// half of the read policy: a durable_runs row that cannot be scanned abandons
// the resolution, failed marks included, rather than applying the failed
// articles against no runs at all. Only a partial read — one wrapping
// durability.ErrIncomplete — is applied.
func TestRestoreResolution_AppliesNothingWhenARunCannotBeScanned(t *testing.T) {
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
	if _, err := db.Exec(`INSERT INTO durable_runs (job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32) VALUES ('j1', 0, 'x', 1, 0, 200, 0)`); err != nil {
		t.Fatalf("insert durable_runs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO failed_articles (job_id, art_idx) VALUES ('j1', 2)`); err != nil {
		t.Fatalf("insert failed_articles: %v", err)
	}

	r := newAppResidency(func(string) (*job.Job, bool) { return j, true }, t.TempDir(), durability.NewStore(db), slog.New(slog.DiscardHandler))
	r.restoreResolution(context.Background(), j)

	if j.Progress().ArticleFailed(2) {
		t.Error("article 2 was marked failed although the runs could not be read; " +
			"an unreadable run record must abandon the whole resolution")
	}
}
