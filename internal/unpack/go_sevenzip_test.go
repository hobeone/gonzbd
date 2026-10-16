package unpack

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "password", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "wrong_password", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
	if err != nil {
		t.Fatalf("GoSevenZip() error: %v", err)
	}
	// empty.7z contains 5 empty directories and 5 zero-byte regular files;
	// directories don't count toward ExtractedFiles.
	if len(res.ExtractedFiles) != 5 {
		t.Errorf("GoSevenZip() extracted %d files, want 5 (empty.7z has 5 zero-byte files + 5 empty dirs)", len(res.ExtractedFiles))
	}
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

	_, err := GoSevenZip(ctx, slog.Default(), archive, outDir, "", Options{})
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
	_, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
	if err == nil {
		t.Fatal("GoSevenZip() expected error for non-archive file")
	}
	if res.Reason != FailNotArchive {
		t.Errorf("GoSevenZip() Reason = %v, want %v", res.Reason, FailNotArchive)
	}
}

func TestGoSevenZip_CommandLineField(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "lzma2.7z"),
	}

	res, err := GoSevenZip(context.Background(), slog.Default(), archive, outDir, "", Options{})
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
// "sevenzip panic" error. The panic here comes from an OnLine that panics
// on its first call, the first entry's "Extracting" line (a convenient,
// deterministic trigger) rather than from the sevenzip library itself, but
// it exercises the same generic recover/format/Reason path that a real
// library panic would hit.
func TestGoSevenZip_PanicRecovery(t *testing.T) {
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(sevenZipTestdata(t), "lzma2.7z"),
	}
	var onlineMsg string
	calls := 0
	res, err := GoSevenZip(context.Background(), testLogger(), archive, t.TempDir(), "", Options{
		OnLine: func(msg string) {
			calls++
			if calls == 1 {
				panic("first OnLine call")
			}
			onlineMsg = msg
		},
	})
	if err == nil {
		t.Fatal("expected error from panic recovery, got nil")
	}
	if !strings.Contains(err.Error(), "sevenzip panic") {
		t.Errorf("expected error to contain 'sevenzip panic', got: %v", err)
	}
	if res.Reason != FailCorrupt {
		t.Errorf("expected res.Reason to be FailCorrupt, got: %v", res.Reason)
	}
	if !strings.Contains(onlineMsg, "ERROR: sevenzip panic:") {
		t.Errorf("expected OnLine callback to receive 'ERROR: sevenzip panic:', got: %q", onlineMsg)
	}
}

func TestGoSevenZipInternal_Direct(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "lzma2.7z"),
	}

	res, err := goSevenZipInternal(context.Background(), slog.Default(), archive, outDir, "", Options{})
	if err != nil {
		t.Fatalf("goSevenZipInternal() error: %v", err)
	}
	if !strings.Contains(res.Output, "Extracting  ") {
		t.Fatalf("goSevenZipInternal() Output = %q, want an Extracting line", res.Output)
	}
	if got := dirTree(t, outDir); len(got) == 0 {
		t.Fatal("goSevenZipInternal() wrote nothing into the directory it was given")
	}
}

// TestExtractSevenZipFile_ChecksumMismatchNeverPublishes is the regression
// guard for issue #82 at the level closest to the bug: it calls
// extractSevenZipFile directly (bypassing GoSevenZip's staging directory,
// which would discard a failed attempt's files) with a tampered CRC32, and verifies the
// corrupt content never becomes visible at destPath in the first place --
// not that it's cleaned up afterward, but that the CRC check gates
// publishing before any rename occurs.
func TestExtractSevenZipFile_ChecksumMismatchNeverPublishes(t *testing.T) {
	t.Parallel()

	td := sevenZipTestdata(t)
	r, err := sevenzip.OpenReader(filepath.Join(td, "lzma2.7z"))
	if err != nil {
		t.Fatalf("sevenzip.OpenReader: %v", err)
	}
	defer r.Close()
	if len(r.File) == 0 {
		t.Fatal("archive has no files")
	}

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	defer root.Close()

	// Shallow-copy the real file entry and corrupt its recorded CRC32 so it
	// can never match the (correct) decompressed content -- isolating the
	// checksum-mismatch path without needing a corrupted archive fixture.
	f := *r.File[0]
	f.CRC32 = f.CRC32 ^ 0xFFFFFFFF //nolint:staticcheck // deliberate: guarantee mismatch regardless of original value

	err = extractSevenZipFile(t.Context(), root, "out.txt", filepath.Join(outDir, "out.txt"), &f, Options{}, slog.Default())
	if !errors.Is(err, errSevenZipChecksum) {
		t.Fatalf("extractSevenZipFile error = %v, want errSevenZipChecksum", err)
	}

	if _, statErr := os.Stat(filepath.Join(outDir, "out.txt")); !os.IsNotExist(statErr) {
		t.Errorf("out.txt exists after checksum mismatch; want never published (stat err: %v)", statErr)
	}
	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("outDir not empty after checksum mismatch: %v", entries)
	}
}

