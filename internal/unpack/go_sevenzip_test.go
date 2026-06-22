package unpack

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/bodgit/sevenzip"
)

// sevenZipTestdata returns the path to the local sevenzip testdata
// directory, vendored into this repo so these tests are self-contained
// and don't depend on a sibling project being checked out.
func sevenZipTestdata(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("testdata", "sevenzip")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Fatalf("local sevenzip testdata not found at %s", dir)
	}
	return dir
}

func TestGoSevenZip_LZMA2(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "lzma2.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
	if len(res.Output) == 0 {
		t.Error("res.Output should be non-empty")
	}
	for _, f := range res.ExtractedFiles {
		wantLine := "Extracting  " + f
		if !strings.Contains(res.Output, wantLine) {
			t.Errorf("expected res.Output to contain %q, got: %q", wantLine, res.Output)
		}
	}
	t.Logf("Extracted %d files", len(res.ExtractedFiles))

	filePath := filepath.Join(outDir, res.ExtractedFiles[0])
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("expected file %s to have mode 0644, got %o", res.ExtractedFiles[0], info.Mode().Perm())
	}
}

func TestGoSevenZip_LZMA(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "lzma.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
}

func TestGoSevenZip_Copy(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "copy.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
}

// corruptedCopySevenZip copies copy.7z into dir, flips one byte within the
// stored (uncompressed) file content, and returns the corrupted archive's
// path. copy.7z uses the 7z "Copy" method, so the on-disk archive bytes
// contain the plaintext content verbatim — the corruption can be applied by
// locating that exact substring in the raw file and changing one byte
// in place, without needing to understand 7z's container format at all.
// The byte count is preserved, so the file's UncompressedSize/structure
// stays intact; only the content (and therefore its real CRC32) changes,
// while FileHeader.CRC32 (parsed from the header) still reflects the
// original, correct content — exactly modeling a RAR/7z volume assembled
// from a download with missing/failed articles (right size, wrong bytes).
func corruptedCopySevenZip(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(sevenZipTestdata(t), "copy.7z")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	const needle = "Lorem ipsum dolor sit amet"
	idx := strings.Index(string(data), needle)
	if idx < 0 {
		t.Fatalf("fixture content marker %q not found in copy.7z — fixture may have changed", needle)
	}
	corrupted := append([]byte(nil), data...)
	corrupted[idx] ^= 0xFF // flip every bit of one byte; same length, wrong content

	dst := filepath.Join(dir, "copy_corrupted.7z")
	if err := os.WriteFile(dst, corrupted, 0o600); err != nil {
		t.Fatalf("write corrupted fixture: %v", err)
	}
	return dst
}

// TestGoSevenZip_ChecksumMismatch_DetectsCorruption guards against the
// reported gap where bodgit/sevenzip parses each file's CRC32 from the
// archive header but never compares it against the actually-decompressed
// content (confirmed via the library's own README FAQ, which tells callers
// to do this themselves for the encrypted+uncompressed case). Without our
// own check, a 7z volume assembled from an incomplete/corrupt download
// (right size, wrong bytes) extracts "successfully" with no signal to the
// quickcheck/repair pipeline that anything is wrong.
func TestGoSevenZip_ChecksumMismatch_DetectsCorruption(t *testing.T) {
	outDir := t.TempDir()
	archivePath := corruptedCopySevenZip(t, t.TempDir())
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: archivePath,
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err == nil {
		t.Fatal("GoSevenZip() expected a checksum error on corrupted content, got nil")
	}
	if !strings.Contains(err.Error(), "checksum error") {
		t.Errorf("expected error to mention 'checksum error', got: %v", err)
	}
	if res.Reason != FailCorrupt {
		t.Errorf("Reason = %v, want FailCorrupt", res.Reason)
	}
}

func TestGoSevenZip_Bzip2(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "bzip2.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
}

func TestGoSevenZip_Deflate(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "deflate.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
}

func TestGoSevenZip_PasswordCorrect(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "aes7z.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{
		Password: "password",
	})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
}

func TestGoSevenZip_PasswordWrong(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "aes7z.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{
		Password: "wrong_password",
	})
	if err == nil {
		t.Fatal("GoSevenZip() expected error for wrong password")
	}
	if res.Reason != FailWrongPassword {
		t.Errorf("GoSevenZip() Reason = %v, want FailWrongPassword", res.Reason)
	}
}

func TestGoSevenZip_MultiVolume(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "multi.7z.001"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
	t.Logf("Extracted %d files from multi-volume", len(res.ExtractedFiles))
}

