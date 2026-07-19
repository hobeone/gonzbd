package unpack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/rarengine"

	"github.com/hobeone/gonzbd/internal/rarheader"
)

func TestGoUnRAR_SingleVolume(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "single_rar5",
		MainFile: filepath.Join("testdata", "single_rar5.rar"),
	}

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoUnRAR() extracted no files")
	}

	// Verify res.Output contains extraction lines
	for _, name := range []string{"file1.txt", "file2.txt"} {
		wantLine := "Extracting  " + name
		if !strings.Contains(res.Output, wantLine) {
			t.Errorf("expected res.Output to contain %q, got: %q", wantLine, res.Output)
		}
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

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

// TestGoUnRAR_MultiVolume_LegacyNaming reproduces a production bug: a RAR5
// multi-volume archive split using the legacy ".rar"/".r00"/".r01" naming
// convention (still common -- WinRAR's "old style volume names" option is
// independent of archive format) instead of new-style ".partNN.rar". The
// RAR5 binary stream itself doesn't encode filenames for volume
// continuation, so copying the (already-verified-correct) multi_new
// .partNN.rar fixtures under legacy names produces an equally valid
// legacy-named multi-volume RAR5 set.
//
// Before the fix, GoUnRAREngine's volume discovery only recognized ".part"
// in the filename and silently treated this as a single-volume archive,
// so rarengine ran out of data partway through and failed with
// "rarengine: expected next volume stream from channel, but channel was
// closed" -- exactly the error reported in production logs.
func TestGoUnRAR_MultiVolume_LegacyNaming(t *testing.T) {
	srcDir := "testdata"
	parts := []string{
		"multi_new.part01.rar", "multi_new.part02.rar", "multi_new.part03.rar",
		"multi_new.part04.rar", "multi_new.part05.rar", "multi_new.part06.rar",
		"multi_new.part07.rar", "multi_new.part08.rar", "multi_new.part09.rar",
		"multi_new.part10.rar",
	}
	legacyNames := []string{
		"multi_legacy.rar", "multi_legacy.r00", "multi_legacy.r01", "multi_legacy.r02",
		"multi_legacy.r03", "multi_legacy.r04", "multi_legacy.r05", "multi_legacy.r06",
		"multi_legacy.r07", "multi_legacy.r08",
	}

	archiveDir := t.TempDir()
	for i, src := range parts {
		data, err := os.ReadFile(filepath.Join(srcDir, src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(archiveDir, legacyNames[i]), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	archives, err := Scan(archiveDir)
	if err != nil {
		t.Fatalf("Scan() error: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("Scan() found %d archives, want 1", len(archives))
	}
	archive := archives[0]
	if len(archive.Parts) != len(legacyNames) {
		t.Fatalf("archive.Parts has %d entries, want %d", len(archive.Parts), len(legacyNames))
	}

	outDir := t.TempDir()
	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoUnRAR() extracted no files")
	}

	path := filepath.Join(outDir, "bigfile.bin")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("bigfile.bin not found: %v", err)
	}
	if info.Size() != 8192 {
		t.Errorf("bigfile.bin size = %d, want 8192", info.Size())
	}
}

// TestGoUnRAR_MultiVolume_LegacyNaming_NoParts exercises the disk-probing
// fallback (discoverRar5Volumes) directly, by constructing an Archive with
// only MainFile set -- no Parts -- the same way TestGoUnRAR_MultiVolume
// does for new-style naming. This proves the fallback path independently
// recognizes legacy ".rNN" siblings too, not just the Archive.Parts path
// exercised by TestGoUnRAR_MultiVolume_LegacyNaming: a caller that builds
// Archive by hand (bypassing Scan) for a legacy-named set must not hit the
// same "channel was closed" bug the Parts-based path was fixed for.
func TestGoUnRAR_MultiVolume_LegacyNaming_NoParts(t *testing.T) {
	srcDir := "testdata"
	parts := []string{
		"multi_new.part01.rar", "multi_new.part02.rar", "multi_new.part03.rar",
		"multi_new.part04.rar", "multi_new.part05.rar", "multi_new.part06.rar",
		"multi_new.part07.rar", "multi_new.part08.rar", "multi_new.part09.rar",
		"multi_new.part10.rar",
	}
	legacyNames := []string{
		"multi_legacy.rar", "multi_legacy.r00", "multi_legacy.r01", "multi_legacy.r02",
		"multi_legacy.r03", "multi_legacy.r04", "multi_legacy.r05", "multi_legacy.r06",
		"multi_legacy.r07", "multi_legacy.r08",
	}

	archiveDir := t.TempDir()
	for i, src := range parts {
		data, err := os.ReadFile(filepath.Join(srcDir, src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(archiveDir, legacyNames[i]), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	archive := Archive{
		Type:     RarArchive,
		Name:     "multi_legacy",
		MainFile: filepath.Join(archiveDir, "multi_legacy.rar"),
		// Parts intentionally left nil to force the discoverRar5Volumes
		// fallback path.
	}

	outDir := t.TempDir()
	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{})
	if err != nil {
		t.Fatalf("GoUnRAR() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoUnRAR() extracted no files")
	}

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

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "testpass", Options{})
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

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "wrongpassword", Options{})
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
	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{})
	if err == nil {
		t.Fatal("GoUnRAR() expected error for encrypted header without password")
	}
	if res.Reason != FailWrongPassword {
		t.Errorf("Reason = %v, want FailWrongPassword", res.Reason)
	}

	// With correct password — should succeed.
	outDir2 := t.TempDir()
	res, err = GoUnRAR(context.Background(), slog.Default(), archive, outDir2, "testpass", Options{})
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

	var onlineCalled bool
	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{
		OnLine: func(msg string) { onlineCalled = true },
	})
	if err == nil {
		t.Fatal("GoUnRAR() expected error for corrupt archive")
	}
	if !onlineCalled {
		t.Error("expected OnLine to be called on corrupt archive error")
	}
	if res.Reason != FailCorrupt {
		t.Errorf("Reason = %v, want FailCorrupt", res.Reason)
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

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	_, err := GoUnRAR(ctx, slog.Default(), archive, outDir, "", Options{})
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

	res, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{
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

	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{
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
	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{
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

	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{})
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

	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{
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

	_, err := GoUnRAR(context.Background(), slog.Default(), archive, outDir, "", Options{
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

// --- os.Root zip-slip regression tests for ExtractEntryRarengine ---

// TestExtractEntryRarengine_Normal verifies that a normal entry is extracted
// correctly when written through an os.Root.
func TestExtractEntryRarengine_Normal(t *testing.T) {
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	fh := &rarengine.FileHeader{Name: "subdir/hello.txt"}
	content := []byte("hello world")
	destRel := "subdir/hello.txt"
	destPath := filepath.Join(outDir, destRel)

	if err := ExtractEntryRarengine(
		context.Background(), root, outDir, destRel, destPath,
		fh, strings.NewReader(string(content)), Options{}, slog.Default(),
	); err != nil {
		t.Fatalf("ExtractEntryRarengine: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestExtractEntryRarengine_TempFileUnlinkedMidWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping test that relies on POSIX unlink-while-open semantics")
	}
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	fh := &rarengine.FileHeader{
		Name:             "perm_test/hello.txt",
		HostOS:           2, // Unix
		Attributes:       0644,
		ModificationTime: time.Now(),
	}

	handler := &testDebugHandler{}
	logger := slog.New(handler)

	destRel := "perm_test/hello.txt"
	destPath := filepath.Join(outDir, destRel)

	ctx := &unlinkingContext{Context: context.Background(), destPath: destPath}
	err = ExtractEntryRarengine(
		ctx, root, outDir, destRel, destPath,
		fh, strings.NewReader("hello world"), Options{
			OverwriteFiles:   true,
			IgnoreUnrarDates: false,
		}, logger,
	)
	// The temp file backing this write is unlinked mid-copy (see
	// unlinkingContext.Err), so Chmod/Chtimes on it fail (soft, logged at
	// debug — checked below) but the final publish rename now also fails
	// hard, since its source path no longer exists. This is the correct,
	// improved behavior under atomic publish: a file that vanishes between
	// write and publish must surface as a real extraction error, not
	// silently succeed with a file that was never actually published.
	if err == nil {
		t.Fatal("ExtractEntryRarengine: expected a publish error, got nil")
	}
	if !strings.Contains(err.Error(), "publish") {
		t.Errorf("ExtractEntryRarengine error = %v, want a publish-step error", err)
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

// TestExtractEntryRarengine_ZipSlip_SymlinkComponent verifies that os.Root
// refuses to write through a symlinked path component inside the output dir
// that resolves outside. This is the security case lexical SanitizeArchivePath
// alone cannot catch: the relative path "link/evil.txt" looks safe lexically,
// but "link" is a symlink to an external directory.
func TestExtractEntryRarengine_ZipSlip_SymlinkComponent(t *testing.T) {
	outDir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "evil.txt")

	// Plant a symlink inside outDir pointing out.
	if err := os.Symlink(outside, filepath.Join(outDir, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	fh := &rarengine.FileHeader{Name: "link/evil.txt"}
	destRel := "link/evil.txt"
	destPath := filepath.Join(outDir, destRel) // lexically inside, resolves outside

	extractErr := ExtractEntryRarengine(
		context.Background(), root, outDir, destRel, destPath,
		fh, strings.NewReader("EVIL"), Options{}, slog.Default(),
	)
	if extractErr == nil {
		t.Fatal("expected error: os.Root must refuse write through symlinked component")
	}

	// The victim outside the root must not have been created.
	if _, statErr := os.Stat(victim); !os.IsNotExist(statErr) {
		t.Fatal("SECURITY: file was created outside output dir via symlinked component")
	}
}

// TestExtractEntryRarengine_ZipSlip_DotDot verifies that a dotdot path
// (which SanitizeArchivePath should already catch) is also refused by
// os.Root as defense-in-depth.
func TestExtractEntryRarengine_ZipSlip_DotDot(t *testing.T) {
	outDir := t.TempDir()
	outside := t.TempDir()
	_ = outside

	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	fh := &rarengine.FileHeader{Name: "../escape.txt"}
	// Intentionally bypass SanitizeArchivePath to test the root enforcement.
	destRel := "../escape.txt"
	destPath := filepath.Join(outDir, destRel)

	extractErr := ExtractEntryRarengine(
		context.Background(), root, outDir, destRel, destPath,
		fh, strings.NewReader("EVIL"), Options{}, slog.Default(),
	)
	if extractErr == nil {
		t.Fatal("expected error: os.Root must refuse dotdot path")
	}

	// Nothing must be written outside outDir.
	escapePath := filepath.Join(filepath.Dir(outDir), "escape.txt")
	if _, statErr := os.Stat(escapePath); !os.IsNotExist(statErr) {
		t.Fatal("SECURITY: file was created outside output dir via dotdot path")
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

func TestDetectRarVersionDirect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("valid rar5 signature", func(t *testing.T) {
		path := filepath.Join(dir, "rar5.rar")
		signature := []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x01, 0x00}
		if err := os.WriteFile(path, signature, 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		got, err := rarheader.Version(path)
		if err != nil {
			t.Fatalf("rarheader.Version error: %v", err)
		}
		if got != 5 {
			t.Errorf("expected version 5 for RAR5 signature, got %d", got)
		}
	})

	t.Run("valid rar3 signature", func(t *testing.T) {
		path := filepath.Join(dir, "rar3.rar")
		signature := []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x00, 0x00}
		if err := os.WriteFile(path, signature, 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		got, err := rarheader.Version(path)
		if err != nil {
			t.Fatalf("rarheader.Version error: %v", err)
		}
		if got != 3 {
			t.Errorf("expected version 3 for RAR3 signature, got %d", got)
		}
	})

	t.Run("too short file", func(t *testing.T) {
		path := filepath.Join(dir, "short.rar")
		if err := os.WriteFile(path, []byte("Rar!"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		_, err := rarheader.Version(path)
		if !errors.Is(err, rarheader.ErrNotRAR) {
			t.Errorf("expected ErrNotRAR for short file, got %v", err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(dir, "empty.rar")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		_, err := rarheader.Version(path)
		if !errors.Is(err, rarheader.ErrNotRAR) {
			t.Errorf("expected ErrNotRAR for empty file, got %v", err)
		}
	})

	t.Run("non-existent file", func(t *testing.T) {
		_, err := rarheader.Version(filepath.Join(dir, "non-existent"))
		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})
}

func TestGoUnRAREngine_PanicRecovery(t *testing.T) {
	archive := Archive{
		Type:     RarArchive,
		MainFile: "dummy.rar",
	}
	var onlineMsg string
	res, err := GoUnRAREngine(context.Background(), nil, archive, t.TempDir(), "", Options{
		OnLine: func(msg string) { onlineMsg = msg },
	})
	if err == nil {
		t.Fatal("expected error from panic recovery, got nil")
	}
	if !strings.Contains(err.Error(), "rarengine panic") {
		t.Errorf("expected error to contain 'rarengine panic', got: %v", err)
	}
	if res.Reason != FailCorrupt {
		t.Errorf("expected res.Reason to be FailCorrupt, got: %v", res.Reason)
	}
	if !strings.Contains(onlineMsg, "ERROR: rarengine panic:") {
		t.Errorf("expected OnLine callback to receive 'ERROR: rarengine panic:', got: %q", onlineMsg)
	}
}

func TestGoUnRAREngineInternal_Direct(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "single_rar5",
		MainFile: filepath.Join("testdata", "single_rar5.rar"),
	}

	res, err := goUnRAREngineInternal(context.Background(), slog.Default(), archive, outDir, "", Options{})
	if err != nil {
		t.Fatalf("goUnRAREngineInternal() error: %v", err)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("goUnRAREngineInternal() extracted no files")
	}
}

func TestDiscoverPartNNVolumes(t *testing.T) {
	t.Run("multi-volume part01", func(t *testing.T) {
		vols, err := discoverPartNNVolumes(filepath.Join("testdata", "multi_new.part01.rar"))
		if err != nil {
			t.Fatalf("discoverPartNNVolumes() error: %v", err)
		}
		if len(vols) != 10 {
			t.Errorf("expected 10 volumes, got %d: %v", len(vols), vols)
		}
	})

	t.Run("non-part file", func(t *testing.T) {
		vols, err := discoverPartNNVolumes(filepath.Join("testdata", "single_rar5.rar"))
		if err != nil {
			t.Fatalf("discoverPartNNVolumes() error: %v", err)
		}
		if len(vols) != 1 || vols[0] != filepath.Join("testdata", "single_rar5.rar") {
			t.Errorf("expected single volume, got: %v", vols)
		}
	})
}

func TestGoUnRAREngineInternal_ErrorBranches(t *testing.T) {
	t.Run("non-existent outDir", func(t *testing.T) {
		archive := Archive{Type: RarArchive, MainFile: filepath.Join("testdata", "single_rar5.rar")}
		_, err := goUnRAREngineInternal(context.Background(), slog.Default(), archive, filepath.Join(t.TempDir(), "missing", "subdir"), "", Options{})
		if err == nil {
			t.Error("expected error for missing outDir")
		}
	})

	t.Run("missing archive file", func(t *testing.T) {
		archive := Archive{Type: RarArchive, MainFile: "/non/existent/file.rar"}
		res, err := goUnRAREngineInternal(context.Background(), slog.Default(), archive, t.TempDir(), "", Options{})
		if err == nil {
			t.Error("expected error for missing archive file")
		}
		if res.Reason != FailMissingVolume {
			t.Errorf("got reason %v, want FailMissingVolume", res.Reason)
		}
	})

	t.Run("missing volume part in Parts list", func(t *testing.T) {
		archive := Archive{
			Type:     RarArchive,
			MainFile: filepath.Join("testdata", "multi_new.part01.rar"),
			Parts:    []string{filepath.Join("testdata", "multi_new.part01.rar"), "/non/existent/part02.rar"},
		}
		res, err := goUnRAREngineInternal(context.Background(), slog.Default(), archive, t.TempDir(), "", Options{})
		if err == nil {
			t.Error("expected error for missing volume part")
		}
		if res.Reason != FailMissingVolume {
			t.Errorf("got reason %v, want FailMissingVolume", res.Reason)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		archive := Archive{Type: RarArchive, MainFile: filepath.Join("testdata", "single_rar5.rar")}
		_, err := goUnRAREngineInternal(ctx, slog.Default(), archive, t.TempDir(), "", Options{})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	})
}
