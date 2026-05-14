package app

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteGzFile_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.gz")

	original := []byte("hello, compressed world!")
	if err := writeGzFile(path, original); err != nil {
		t.Fatalf("writeGzFile: %v", err)
	}

	// Read back and decompress.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(gr); err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if err := gr.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), original) {
		t.Errorf("decompressed = %q, want %q", buf.String(), string(original))
	}
}

func TestWriteGzFile_EmptyData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.gz")

	if err := writeGzFile(path, nil); err != nil {
		t.Fatalf("writeGzFile(nil): %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(gr); err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	_ = gr.Close()
	if buf.Len() != 0 {
		t.Errorf("expected empty, got %d bytes", buf.Len())
	}
}

func TestWriteGzFile_Atomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.gz")

	// Write initial content.
	if err := writeGzFile(path, []byte("first")); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Write again — should atomically replace.
	if err := writeGzFile(path, []byte("second")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	// Verify content is "second".
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(gr); err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	_ = gr.Close()
	if buf.String() != "second" {
		t.Errorf("got %q, want %q", buf.String(), "second")
	}
}

func TestWriteGzFile_BadPath(t *testing.T) {
	t.Parallel()
	// Write to a non-existent directory should fail.
	err := writeGzFile("/nonexistent_dir_xyz/test.gz", []byte("data"))
	if err == nil {
		t.Error("expected error for bad path, got nil")
	}
}
