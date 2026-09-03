package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

func writeTestManifest(t *testing.T, path string, _ *job.Job) {
	t.Helper()
	content := `{"files":[{"subject":"test.rar","bytes":100,"articles":[{"id":"m1","bytes":100,"number":1}]}]}`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestAppResidency_HydrateThenEvict pins the contract dispatch.Residency
// states: Hydrate makes the manifest available and may block on disk; Evict
// takes it away. The dispatcher decides WHEN and delegates WHAT to here.
func TestAppResidency_HydrateThenEvict(t *testing.T) {
	dir := t.TempDir()
	j := job.New("abc123", "test", job.PolicyFromPP(3))
	writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j)

	r := newAppResidency(func(id string) (*job.Job, bool) {
		if id == "abc123" {
			return j, true
		}
		return nil, false
	}, dir, nil, nil)

	if j.Resident() {
		t.Fatal("precondition: job must start non-resident")
	}
	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if !j.Resident() {
		t.Fatal("Hydrate must leave the job resident")
	}

	r.Evict("abc123")
	if j.Resident() {
		t.Fatal("Evict must leave the job non-resident")
	}

	// Subtest: Re-hydration preserves progress rather than re-zeroing counters.
	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("second Hydrate: %v", err)
	}
	if err := j.MarkArticleDone(0, 100, "srv"); err != nil {
		t.Fatalf("MarkArticleDone: %v", err)
	}
	if !j.Progress().ArticleDone(0) {
		t.Fatal("precondition: article 0 must be marked done")
	}

	r.Evict("abc123")
	if j.Resident() {
		t.Fatal("Evict must leave the job non-resident")
	}

	if err := r.Hydrate(context.Background(), "abc123"); err != nil {
		t.Fatalf("re-Hydrate: %v", err)
	}
	if !j.Resident() {
		t.Fatal("re-Hydrate must leave the job resident")
	}
	if !j.Progress().ArticleDone(0) {
		t.Fatal("re-hydration re-zeroed progress counters instead of preserving them via RestoreContent")
	}
}

// TestAppResidency_HydrateUnknownJobErrors pins that a missing job is an error
// rather than a silent no-op: the dispatcher logs Residency failures
// (logResidencyError, dispatch.go:171) and a silent success would strand a job
// at Fetching with nothing to fetch from.
func TestAppResidency_HydrateUnknownJobErrors(t *testing.T) {
	r := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, t.TempDir(), nil, nil)
	if err := r.Hydrate(context.Background(), "nope"); err == nil {
		t.Fatal("Hydrate of an unknown job must error")
	}
}

func TestAppResidency_RestoreResolution(t *testing.T) {
	rNilDB := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, t.TempDir(), nil, nil)
	jUnattached := job.New("j_nil", "name", job.Policy{})
	rNilDB.restoreResolution(context.Background(), jUnattached)

	dir := t.TempDir()
	hdb, err := history.Open(t.Context(), filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	defer func() { _ = hdb.Close() }()

	repo := history.NewRepository(hdb)
	db := repo.DB()

	j := job.New("j1", "name", job.PolicyFromPP(3))
	writeTestManifest(t, filepath.Join(dir, "j1.json.gz"), j)

	r := newAppResidency(func(id string) (*job.Job, bool) {
		if id == "j1" {
			return j, true
		}
		return nil, false
	}, dir, db, nil)

	if err := r.Hydrate(context.Background(), "j1"); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	_, err = db.Exec(`INSERT INTO durable_runs (job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"j1", 0, 0, 0, 0, 100, 12345)
	if err != nil {
		t.Fatalf("insert durable_runs: %v", err)
	}
	_, err = db.Exec(`INSERT INTO failed_articles (job_id, art_idx) VALUES (?, ?)`, "j1", 1)
	if err != nil {
		t.Fatalf("insert failed_articles: %v", err)
	}

	// First call: failed art_idx 1 causes ApplyResolution error
	r.restoreResolution(context.Background(), j)

	// Second call: art_idx 0 is valid, ApplyResolution succeeds with nil error
	_, _ = db.Exec(`DELETE FROM failed_articles WHERE job_id = ?`, "j1")
	_, _ = db.Exec(`INSERT INTO failed_articles (job_id, art_idx) VALUES (?, ?)`, "j1", 0)
	r.restoreResolution(context.Background(), j)

	// Third call: failed_articles table dropped causes QueryContext error
	_, _ = db.Exec(`DROP TABLE failed_articles`)
	r.restoreResolution(context.Background(), j)

	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel()
	r.restoreResolution(ctxCancel, j)

	r.Evict("missing")

	readyCh := make(chan struct{})
	r.mu.Lock()
	r.hydrating["waiting"] = readyCh
	r.mu.Unlock()
	go func() {
		close(readyCh)
	}()
	r.Evict("waiting")
}