func TestExtractSevenZipEntry_Coverage(t *testing.T) {
	t.Parallel()
	td := sevenZipTestdata(t)
	r, err := sevenzip.OpenReader(filepath.Join(td, "lzma2.7z"))
	if err != nil {
		t.Fatalf("open lzma2.7z: %v", err)
	}
	defer r.Close()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	// Normal extraction via extractSevenZipEntry
	extracted, err := extractSevenZipEntry(t.Context(), r.File[0], outDir, root, Options{}, slog.Default())
	if err != nil || !extracted {
		t.Errorf("expected successful extraction, got extracted=%v err=%v", extracted, err)
	}

	// OneFolder mode without overwrite (triggers uniquePath)
	extracted, err = extractSevenZipEntry(t.Context(), r.File[0], outDir, root, Options{OneFolder: true}, slog.Default())
	if err != nil || !extracted {
		t.Errorf("expected OneFolder extraction, got extracted=%v err=%v", extracted, err)
	}

	// Bad path (should return false, nil and skip)
	badFile := *r.File[0]
	badFile.Name = "../../etc/passwd"
	var onlineMsg string
	extracted, err = extractSevenZipEntry(t.Context(), &badFile, outDir, root, Options{
		OnLine: func(msg string) { onlineMsg = msg },
	}, slog.Default())
	if err != nil || extracted {
		t.Errorf("expected skip on bad path, got extracted=%v err=%v", extracted, err)
	}
	if !strings.Contains(onlineMsg, "Skipping bad path") {
		t.Errorf("expected OnLine callback for bad path, got: %q", onlineMsg)
	}

	// Directory entry (Attributes 0x10)
	dirFile := *r.File[0]
	dirFile.Name = "testdir"
	dirFile.Attributes = 0x10
	extracted, err = extractSevenZipEntry(t.Context(), &dirFile, outDir, root, Options{}, slog.Default())
	if err != nil || extracted {
		t.Errorf("expected directory skip, got extracted=%v err=%v", extracted, err)
	}

	// Symlink entry (UNIX S_IFLNK 0120000 in upper 16 bits = 0xA0000000)
	symFile := *r.File[0]
	symFile.Name = "testlink"
	symFile.Attributes = 0xA0000000
	extracted, err = extractSevenZipEntry(t.Context(), &symFile, outDir, root, Options{
		OnLine: func(msg string) { onlineMsg = msg },
	}, slog.Default())
	if err != nil || extracted {
		t.Errorf("expected symlink skip, got extracted=%v err=%v", extracted, err)
	}
	if onlineMsg != "Skipping link: testlink" {
		t.Errorf("symlink skip reported as %q, want %q", onlineMsg, "Skipping link: testlink")
	}

	// Non-regular entry (UNIX S_IFCHR 0020000 in upper 16 bits = 0x20000000)
	charFile := *r.File[0]
	charFile.Name = "testchar"
	charFile.Attributes = 0x20000000
	extracted, err = extractSevenZipEntry(t.Context(), &charFile, outDir, root, Options{
		OnLine: func(msg string) { onlineMsg = msg },
	}, slog.Default())
	if err != nil || extracted {
		t.Errorf("expected non-regular skip, got extracted=%v err=%v", extracted, err)
	}
}

func TestExtractSevenZipFile_Coverage(t *testing.T) {
	t.Parallel()
	td := sevenZipTestdata(t)
	r, err := sevenzip.OpenReader(filepath.Join(td, "lzma2.7z"))
	if err != nil {
		t.Fatalf("open lzma2.7z: %v", err)
	}
	defer r.Close()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	f := r.File[0]

	// Extract first time
	err = extractSevenZipFile(t.Context(), root, "exist_test", filepath.Join(outDir, "exist_test"), f, Options{}, slog.Default())
	if err != nil {
		t.Fatalf("extract first time: %v", err)
	}

	// Extract second time with OverwriteFiles = false (should skip existing)
	var onlineMsg string
	err = extractSevenZipFile(t.Context(), root, "exist_test", filepath.Join(outDir, "exist_test"), f, Options{
		OverwriteFiles: false,
		OnLine:         func(msg string) { onlineMsg = msg },
	}, slog.Default())
	if err != nil {
		t.Errorf("extract second time: %v", err)
	}
	if !strings.Contains(onlineMsg, "Skipping existing") {
		t.Errorf("expected OnLine callback for existing file, got: %q", onlineMsg)
	}

	// Extract with IgnoreUnrarDates = false and valid modification time
	err = extractSevenZipFile(t.Context(), root, "date_test", filepath.Join(outDir, "date_test"), f, Options{
		OverwriteFiles:   true,
		IgnoreUnrarDates: false,
	}, slog.Default())
	if err != nil {
		t.Errorf("extract with date: %v", err)
	}
}

type testDebugHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *testDebugHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelDebug
}

func (h *testDebugHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *testDebugHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *testDebugHandler) WithGroup(name string) slog.Handler       { return h }

type unlinkingContext struct {
	context.Context
	destPath string
	unlinked bool
}

// Err runs on every contextCopy iteration. Since writeEntrySafely now writes
// through a temp sibling file (published to destPath only via rename after
// the write completes — see the atomic-publish rework), destPath itself
// doesn't exist yet to unlink. Instead, this finds and unlinks the
// in-progress temp file (named ".gonzbd-tmp-<random>", per
// fsutil.RootedCreateTemp -- independent of destPath's own basename, so a
// near-NAME_MAX entry name doesn't overflow the temp name) out from under
// the open descriptor, exploiting the same POSIX unlink-while-open
// semantics to force the post-write Chmod/Chtimes calls (which operate on
// the temp name pre-rename) to fail.
func (u *unlinkingContext) Err() error {
	// Only mark unlinked once a temp file was actually found and removed --
	// RootedCreateTemp now checks ctx.Err() before the temp file exists
	// (during its own setup/retry-loop guard), so an early call here would
	// otherwise consume the one-shot unlink against an empty directory
	// before there's anything to remove.
	if !u.unlinked {
		dir := filepath.Dir(u.destPath)
		const prefix = ".gonzbd-tmp-"
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), prefix) {
					if rmErr := os.Remove(filepath.Join(dir, e.Name())); rmErr == nil {
						u.unlinked = true
					}
				}
			}
		}
	}
	return u.Context.Err()
}

func TestExtractSevenZipFile_TempFileUnlinkedMidWrite(t *testing.T) {
	td := sevenZipTestdata(t)
	r, err := sevenzip.OpenReader(filepath.Join(td, "lzma2.7z"))
	if err != nil {
		t.Fatalf("open lzma2.7z: %v", err)
	}
	defer r.Close()

	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	f := r.File[0]

	handler := &testDebugHandler{}
	logger := slog.New(handler)

	destRel := "perm_test"
	destPath := filepath.Join(outDir, destRel)

	ctx := &unlinkingContext{Context: context.Background(), destPath: destPath}
	err = extractSevenZipFile(ctx, root, destRel, destPath, f, Options{
		OverwriteFiles:   true,
		IgnoreUnrarDates: false,
	}, logger)
	// dupcomment:ok the go_sevenzip and go_unrar backends are asserted
	// against the same publish contract; the two copies must stay in step.
	//
	// The temp file backing this write is unlinked mid-copy (see
	// unlinkingContext.Err), so Chmod/Chtimes on it fail (soft, logged at
	// debug — checked below) but the final publish rename now also fails
	// hard, since its source path no longer exists. This is the correct,
	// improved behavior under atomic publish: a file that vanishes between
	// write and publish must surface as a real extraction error, not
	// silently succeed with a file that was never actually published.
	if err == nil {
		t.Fatal("extractSevenZipFile: expected a publish error, got nil")
	}
	if !strings.Contains(err.Error(), "publish") {
		t.Errorf("extractSevenZipFile error = %v, want a publish-step error", err)
	}

	handler.mu.Lock()
	records := append([]slog.Record(nil), handler.records...)
	handler.mu.Unlock()

	var foundChmod, foundChtimes bool
	for _, rec := range records {
		if rec.Level == slog.LevelDebug && strings.Contains(rec.Message, "failed to set file permissions") {
			foundChmod = true
		}
		if rec.Level == slog.LevelDebug && strings.Contains(rec.Message, "failed to set file timestamps") {
			foundChtimes = true
		}
	}
	if !foundChmod {
		t.Error("expected debug log for failed Chmod")
	}
	if !foundChtimes {
		t.Error("expected debug log for failed Chtimes")
	}
}

