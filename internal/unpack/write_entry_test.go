package unpack

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWriteEntrySafely_Normal verifies the ordinary path: a regular entry is
// written through root, and its content lands on disk unmodified.
func TestWriteEntrySafely_Normal(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	src := strings.NewReader("hello world")
	wrote, err := writeEntrySafely(
		t.Context(), root, "file.txt", filepath.Join(outDir, "file.txt"),
		src, nil, true, 0o644, time.Time{}, Options{}, "file.txt", "go_test", slog.Default(), nil,
	)
	if err != nil {
		t.Fatalf("writeEntrySafely: %v", err)
	}
	if !wrote {
		t.Fatal("expected wrote=true for a fresh entry")
	}

	got, err := os.ReadFile(filepath.Join(outDir, "file.txt")) //nolint:gosec // test reads its own fixture
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("content mismatch: got %q", got)
	}
}

// TestWriteEntrySafely_SkipExisting verifies that an existing file is left
// untouched when opts.OverwriteFiles is false, and that drainOnSkip controls
// whether the source reader is drained.
func TestWriteEntrySafely_SkipExisting(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	if err := os.WriteFile(filepath.Join(outDir, "exists.txt"), []byte("original"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	src := strings.NewReader("new content")
	wrote, err := writeEntrySafely(
		t.Context(), root, "exists.txt", filepath.Join(outDir, "exists.txt"),
		src, nil, true, 0o644, time.Time{}, Options{OverwriteFiles: false}, "exists.txt", "go_test", slog.Default(), nil,
	)
	if err != nil {
		t.Fatalf("writeEntrySafely: %v", err)
	}
	if wrote {
		t.Fatal("expected wrote=false when skipping an existing file")
	}

	got, err := os.ReadFile(filepath.Join(outDir, "exists.txt")) //nolint:gosec // test reads its own fixture
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("existing file must be left untouched, got %q", got)
	}

	// drainOnSkip=true must have consumed src entirely.
	if n, _ := src.Read(make([]byte, 1)); n != 0 {
		t.Fatal("expected src to be drained on skip when drainOnSkip=true")
	}
}

// TestWriteEntrySafely_SkipExisting_NoDrain verifies that drainOnSkip=false
// (go_sevenzip's behavior) leaves the source reader unconsumed on a skip.
func TestWriteEntrySafely_SkipExisting_NoDrain(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	if err := os.WriteFile(filepath.Join(outDir, "exists.txt"), []byte("original"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	src := strings.NewReader("new content")
	wrote, err := writeEntrySafely(
		t.Context(), root, "exists.txt", filepath.Join(outDir, "exists.txt"),
		src, nil, false, 0o644, time.Time{}, Options{OverwriteFiles: false}, "exists.txt", "go_test", slog.Default(), nil,
	)
	if err != nil {
		t.Fatalf("writeEntrySafely: %v", err)
	}
	if wrote {
		t.Fatal("expected wrote=false when skipping an existing file")
	}

	buf := make([]byte, 1)
	n, _ := src.Read(buf)
	if n == 0 {
		t.Fatal("expected src to remain undrained when drainOnSkip=false")
	}
}

// errReader is an io.Reader that always fails, simulating a bomb-detection
// error (or any other read failure) from a src reader being drained on skip.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// TestWriteEntrySafely_SkipExisting_DrainErrorPropagates is a regression
// guard: a read error while draining a skipped entry (e.g. a bomb-detection
// error from a *boundReader-wrapped src) must be returned, not silently
// discarded — src may still enforce safety limits during the drain even
// though this entry's bytes aren't kept.
func TestWriteEntrySafely_SkipExisting_DrainErrorPropagates(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	if err := os.WriteFile(filepath.Join(outDir, "exists.txt"), []byte("original"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	wantErr := errors.New("simulated bomb-detection error during drain")
	src := errReader{err: wantErr}

	_, err = writeEntrySafely(
		t.Context(), root, "exists.txt", filepath.Join(outDir, "exists.txt"),
		src, nil, true, 0o644, time.Time{}, Options{OverwriteFiles: false}, "exists.txt", "go_test", slog.Default(), nil,
	)
	if err == nil {
		t.Fatal("writeEntrySafely: expected an error from the failing drain, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("writeEntrySafely error = %v, want wrapping %v", err, wantErr)
	}
}

// TestWriteEntrySafely_PathEscape verifies that os.Root refuses to write
// through a symlinked path component that resolves outside outDir, mirroring
// the existing ExtractEntryRarengine zip-slip regression tests (see
// TestExtractEntryRarengine_ZipSlip_SymlinkComponent in go_unrar_test.go).
func TestWriteEntrySafely_PathEscape(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "evil.txt")

	if err := os.Symlink(outside, filepath.Join(outDir, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	destRel := "link/evil.txt"
	destPath := filepath.Join(outDir, destRel) // lexically inside, resolves outside

	_, err = writeEntrySafely(
		t.Context(), root, destRel, destPath,
		strings.NewReader("EVIL"), nil, true, 0o644, time.Time{}, Options{}, "evil.txt", "go_test", slog.Default(), nil,
	)
	if err == nil {
		t.Fatal("expected error: os.Root must refuse write through symlinked component")
	}

	if _, statErr := os.Stat(victim); !os.IsNotExist(statErr) {
		t.Fatal("SECURITY: file was created outside output dir via symlinked component")
	}
}

// TestWriteEntrySafely_ExtraWriter verifies that extraWriter (go_sevenzip's
// CRC32 hasher use case) receives every byte written, alongside the file.
func TestWriteEntrySafely_ExtraWriter(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	var mirror strings.Builder
	src := strings.NewReader("mirrored content")
	wrote, err := writeEntrySafely(
		t.Context(), root, "mirror.txt", filepath.Join(outDir, "mirror.txt"),
		src, &mirror, true, 0o644, time.Time{}, Options{}, "mirror.txt", "go_test", slog.Default(), nil,
	)
	if err != nil {
		t.Fatalf("writeEntrySafely: %v", err)
	}
	if !wrote {
		t.Fatal("expected wrote=true")
	}
	if mirror.String() != "mirrored content" {
		t.Fatalf("extraWriter mismatch: got %q", mirror.String())
	}
}

// TestWriteEntrySafely_ModeZeroSkipsChmod verifies mode=0 is the documented
// "skip chmod" sentinel (used by go_unrar when fh.HostOS == 0), by confirming
// no error occurs and the file is still written with the OpenFile default.
func TestWriteEntrySafely_ModeZeroSkipsChmod(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	wrote, err := writeEntrySafely(
		t.Context(), root, "nomode.txt", filepath.Join(outDir, "nomode.txt"),
		strings.NewReader("x"), nil, true, 0, time.Time{}, Options{}, "nomode.txt", "go_test", slog.Default(), nil,
	)
	if err != nil {
		t.Fatalf("writeEntrySafely: %v", err)
	}
	if !wrote {
		t.Fatal("expected wrote=true")
	}

	fi, err := os.Stat(filepath.Join(outDir, "nomode.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// OpenFile's own default (0o600), not the passed mode=0 or any
	// world-writable leftover -- proves Chmod was skipped rather than
	// zeroing the file's permissions.
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected default 0o600 perms when mode=0 skips chmod, got %v", fi.Mode().Perm())
	}
}

// TestWriteEntrySafely_ModTimeRestored verifies a non-zero modTime is
// restored via root.Chtimes.
func TestWriteEntrySafely_ModTimeRestored(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	want := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	wrote, err := writeEntrySafely(
		t.Context(), root, "timed.txt", filepath.Join(outDir, "timed.txt"),
		strings.NewReader("x"), nil, true, 0o644, want, Options{}, "timed.txt", "go_test", slog.Default(), nil,
	)
	if err != nil {
		t.Fatalf("writeEntrySafely: %v", err)
	}
	if !wrote {
		t.Fatal("expected wrote=true")
	}

	fi, err := os.Stat(filepath.Join(outDir, "timed.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !fi.ModTime().Equal(want) {
		t.Fatalf("expected mtime %v, got %v", want, fi.ModTime())
	}
}

// TestWriteEntrySafely_IgnoreUnrarDates verifies opts.IgnoreUnrarDates=true
// skips timestamp restoration even when modTime is non-zero.
func TestWriteEntrySafely_IgnoreUnrarDates(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	before := time.Now()
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	wrote, err := writeEntrySafely(
		t.Context(), root, "ignoredate.txt", filepath.Join(outDir, "ignoredate.txt"),
		strings.NewReader("x"), nil, true, 0o644, future, Options{IgnoreUnrarDates: true}, "ignoredate.txt", "go_test", slog.Default(), nil,
	)
	if err != nil {
		t.Fatalf("writeEntrySafely: %v", err)
	}
	if !wrote {
		t.Fatal("expected wrote=true")
	}

	fi, err := os.Stat(filepath.Join(outDir, "ignoredate.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.ModTime().Equal(future) || fi.ModTime().Before(before.Add(-time.Minute)) {
		t.Fatalf("expected mtime near now (restoration skipped), got %v", fi.ModTime())
	}
}

// --- context cancellation ---

func TestWriteEntrySafely_ContextCanceled(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = writeEntrySafely(
		ctx, root, "cancelled.txt", filepath.Join(outDir, "cancelled.txt"),
		strings.NewReader("x"), nil, true, 0o644, time.Time{}, Options{}, "cancelled.txt", "go_test", slog.Default(), nil,
	)
	if err == nil {
		t.Fatal("expected an error from a canceled context")
	}
}

// partialThenErrReader returns some content successfully on its first Read,
// then fails on every subsequent Read, simulating a write that fails
// partway through rather than before any bytes are read.
type partialThenErrReader struct {
	data []byte
	err  error
	sent bool
}

func (r *partialThenErrReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		n := copy(p, r.data)
		return n, nil
	}
	return 0, r.err
}

// TestWriteEntrySafely_WriteFailureLeavesNoPartialFile is the regression
// guard for issue #81: a write that fails partway through must never leave
// a partial file visible at destPath. Before the atomic temp+rename publish
// rework, content was written directly to destPath, so a mid-write failure
// left whatever had been written so far sitting at the final name.
func TestWriteEntrySafely_WriteFailureLeavesNoPartialFile(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	wantErr := errors.New("simulated write failure")
	src := &partialThenErrReader{data: []byte("partial content"), err: wantErr}

	_, err = writeEntrySafely(
		t.Context(), root, "victim.txt", filepath.Join(outDir, "victim.txt"),
		src, nil, true, 0o644, time.Time{}, Options{}, "victim.txt", "go_test", slog.Default(), nil,
	)
	if err == nil {
		t.Fatal("writeEntrySafely: expected an error from the failing reader")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("writeEntrySafely error = %v, want wrapping %v", err, wantErr)
	}

	if _, statErr := os.Stat(filepath.Join(outDir, "victim.txt")); !os.IsNotExist(statErr) {
		t.Errorf("victim.txt exists after write failure; want no file published (stat err: %v)", statErr)
	}

	// No orphaned temp file should remain either.
	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("outDir not empty after write failure: %v", entries)
	}
}

// TestWriteEntrySafely_VerifyFailureLeavesNoPublishedFile is the regression
// guard for issue #82: a verify callback failure (e.g. go_sevenzip's CRC32
// check) must gate publishing, so the entry never becomes visible at
// destPath under a name that looks like a successfully extracted file.
func TestWriteEntrySafely_VerifyFailureLeavesNoPublishedFile(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	wantErr := errors.New("simulated checksum mismatch")
	verify := func() error { return wantErr }

	_, err = writeEntrySafely(
		t.Context(), root, "corrupt.txt", filepath.Join(outDir, "corrupt.txt"),
		strings.NewReader("looks fine but isn't"), nil, true, 0o644, time.Time{}, Options{}, "corrupt.txt", "go_test", slog.Default(), verify,
	)
	if !errors.Is(err, wantErr) {
		t.Errorf("writeEntrySafely error = %v, want wrapping %v", err, wantErr)
	}

	if _, statErr := os.Stat(filepath.Join(outDir, "corrupt.txt")); !os.IsNotExist(statErr) {
		t.Errorf("corrupt.txt exists after verify failure; want no file published (stat err: %v)", statErr)
	}

	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("outDir not empty after verify failure: %v", entries)
	}
}

// TestWriteEntrySafely_NoTempFileLeftOnSuccess verifies that a successful
// write leaves exactly the published file behind — no leftover
// ".gonzbd-tmp-*" sibling from the atomic-publish temp file.
func TestWriteEntrySafely_NoTempFileLeftOnSuccess(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup, best-effort

	wrote, err := writeEntrySafely(
		t.Context(), root, "file.txt", filepath.Join(outDir, "file.txt"),
		strings.NewReader("hello world"), nil, true, 0o644, time.Time{}, Options{}, "file.txt", "go_test", slog.Default(), nil,
	)
	if err != nil {
		t.Fatalf("writeEntrySafely: %v", err)
	}
	if !wrote {
		t.Fatal("expected wrote=true")
	}

	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "file.txt" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("outDir entries = %v, want exactly [\"file.txt\"]", names)
	}
}
