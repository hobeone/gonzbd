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

// TestDecodeArticles_DecodesGzippedJSON pins decodeArticles's happy path:
// gzip-decompress, then JSON-decode the article list, closing the file it
// was handed either way.
//
// Pre-existing helper surfaced by check_test_alignment while this package was
// touched for #329 — not part of that change, but a real gap once flagged.
func TestDecodeArticles_DecodesGzippedJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json.gz")
	content := `{"articles":[{"id":"m1","bytes":100,"number":1}]}`
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
	arts, err := decodeArticles(f)
	if err != nil {
		t.Fatalf("decodeArticles: %v", err)
	}
	if len(arts) != 1 || arts[0].ID != "m1" {
		t.Errorf("decodeArticles = %v, want the one article m1", arts)
	}

	// decodeArticles closes f itself (its doc comment says so); a second
	// Close must report the file already closed rather than succeed, which
	// is how this pins that it actually happened rather than merely reading
	// the content successfully despite a leaked handle.
	if err := f.Close(); err == nil {
		t.Error("f.Close() after decodeArticles returned nil, want already-closed — decodeArticles did not close its file")
	}
}

// TestDecodeArticles_RejectsNonGzip pins the error path: a file that is not
// gzip-compressed is rejected at the gzip.NewReader step rather than
// producing a zero-value manifest.
func TestDecodeArticles_RejectsNonGzip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json.gz")
	if err := os.WriteFile(path, []byte("not gzip"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := decodeArticles(f); err == nil {
		t.Error("decodeArticles of a non-gzip file returned nil error, want one")
	}
}

// TestReadArticles_ReadsFromDir pins readArticles's composition of
// openManifestIn and decodeArticles: given a job ID, it locates and decodes
// that job's manifest under the residency's manifest directory.
func TestReadArticles_ReadsFromDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := job.New("abc123", "test", job.Policy{})
	writeTestManifest(t, filepath.Join(dir, "abc123.json.gz"), j)

	r := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, dir, nil, nil)
	arts, err := r.readArticles(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("readArticles: %v", err)
	}
	if len(arts) != 1 {
		t.Errorf("len(articles) = %d, want 1", len(arts))
	}
}

// TestReadArticles_MissingJobErrors pins that a job ID with no manifest file
// on disk reports an error rather than an empty list with a nil error.
func TestReadArticles_MissingJobErrors(t *testing.T) {
	t.Parallel()
	r := newAppResidency(func(string) (*job.Job, bool) { return nil, false }, t.TempDir(), nil, nil)
	if _, err := r.readArticles(context.Background(), "nope"); err == nil {
		t.Error("readArticles for a missing job returned nil error, want one")
	}
}