func TestGoSevenZipWithPasswords_StoredEncryptedArchiveTriesSecondPassword(t *testing.T) {
	sevenZipPath, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z binary not found")
	}
	dir := t.TempDir()
	contentFile := filepath.Join(dir, "content.txt")
	const payload = "stored encrypted payload content 1234567890"
	if err := os.WriteFile(contentFile, []byte(payload), 0o600); err != nil {
		t.Fatalf("write content file: %v", err)
	}
	archivePath := filepath.Join(dir, "stored_encrypted.7z")
	cmd := exec.Command(sevenZipPath, "a", "-t7z", "-mx=0", "-psecret123", archivePath, contentFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create 7z archive failed: %v, output: %s", err, out)
	}

	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: archivePath,
	}
	opts := Options{
		Passwords: []string{"wrongpass", "secret123"},
	}
	res, err := GoSevenZipWithPasswords(context.Background(), slog.Default(), archive, outDir, opts)
	if err != nil {
		t.Fatalf("GoSevenZipWithPasswords() expected success on second password, got error: %v, reason: %v", err, res.Reason)
	}
	extractedFile := filepath.Join(outDir, "content.txt")
	data, err := os.ReadFile(extractedFile)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != payload {
		t.Fatalf("extracted content = %q, want %q", string(data), payload)
	}

	// Verify that an unencrypted corrupted archive still fails with FailCorrupt and does not try all passwords.
	corruptPath := corruptedCopySevenZip(t, t.TempDir())
	corruptArchive := Archive{
		Type:     SevenZipArchive,
		MainFile: corruptPath,
	}
	corruptOut := t.TempDir()
	resCorrupt, errCorrupt := GoSevenZipWithPasswords(context.Background(), slog.Default(), corruptArchive, corruptOut, opts)
	if errCorrupt == nil {
		t.Fatal("expected corrupted archive extraction to fail, got nil")
	}
	if resCorrupt.Reason != FailCorrupt {
		t.Fatalf("resCorrupt.Reason = %v, want %v (FailCorrupt)", resCorrupt.Reason, FailCorrupt)
	}
}

func TestIsSevenZipEncrypted(t *testing.T) {
	sevenZipPath, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z binary not found")
	}

	dir := t.TempDir()
	contentFile := filepath.Join(dir, "content.txt")
	const payload = "stored encrypted payload content 1234567890"
	if err := os.WriteFile(contentFile, []byte(payload), 0o600); err != nil {
		t.Fatalf("write content file: %v", err)
	}

	// 1. Stored encrypted archive (unencrypted header, encrypted stream)
	storedEncPath := filepath.Join(dir, "stored_enc.7z")
	cmd := exec.Command(sevenZipPath, "a", "-t7z", "-mx=0", "-psecret123", storedEncPath, contentFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create stored_enc.7z failed: %v, output: %s", err, out)
	}

	// 2. Encrypted header archive (-mhe=on)
	headerEncPath := filepath.Join(dir, "header_enc.7z")
	cmdHeader := exec.Command(sevenZipPath, "a", "-t7z", "-mhe=on", "-psecret123", headerEncPath, contentFile)
	if out, err := cmdHeader.CombinedOutput(); err != nil {
		t.Fatalf("create header_enc.7z failed: %v, output: %s", err, out)
	}

	// 3. Unencrypted archive
	unencPath := filepath.Join(dir, "unenc.7z")
	cmdUnenc := exec.Command(sevenZipPath, "a", "-t7z", unencPath, contentFile)
	if out, err := cmdUnenc.CombinedOutput(); err != nil {
		t.Fatalf("create unenc.7z failed: %v, output: %s", err, out)
	}

	// Case A: Stored encrypted, named entry
	if !isSevenZipEncrypted(storedEncPath, "wrongpass", "content.txt") {
		t.Error("isSevenZipEncrypted(storedEnc, wrongpass, content.txt) = false, want true")
	}

	// Case B: Stored encrypted, empty entryName (finds first regular file)
	if !isSevenZipEncrypted(storedEncPath, "wrongpass", "") {
		t.Error("isSevenZipEncrypted(storedEnc, wrongpass, '') = false, want true")
	}

	// Case B2: Stored encrypted, empty password provided (Open fails with ReadError.Encrypted)
	if !isSevenZipEncrypted(storedEncPath, "", "content.txt") {
		t.Error("isSevenZipEncrypted(storedEnc, '', content.txt) = false, want true")
	}

	// Case C: Encrypted header (-mhe=on)
	if !isSevenZipEncrypted(headerEncPath, "wrongpass", "") {
		t.Error("isSevenZipEncrypted(headerEnc, wrongpass, '') = false, want true")
	}

	// Case D: Unencrypted archive, named entry
	if isSevenZipEncrypted(unencPath, "wrongpass", "content.txt") {
		t.Error("isSevenZipEncrypted(unenc, wrongpass, content.txt) = true, want false")
	}

	// Case E: Unencrypted archive, empty entryName
	if isSevenZipEncrypted(unencPath, "wrongpass", "") {
		t.Error("isSevenZipEncrypted(unenc, wrongpass, '') = true, want false")
	}

	// Case F: Non-existent entryName
	if isSevenZipEncrypted(storedEncPath, "wrongpass", "nonexistent.file") {
		t.Error("isSevenZipEncrypted(storedEnc, wrongpass, nonexistent) = true, want false")
	}

	// Case G: Non-existent archive path
	if isSevenZipEncrypted(filepath.Join(dir, "does_not_exist.7z"), "wrongpass", "") {
		t.Error("isSevenZipEncrypted(nonexistent archive) = true, want false")
	}
}