func TestGoSevenZip_Empty(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "empty.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	// Empty archives may contain zero files; just ensure no error.
	t.Logf("Extracted %d files from empty archive", len(res.ExtractedFiles))
}

func TestGoSevenZip_ContextCancellation(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "lzma2.7z"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := GoSevenZip(ctx, slog.Default(), archive, outDir, Options{})
	if err == nil {
		t.Fatal("GoSevenZip() expected error for cancelled context")
	}
}

func TestGoSevenZip_OnLineCallback(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "lzma2.7z"),
	}

	var lines []string
	_, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{
		OnLine: func(line string) {
			lines = append(lines, line)
		},
	})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("GoSevenZip() OnLine was never called")
	}
	for _, line := range lines {
		if len(line) < 12 || line[:12] != "Extracting  " {
			t.Errorf("unexpected OnLine content: %q", line)
		}
	}
}

func TestGoSevenZip_BCJ(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "bcj.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
}

func TestGoSevenZip_BCJ2(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "bcj2.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
}

func TestGoSevenZip_Zstd(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "zstd.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
}

func TestGoSevenZip_Brotli(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "brotli.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZip() extracted no files")
	}
}

func TestGoSevenZip_NotAnArchive(t *testing.T) {
	outDir := t.TempDir()

	// Create a file that's not a 7z archive.
	fakeFile := filepath.Join(outDir, "not_archive.7z")
	if err := os.WriteFile(fakeFile, []byte("this is not a 7z archive"), 0600); err != nil {
		t.Fatal(err)
	}

	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: fakeFile,
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err == nil {
		t.Fatal("GoSevenZip() expected error for non-archive file")
	}
	// Should classify as not-archive or corrupt.
	if res.Reason != FailNotArchive && res.Reason != FailCorrupt {
		t.Logf("GoSevenZip() Reason = %v (error: %v)", res.Reason, err)
	}
}

func TestGoSevenZip_CommandLineField(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "lzma2.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	if res.CommandLine == "" {
		t.Fatal("GoSevenZip() CommandLine is empty")
	}
	if res.CommandLine[:5] != "go_7z" {
		t.Errorf("GoSevenZip() CommandLine = %q, want prefix go_7z", res.CommandLine)
	}
}

func TestClassifySevenZipErrorDirect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want FailReason
	}{
		{
			// nil is never passed in production: callers only invoke this function
			// after confirming err != nil. Returning FailUnknown for nil is
			// arbitrary; this case documents the observed behavior so a future
			// refactor doesn't accidentally change it silently.
			name: "nil error",
			err:  nil,
			want: FailUnknown,
		},
		{
			name: "read error encrypted",
			err:  &sevenzip.ReadError{Encrypted: true},
			want: FailWrongPassword,
		},
		{
			name: "read error unencrypted",
			err:  &sevenzip.ReadError{Encrypted: false},
			want: FailUnknown,
		},
		{
			name: "not a valid 7-zip file",
			err:  errors.New("not a valid 7-zip file"),
			want: FailNotArchive,
		},
		{
			name: "checksum error",
			err:  errors.New("checksum error"),
			want: FailCorrupt,
		},
		{
			name: "unsupported compression algorithm",
			err:  errors.New("unsupported compression algorithm"),
			want: FailCorrupt,
		},
		{
			name: "disk full",
			err:  syscall.ENOSPC,
			want: FailDiskFull,
		},
		{
			name: "other error",
			err:  errors.New("something went wrong"),
			want: FailUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySevenZipError(tc.err)
			if got != tc.want {
				t.Errorf("classifySevenZipError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestGoSevenZip_PanicRecovery verifies the top-level recover() in
// GoSevenZip converts any panic into a FailCorrupt result with a
// "sevenzip panic" error. The panic here comes from log.With on the
// nil *slog.Logger (a convenient, deterministic trigger) rather than
// from the sevenzip library itself, but it exercises the same generic
// recover/format/Reason path that a real library panic would hit.
func TestGoSevenZip_PanicRecovery(t *testing.T) {
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: "dummy.7z",
	}
	res, err := GoSevenZip(context.Background(), nil, archive, t.TempDir(), Options{})
	if err == nil {
		t.Fatal("expected error from panic recovery, got nil")
	}
	if !strings.Contains(err.Error(), "sevenzip panic") {
		t.Errorf("expected error to contain 'sevenzip panic', got: %v", err)
	}
	if res.Reason != FailCorrupt {
		t.Errorf("expected res.Reason to be FailCorrupt, got: %v", res.Reason)
	}
}
