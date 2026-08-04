package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
)

// ReplaceManifest's three pre-transaction failures. Each one has to be
// reported rather than fall through to the transaction: committing rows for a
// manifest that never reached disk is the mismatch ErrManifestStale exists to
// describe, manufactured deliberately instead of by a crash.

// A non-resident job has no manifest to write. Asking it for one must fail
// here rather than silently write nothing — Store.ReplaceManifest's contract
// says the manifest being persisted is the one the job holds.
func TestReplaceManifest_RejectsANonResidentJob(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	filler := makeMultiFileJob(t, "replace-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJob(t, "replace-nonresident", 1, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: the job is resident, so Manifest() will not fail")
	}

	err := store.ReplaceManifest(t.Context(), job)
	if err == nil {
		t.Fatal("ReplaceManifest accepted a non-resident job")
	}
	if !strings.Contains(err.Error(), "replace manifest") {
		t.Errorf("error %q does not identify the operation", err)
	}
}

// replaceManifestJob returns a store whose directory is dir, plus a resident
// single-file job that has never been persisted — so nothing in the fixture
// depends on the manifests directory already existing.
func replaceManifestJob(t *testing.T, dir string) (*SQLiteStore, *Job) {
	t.Helper()
	// The store's own database lives beside the blocked path rather than in
	// it, so only the manifest write is affected.
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)

	job, err := NewJob(par2NZB(), AddOptions{Filename: "replace.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if _, mErr := job.Manifest(); mErr != nil {
		t.Fatalf("fixture guard: the job must be resident, got %v", mErr)
	}
	return store, job
}

// A regular file where the manifests directory belongs makes MkdirAll fail.
// The transaction must not run: rows describing a manifest that could not be
// written are worse than no write at all.
func TestReplaceManifest_ReportsAnUncreatableManifestsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifests"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store, job := replaceManifestJob(t, dir)

	err := store.ReplaceManifest(t.Context(), job)
	if err == nil {
		t.Fatal("ReplaceManifest succeeded with a file where the manifests directory belongs")
	}
	if !strings.Contains(err.Error(), "mkdir manifests") {
		t.Errorf("error %q does not name the failed mkdir", err)
	}
}

// A directory occupying the manifest's own path makes the write fail after
// MkdirAll has succeeded — the same must hold one step later.
func TestReplaceManifest_ReportsAnUnwritableManifest(t *testing.T) {
	dir := t.TempDir()
	store, job := replaceManifestJob(t, dir)
	blocked := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if err := os.MkdirAll(blocked, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	err := store.ReplaceManifest(t.Context(), job)
	if err == nil {
		t.Fatal("ReplaceManifest succeeded with a directory occupying the manifest's path")
	}
	if !strings.Contains(err.Error(), "write manifest") {
		t.Errorf("error %q does not name the failed write", err)
	}
}
