package cache

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------- sameSlice ----------

func TestSameSlice_BothNil(t *testing.T) {
	t.Parallel()
	if !sameSlice(nil, nil) {
		t.Error("both nil should be same")
	}
}

func TestSameSlice_OneNil(t *testing.T) {
	t.Parallel()
	if sameSlice(nil, []byte{1}) {
		t.Error("nil vs non-nil should be different")
	}
	if sameSlice([]byte{1}, nil) {
		t.Error("non-nil vs nil should be different")
	}
}

func TestSameSlice_SameBacking(t *testing.T) {
	t.Parallel()
	data := []byte("hello world")
	a := data[:5]
	b := data[:5]
	if !sameSlice(a, b) {
		t.Error("same backing array should be same")
	}
}

func TestSameSlice_DifferentBacking(t *testing.T) {
	t.Parallel()
	a := make([]byte, 5)
	copy(a, "hello")
	b := make([]byte, 5)
	copy(b, "hello")
	if sameSlice(a, b) {
		t.Error("different backing arrays should be different")
	}
}

// ---------- Limit ----------

func TestCache_Limit(t *testing.T) {
	t.Parallel()
	c := New(Options{Limit: 1024})
	if c.Limit() != 1024 {
		t.Errorf("Limit() = %d, want 1024", c.Limit())
	}
}

func TestCache_LimitZero(t *testing.T) {
	t.Parallel()
	c := New(Options{Limit: 0})
	if c.Limit() != 0 {
		t.Errorf("Limit() = %d, want 0", c.Limit())
	}
}

// ---------- Flush ----------

func TestCache_FlushEmpty(t *testing.T) {
	c := New(Options{Limit: 1024})
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush empty: %v", err)
	}
}

func TestCache_FlushToDisk(t *testing.T) {
	dir := t.TempDir()
	c := New(Options{Limit: 1024})

	c.Save("key1", dir, []byte("data1"))
	if c.Used() == 0 {
		t.Fatal("expected data in memory")
	}

	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if c.Used() != 0 {
		t.Errorf("Used = %d after Flush, want 0", c.Used())
	}

	// Data should be loadable from disk.
	data, err := c.Load("key1", dir)
	if err != nil {
		t.Fatalf("Load after Flush: %v", err)
	}
	if string(data) != "data1" {
		t.Errorf("data = %q, want data1", data)
	}
}

// ---------- Purge ----------

func TestCache_PurgeMemory(t *testing.T) {
	dir := t.TempDir()
	c := New(Options{Limit: 1024})

	c.Save("key1", dir, []byte("data"))
	c.Purge([]string{"key1"}, dir)

	if c.Used() != 0 {
		t.Errorf("Used = %d after Purge, want 0", c.Used())
	}

	_, err := c.Load("key1", dir)
	if err != ErrNotFound {
		t.Errorf("Load after Purge: want ErrNotFound, got %v", err)
	}
}

func TestCache_PurgeDisk(t *testing.T) {
	dir := t.TempDir()
	c := New(Options{Limit: 0}) // no cache = all to disk

	c.Save("key1", dir, []byte("data"))

	// Verify file exists on disk.
	path := diskPath(dir, "key1")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("disk file should exist")
	}

	c.Purge([]string{"key1"}, dir)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("disk file should be removed after Purge")
	}
}

// ---------- writeTempFile ----------

func TestWriteTempFile_Success(t *testing.T) {
	dir := t.TempDir()
	c := New(Options{Limit: 1024})

	tmpName, err := c.writeTempFile("key", dir, []byte("testdata"))
	if err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}

	data, err := os.ReadFile(tmpName)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "testdata" {
		t.Errorf("data = %q", data)
	}

	// Cleanup.
	os.Remove(tmpName)
}

func TestWriteTempFile_BadDir(t *testing.T) {
	c := New(Options{Limit: 1024})
	_, err := c.writeTempFile("key", "/nonexistent/dir/cache", []byte("data"))
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

// ---------- writeToDisk ----------

func TestWriteToDisk_Success(t *testing.T) {
	dir := t.TempDir()
	c := New(Options{Limit: 1024})

	if err := c.writeToDisk("key", dir, []byte("diskdata")); err != nil {
		t.Fatalf("writeToDisk: %v", err)
	}

	path := diskPath(dir, "key")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "diskdata" {
		t.Errorf("data = %q", data)
	}
}

// ---------- diskPath ----------

func TestDiskPath_Deterministic(t *testing.T) {
	t.Parallel()
	p1 := diskPath("/tmp/admin", "msg-123")
	p2 := diskPath("/tmp/admin", "msg-123")
	if p1 != p2 {
		t.Error("diskPath should be deterministic")
	}

	// Different keys = different paths.
	p3 := diskPath("/tmp/admin", "msg-456")
	if p1 == p3 {
		t.Error("different keys should produce different paths")
	}
}

func TestDiskPath_FixedLength(t *testing.T) {
	t.Parallel()
	p := diskPath("/tmp", "key")
	base := filepath.Base(p)
	if len(base) != 64 { // sha256 hex = 64 chars
		t.Errorf("filename length = %d, want 64 (sha256 hex)", len(base))
	}
}
