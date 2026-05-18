package unpack

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	rardecode "github.com/nwaples/rardecode/v2"
)

func TestGoUnRAR_SingleVolume(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "single_rar5",
		MainFile: filepath.Join("testdata", "single_rar5.rar"),
	}

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoUnRAR() extracted no files")
	}

	// Verify expected files exist.
	for _, name := range []string{"file1.txt", "file2.txt"} {
		path := filepath.Join(outDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("expected file %s not found: %v", name, err)
			continue
		}
		want := fmt.Sprintf("Hello from %s\n", name)
		if string(data) != want {
			t.Errorf("file %s: got %q, want %q", name, data, want)
		}
	}

	// Verify nested file.
	nested := filepath.Join(outDir, "subdir", "nested.txt")
	data, err := os.ReadFile(nested)
	if err != nil {
		t.Errorf("nested file not found: %v", err)
	} else if string(data) != "Nested file content\n" {
		t.Errorf("nested file: got %q", data)
	}
}

func TestGoUnRAR_MultiVolume(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "multi_new",
		MainFile: filepath.Join("testdata", "multi_new.part01.rar"),
	}

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoUnRAR() extracted no files")
	}

	// Verify the bigfile.bin was extracted.
	path := filepath.Join(outDir, "bigfile.bin")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("bigfile.bin not found: %v", err)
	}
	if info.Size() != 8192 {
		t.Errorf("bigfile.bin size = %d, want 8192", info.Size())
	}
}

func TestGoUnRAR_PasswordCorrect(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "password_rar5",
		MainFile: filepath.Join("testdata", "password_rar5.rar"),
	}

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{
		Password: "testpass",
	})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoUnRAR() extracted no files")
	}
}

func TestGoUnRAR_PasswordWrong(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "password_rar5",
		MainFile: filepath.Join("testdata", "password_rar5.rar"),
	}

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{
		Password: "wrongpassword",
	})
	if err == nil {
		t.Fatal("GoUnRAR() expected error for wrong password")
	}
	if res.Reason != FailWrongPassword {
		t.Errorf("Reason = %v, want FailWrongPassword", res.Reason)
	}
}

func TestGoUnRAR_EncryptedHeader(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "encrypted_header",
		MainFile: filepath.Join("testdata", "encrypted_header.rar"),
	}

	// Without password — should fail.
	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{})
	if err == nil {
		t.Fatal("GoUnRAR() expected error for encrypted header without password")
	}
	if res.Reason != FailWrongPassword {
		t.Errorf("Reason = %v, want FailWrongPassword", res.Reason)
	}

	// With correct password — should succeed.
	outDir2 := t.TempDir()
	res, err = GoUnRAR(context.Background(), slog.Default(), archive, outDir2, Options{
		Password: "testpass",
	})
	if err != nil {
		t.Fatalf("GoUnRAR() with password error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoUnRAR() extracted no files")
	}
}

func TestGoUnRAR_Corrupt(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "corrupt",
		MainFile: filepath.Join("testdata", "corrupt.rar"),
	}

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{})
	if err == nil {
		t.Fatal("GoUnRAR() expected error for corrupt archive")
	}
	// Should classify as corrupt, not panic.
	if res.Reason != FailCorrupt && res.Reason != FailNotArchive {
		t.Errorf("Reason = %v, want FailCorrupt or FailNotArchive", res.Reason)
	}
}

func TestGoUnRAR_NotAnArchive(t *testing.T) {
	outDir := t.TempDir()

	// Create a non-RAR file.
	notRar := filepath.Join(t.TempDir(), "notrar.rar")
	if err := os.WriteFile(notRar, []byte("this is not a rar file at all"), 0600); err != nil {
		t.Fatal(err)
	}

	archive := Archive{
		Type:     RarArchive,
		Name:     "notrar",
		MainFile: notRar,
	}

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{})
	if err == nil {
		t.Fatal("GoUnRAR() expected error for non-RAR file")
	}
	if res.Reason != FailNotArchive {
		t.Errorf("Reason = %v, want FailNotArchive", res.Reason)
	}
}

