package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

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
	r := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, t.TempDir(), nil)
	if err := r.Hydrate(context.Background(), "nope"); err == nil {
		t.Fatal("Hydrate of an unknown job must error")
	}
}
