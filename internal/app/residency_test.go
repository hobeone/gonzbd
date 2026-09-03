package app

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

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
	}, dir, nil)

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

	// The Restore-vs-Attach branch is invisible on a fresh job: both branches
	// leave a freshly-hydrated job with zeroed progress, so the assertions
	// above pass identically whichever branch runs. Mark an article done,
	// evict, and re-hydrate to force the branch that actually distinguishes
	// them — RestoreContent must reuse the surviving *JobProgress rather
	// than AttachContent installing a fresh, zeroed one.
	t.Run("survives re-hydration after an article is marked done", func(t *testing.T) {
		if err := r.Hydrate(context.Background(), "abc123"); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if err := j.MarkArticleDone(0); err != nil {
			t.Fatalf("MarkArticleDone: %v", err)
		}
		if !j.Progress().ArticleDone(0) {
			t.Fatal("ArticleDone(0) before evict = false, want true")
		}

		r.Evict("abc123")
		if j.Resident() {
			t.Fatal("Evict must leave the job non-resident")
		}
		// Progress is always-resident (docs/queue-lifecycle.md) and must
		// survive eviction on its own — this is the precondition that makes
		// the re-hydrate below a real test of RestoreContent rather than of
		// AttachContent operating on a progress record Evict already wiped.
		if !j.Progress().ArticleDone(0) {
			t.Fatal("ArticleDone(0) after evict = false, want true (progress must survive eviction)")
		}

		if err := r.Hydrate(context.Background(), "abc123"); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !j.Resident() {
			t.Fatal("Hydrate must leave the job resident")
		}
		if !j.Progress().ArticleDone(0) {
			t.Fatal("ArticleDone(0) after re-hydrate = false, want true (RestoreContent must not zero it)")
		}
	})
}

// TestAppResidency_HydrateUnknownJobErrors pins that a missing job is an error
// rather than a silent no-op: the dispatcher logs Residency failures
// (logResidencyError, dispatch.go:171) and a silent success would strand a job
// at Fetching with nothing to fetch from.
func TestAppResidency_HydrateUnknownJobErrors(t *testing.T) {
	r := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, t.TempDir(), nil)
	if err := r.Hydrate(context.Background(), "nope"); err == nil {
		t.Fatal("Hydrate of an unknown job must error")
	}
}

// The on-disk manifest shapes below mirror internal/job's unexported
// manifestJSON/manifestJSONFile/manifestJSONArticle wire format exactly
// (field names, json tags, and nesting) so that job.Manifest's exported
// json.Unmarshaler implementation can decode them. They are a fixture-local
// copy, not a shared type, because internal/job exports no constructor for a
// Manifest from a file list other than round-tripping through JSON.
type testManifestArticle struct {
	ID     string `json:"id"`
	Bytes  int    `json:"bytes"`
	Number int    `json:"number"`
}

type testManifestFile struct {
	Subject        string                `json:"subject"`
	Date           time.Time             `json:"date"`
	Bytes          int64                 `json:"bytes"`
	IsPar2Recovery bool                  `json:"is_par2_recovery,omitempty"`
	Articles       []testManifestArticle `json:"articles"`
}

type testManifest struct {
	Files      []testManifestFile `json:"files"`
	TotalBytes int64              `json:"total_bytes"`
}

// writeTestManifest writes a minimal one-file, one-article gzip-JSON
// manifest for job j to path, in the on-disk shape appResidency.readManifest
// expects to find under <dir>/<id>.json.gz.
func writeTestManifest(t *testing.T, path string, j *job.Job) {
	t.Helper()

	mj := testManifest{
		Files: []testManifestFile{
			{
				Subject: "test.rar",
				Date:    time.Now(),
				Bytes:   100,
				Articles: []testManifestArticle{
					{ID: "1@" + j.ID() + ".test", Bytes: 100, Number: 1},
				},
			},
		},
		TotalBytes: 100,
	}

	data, err := json.Marshal(mj)
	if err != nil {
		t.Fatalf("marshal test manifest: %v", err)
	}

	f, err := os.Create(path) //nolint:gosec // t.TempDir()-rooted test fixture path
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	zw := gzip.NewWriter(f)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("write gzip manifest: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
}