func TestGoUnRAR_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "single_rar5",
		MainFile: filepath.Join("testdata", "single_rar5.rar"),
	}

	_, err := GoUnRAR(ctx, slog.Default(), archive, outDir, Options{})
	if err == nil {
		t.Fatal("GoUnRAR() expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestGoUnRAR_OneFolder(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "single_rar5",
		MainFile: filepath.Join("testdata", "single_rar5.rar"),
	}

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{
		OneFolder: true,
	})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoUnRAR() extracted no files")
	}

	// In OneFolder mode, subdir/nested.txt should be extracted as just "nested.txt".
	nested := filepath.Join(outDir, "nested.txt")
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("OneFolder: expected nested.txt in root, not found: %v", err)
	}

	// The subdir itself should not exist.
	subdir := filepath.Join(outDir, "subdir")
	if _, err := os.Stat(subdir); err == nil {
		t.Error("OneFolder: subdir/ should not exist")
	}
}

func TestGoUnRAR_IgnoreUnrarDates(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "single_rar5",
		MainFile: filepath.Join("testdata", "single_rar5.rar"),
	}

	before := time.Now().Add(-time.Second) // just before extraction

	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{
		IgnoreUnrarDates: true,
	})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}

	// When IgnoreUnrarDates is true, file mod times should be recent (not from archive).
	path := filepath.Join(outDir, "file1.txt")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Before(before) {
		t.Errorf("IgnoreUnrarDates: file mod time %v is older than extraction start %v",
			info.ModTime(), before)
	}
}

func TestGoUnRAR_OnLineCallback(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "single_rar5",
		MainFile: filepath.Join("testdata", "single_rar5.rar"),
	}

	var lines []string
	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{
		OnLine: func(line string) { lines = append(lines, line) },
	})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}

	if len(lines) == 0 {
		t.Error("OnLine callback was never called")
	}

	// All lines should start with "Extracting  ".
	for _, line := range lines {
		if !strings.HasPrefix(line, "Extracting  ") {
			t.Errorf("unexpected OnLine output: %q", line)
		}
	}
}

func TestGoUnRAR_WithDirs(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "with_dirs",
		MainFile: filepath.Join("testdata", "with_dirs.rar"),
	}

	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}

	// Verify the directory structure was created.
	nested := filepath.Join(outDir, "subdir", "nested.txt")
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("nested file not found: %v", err)
	}
}

// TestGoUnRAR_OverwriteFilesFalse verifies that GoUnRAR skips existing
// files when OverwriteFiles is false (the default). This is a regression
// test for C1: GoUnRAR previously always truncated with O_TRUNC.
func TestGoUnRAR_OverwriteFilesFalse(t *testing.T) {
	outDir := t.TempDir()

	// Pre-create a file that the archive would extract.
	preExisting := filepath.Join(outDir, "file1.txt")
	sentinel := "DO NOT OVERWRITE ME\n"
	if err := os.WriteFile(preExisting, []byte(sentinel), 0600); err != nil {
		t.Fatal(err)
	}

	archive := Archive{
		Type:     RarArchive,
		Name:     "single_rar5",
		MainFile: filepath.Join("testdata", "single_rar5.rar"),
	}

	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{
		OverwriteFiles: false,
	})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}

	// The pre-existing file should NOT have been overwritten.
	data, err := os.ReadFile(preExisting)
	if err != nil {
		t.Fatalf("reading pre-existing file: %v", err)
	}
	if string(data) != sentinel {
		t.Errorf("file1.txt was overwritten: got %q, want %q", data, sentinel)
	}
}

