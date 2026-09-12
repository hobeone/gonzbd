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

// TestDecodeManifest_DecodesGzippedJSON pins decodeManifest's happy path:
// gzip-decompress, then JSON-decode into a job.Manifest, closing the file it
// was handed either way.
//
// Pre-existing helper surfaced by check_test_alignment while this package was
// touched for #329 — not part of that change, but a real gap once flagged.
func TestDecodeManifest_DecodesGzippedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json.gz")
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

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m, err := decodeManifest(f)
	if err != nil {
		t.Fatalf("decodeManifest: %v", err)
	}
	if m.NumFiles() != 1 {
		t.Errorf("NumFiles() = %d, want 1", m.NumFiles())
	}

	// decodeManifest closes f itself (its doc comment says so); a second
	// Close must report the file already closed rather than succeed, which
	// is how this pins that it actually happened rather than merely reading
	// the content successfully despite a leaked handle.
	if err := f.Close(); err == nil {
		t.Error("f.Close() after decodeManifest returned nil, want already-closed — decodeManifest did not close its file")
	}
}

// TestDecodeManifest_RejectsNonGzip pins the error path: a file that is not
// gzip-compressed is rejected at the gzip.NewReader step rather than
// producing a zero-value manifest.
func TestDecodeManifest_RejectsNonGzip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json.gz")
	if err := os.WriteFile(path, []byte("not gzip"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := decodeManifest(f); err == nil {
		t.Error("decodeManifest of a non-gzip file returned nil error, want one")
	}
}

// TestReadManifest_ReadsFromDir pins readManifest's composition of
// openManifestIn and decodeManifest: given a job ID, it locates and decodes
// that job's manifest under the residency's manifest directory.
func TestReadManifest_ReadsFromDir(t *testing.T) {
	dir := t.TempDir()
	j := job.New("abc123", "test", job.Policy{})
	writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j)

	r := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, dir, nil, nil)
	m, err := r.readManifest(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if m.NumFiles() != 1 {
		t.Errorf("NumFiles() = %d, want 1", m.NumFiles())
	}
}

// TestReadManifest_MissingJobErrors pins that a job ID with no manifest file
// on disk reports an error rather than a nil manifest with a nil error.
func TestReadManifest_MissingJobErrors(t *testing.T) {
	r := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, t.TempDir(), nil, nil)
	if _, err := r.readManifest(context.Background(), "nope"); err == nil {
		t.Error("readManifest for a missing job returned nil error, want one")
	}
}
