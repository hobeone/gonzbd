package fsutil_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

func TestWriteAtomic_Success(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "output.json")
	want := []byte(`{"ok":true}`)

	err := fsutil.WriteAtomic(path, func(w io.Writer) error {
		_, err := w.Write(want)
		return err
	})
	if err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("content = %q, want %q", got, want)
	}
}

func TestWriteAtomic_WriteErrorLeavesTargetUntouched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "output.json")
	original := []byte("original")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	writeErr := errors.New("simulated encode failure")
	err := fsutil.WriteAtomic(path, func(w io.Writer) error {
		return writeErr
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("error chain missing writeErr: %v", err)
	}

	// Target must be untouched.
	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Errorf("target modified on write error: got %q, want %q", got, original)
	}

	// Temp file must be cleaned up.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "output.json" {
			t.Errorf("stray temp file left: %s", e.Name())
		}
	}
}

func TestWriteAtomic_TempFileCleanedOnWriteError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	_ = fsutil.WriteAtomic(path, func(w io.Writer) error {
		return errors.New("fail")
	})

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "out.bin" {
			t.Errorf("temp file not removed: %s", e.Name())
		}
	}
	// Target was never created.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("target should not exist after failed write")
	}
}

func TestWriteAtomic_Overwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")

	if err := fsutil.WriteAtomicBytes(path, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteAtomicBytes(path, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "v2" {
		t.Errorf("got %q, want v2", got)
	}
}

func TestWriteAtomicBytes_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bytes.bin")
	want := []byte("hello atomic world")

	if err := fsutil.WriteAtomicBytes(path, want); err != nil {
		t.Fatalf("WriteAtomicBytes: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriteAtomic_MissingDirFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nosuchdir", "out.bin")
	err := fsutil.WriteAtomicBytes(path, []byte("x"))
	if err == nil {
		t.Error("expected error for missing parent directory, got nil")
	}
}

func TestWriteAtomic_SyncError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sync_err.bin")

	err := fsutil.WriteAtomic(path, func(w io.Writer) error {
		f, ok := w.(*os.File)
		if !ok {
			t.Fatal("expected *os.File")
		}
		// Closing the file early causes tmp.Sync() to fail with EBADF/file already closed.
		_ = f.Close()
		return nil
	})
	if err == nil {
		t.Fatal("expected sync error when file is closed early, got nil")
	}
}

func TestWriteAtomic_RenameError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rename_err.bin")

	err := fsutil.WriteAtomic(path, func(w io.Writer) error {
		f, ok := w.(*os.File)
		if !ok {
			t.Fatal("expected *os.File")
		}
		_, _ = f.Write([]byte("data"))
		// Removing the temp file during write causes os.Rename to fail.
		_ = os.Remove(f.Name())
		return nil
	})
	if err == nil {
		t.Fatal("expected rename error when temp file is removed early, got nil")
	}
}