// TestGoUnRAR_OverwriteFilesTrue verifies that GoUnRAR does overwrite
// existing files when OverwriteFiles is true.
func TestGoUnRAR_OverwriteFilesTrue(t *testing.T) {
	outDir := t.TempDir()

	// Pre-create a file that the archive would extract.
	preExisting := filepath.Join(outDir, "file1.txt")
	sentinel := "OVERWRITE ME\n"
	if err := os.WriteFile(preExisting, []byte(sentinel), 0600); err != nil {
		t.Fatal(err)
	}

	archive := Archive{
		Type:     RarArchive,
		Name:     "single_rar5",
		MainFile: filepath.Join("testdata", "single_rar5.rar"),
	}

	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, Options{
		OverwriteFiles: true,
	})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}

	// The pre-existing file SHOULD have been overwritten.
	data, err := os.ReadFile(preExisting)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if string(data) == sentinel {
		t.Error("file1.txt was NOT overwritten when OverwriteFiles=true")
	}
}

// --- sanitizeArchivePath tests ---

func TestSanitizeArchivePath(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		oneFolder bool
		want      string
		wantErr   bool
	}{
		{"simple", "file.txt", false, "file.txt", false},
		{"nested", "subdir/file.txt", false, "subdir/file.txt", false},
		{"backslash", "subdir\\file.txt", false, "subdir/file.txt", false},
		{"leading_slash", "/etc/passwd", false, "etc/passwd", false},
		{"traversal", "../../etc/passwd", false, "", true},
		{"traversal_clean", "foo/../../etc/passwd", false, "", true},
		{"traversal_deep", "a/b/c/../../../../etc/shadow", false, "", true},
		{"traversal_backslash", "..\\..\\etc\\passwd", false, "", true},
		{"internal_dotdot_safe", "a/../b/file.txt", false, "b/file.txt", false},
		{"dotdot_component_only", "..", false, "", true},
		{"null_byte", "file\x00.txt", false, "", true},
		{"empty_after_clean", ".", false, "", true},
		{"one_folder_nested", "a/b/c.txt", true, "c.txt", false},
		{"one_folder_simple", "file.txt", true, "file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SanitizeArchivePath(tt.input, tt.oneFolder)
			if (err != nil) != tt.wantErr {
				t.Errorf("SanitizeArchivePath(%q, %v) error = %v, wantErr %v",
					tt.input, tt.oneFolder, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("SanitizeArchivePath(%q, %v) = %q, want %q",
					tt.input, tt.oneFolder, got, tt.want)
			}
		})
	}
}

// --- classifyRarDecodeError tests ---

func TestClassifyRarDecodeError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FailReason
	}{
		{"nil", nil, FailUnknown},
		{"bad_password", rardecode.ErrBadPassword, FailWrongPassword},
		{"archive_encrypted", rardecode.ErrArchiveEncrypted, FailWrongPassword},
		{"file_encrypted", rardecode.ErrArchivedFileEncrypted, FailWrongPassword},
		{"bad_checksum", rardecode.ErrBadFileChecksum, FailCorrupt},
		{"corrupt_block", rardecode.ErrCorruptBlockHeader, FailCorrupt},
		{"corrupt_file_hdr", rardecode.ErrCorruptFileHeader, FailCorrupt},
		{"bad_header_crc", rardecode.ErrBadHeaderCRC, FailCorrupt},
		{"huff_failed", rardecode.ErrHuffDecodeFailed, FailCorrupt},
		{"corrupt_ppm", rardecode.ErrCorruptPPM, FailCorrupt},
		{"short_file", rardecode.ErrShortFile, FailCorrupt},
		{"decoder_ood", rardecode.ErrDecoderOutOfData, FailCorrupt},
		{"dict_too_large", rardecode.ErrDictionaryTooLarge, FailCorrupt},
		{"no_sig", rardecode.ErrNoSig, FailNotArchive},
		{"unknown_ver", rardecode.ErrUnknownVersion, FailNotArchive},
		{"enospc", syscall.ENOSPC, FailDiskFull},
		{"not_exist", fs.ErrNotExist, FailMissingVolume},
		{"wrapped_not_exist", fmt.Errorf("open failed: %w", fs.ErrNotExist), FailMissingVolume},
		{"generic", errors.New("something else"), FailUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRarDecodeError(tt.err)
			if got != tt.want {
				t.Errorf("ClassifyRarDecodeError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
