package unpack

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// sevenZipTestdata returns the path to the bodgit/sevenzip testdata directory.
// Tests are skipped if the directory doesn't exist.
func sevenZipTestdata(t *testing.T) string {
	t.Helper()
	// Relative to the gonzbd project root: ../sevenzip/testdata
	dir := filepath.Join("..", "..", "..", "sevenzip", "testdata")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skipf("sevenzip testdata not found at %s (run tests from gonzbd root)", dir)
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
	t.Logf("Extracted %d files", len(res.ExtractedFiles))
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
