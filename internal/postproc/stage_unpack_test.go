package postproc

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// unpackFixture returns the absolute path to a file in the shared
// unpack testdata directory. Go tests run with cwd = package directory,
// so "../unpack/testdata" is stable and trimpath-safe.
func unpackFixture(name string) string {
	return filepath.Join("..", "unpack", "testdata", name)
}

// copyToDir copies src to dstDir/baseName and returns the destination path.
func copyToDir(t *testing.T, src, dstDir string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	dst := filepath.Join(dstDir, filepath.Base(src))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

// enabledUnpackStage builds an UnpackStage that is enabled and uses
// pure-Go RAR extraction so tests run without an external unrar binary.
func enabledUnpackStage() *UnpackStage {
	s := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, false)
	s.SetEnabled(true)
	return s
}

// ---------- Extraction path tests ----------

// TestUnpackStage_ExtractSingleRarGoMode verifies the GoRAR extraction path:
// a healthy single-volume RAR is extracted and the job has no UnpackError.
func TestUnpackStage_ExtractSingleRarGoMode(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	if err := enabledUnpackStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.UnpackError {
		t.Error("UnpackError = true; want false")
	}
	// At least the three files from single_rar5.rar must be present after extraction.
	for _, name := range []string{"file1.txt", "file2.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("extracted file %s not found: %v", name, err)
		}
	}
}

// TestUnpackStage_ExtractSingleRarGoMode_LabelsGoUnrarEngine verifies that
// OutputLines for a go_unrar extraction are labeled "[go_unrar]", not the
// generic "[unrar]" archive-type label, so the UI/log can distinguish which
// engine actually performed the extraction.
func TestUnpackStage_ExtractSingleRarGoMode_LabelsGoUnrarEngine(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	if err := enabledUnpackStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawGoUnrarLabel, sawBareUnrarLabel bool
	for _, line := range job.OutputLines {
		if strings.HasPrefix(line, "[go_unrar]") {
			sawGoUnrarLabel = true
		}
		if strings.HasPrefix(line, "[unrar]") {
			sawBareUnrarLabel = true
		}
	}
	if !sawGoUnrarLabel {
		t.Errorf("OutputLines missing [go_unrar] label: %v", job.OutputLines)
	}
	if sawBareUnrarLabel {
		t.Errorf("OutputLines contains generic [unrar] label for a go_unrar extraction: %v", job.OutputLines)
	}
}

// TestUnpackStage_CleanupDeletesArchiveParts verifies that when cleanup is
// enabled, the source RAR file is deleted after successful extraction.
func TestUnpackStage_CleanupDeletesArchiveParts(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	rarPath := copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	var output []string
	var mu sync.Mutex
	job.OnOutput = func(tool, line string) {
		mu.Lock()
		defer mu.Unlock()
		output = append(output, fmt.Sprintf("[%s] %s", tool, line))
	}

	s := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true /* cleanup */)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(rarPath); !os.IsNotExist(err) {
		t.Errorf("archive still exists after cleanup: want removed, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, line := range output {
		if strings.Contains(line, "[unpack] Deleted archive file: single_rar5.rar") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected deleted archive file log, got: %v", output)
	}

	foundSummary := slices.Contains(job.OutputLines, "Cleaned up 1 archive file(s)")
	if !foundSummary {
		t.Errorf("expected 'Cleaned up 1 archive file(s)' in job.OutputLines, got: %v", job.OutputLines)
	}
}

// TestUnpackStage_CorruptRarGoMode verifies that extracting a corrupt RAR
// sets job.UnpackError and returns an error.
func TestUnpackStage_CorruptRarGoMode(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("corrupt.rar"), dir)

	if err := enabledUnpackStage().Run(t.Context(), job); err == nil {
		t.Fatal("expected error for corrupt RAR, got nil")
	}
	if !job.UnpackError {
		t.Error("UnpackError = false; want true for corrupt RAR")
	}
}

// TestUnpackStage_DirectUnpackPrePopulated verifies that RAR parts from
// DirectUnpackSets are included in allSuccessful so cleanup can remove them.
func TestUnpackStage_DirectUnpackPrePopulated(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)

	// Create a fake RAR part on disk (content doesn't matter; cleanup just os.Remove it).
	fakePart := filepath.Join(dir, "movie.part01.rar")
	if err := os.WriteFile(fakePart, []byte("x"), 0o644); err != nil {
		t.Fatalf("write fake part: %v", err)
	}

	job.DirectUnpackSets = map[string]directunpack.SuccessSet{
		"movie": {
			RarParts:       []string{fakePart},
			ExtractedFiles: []string{filepath.Join(dir, "movie.mkv")},
		},
	}

	s := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true /* cleanup */)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Cleanup must have deleted the fake part that DirectUnpack recorded.
	if _, err := os.Stat(fakePart); !os.IsNotExist(err) {
		t.Errorf("DirectUnpack part %s still on disk after cleanup", fakePart)
	}
}

// TestUnpackStage_DisabledSkips verifies that a disabled stage returns nil
// immediately and sets no error flags on the job.
func TestUnpackStage_DisabledSkips(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	s := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, false)
	// deliberately NOT calling s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.UnpackError {
		t.Error("UnpackError set on disabled stage")
	}
	// The archive must still be present (stage did nothing).
	if _, err := os.Stat(filepath.Join(dir, "single_rar5.rar")); err != nil {
		t.Errorf("archive removed even though stage is disabled: %v", err)
	}
}

// ---------- applyPermissions helper ----------

// TestApplyPermissions_SetsFileModeOnFiles verifies that applyPermissions
// applies the given mode to files (execute bits stripped) and directories
// (full mode).
func TestApplyPermissions_SetsFileModeOnFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	count, err := applyPermissions(dir, "755")
	if err != nil {
		t.Fatalf("applyPermissions: %v", err)
	}
	if count == 0 {
		t.Error("count = 0; want > 0")
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	// Regular files must have execute bits stripped: 0755 → 0644.
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode = %04o; want 0644", got)
	}

	di, err := os.Stat(subDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o755 {
		t.Errorf("dir mode = %04o; want 0755", got)
	}
}

// TestApplyPermissions_BadPermString verifies that an invalid octal string
// returns an error.
func TestApplyPermissions_BadPermString(t *testing.T) {
	t.Parallel()

	_, err := applyPermissions(t.TempDir(), "not-octal")
	if err == nil {
		t.Error("expected error for bad perm string, got nil")
	}
}

// ---------- archiveTypePriority helper ----------

// TestArchiveTypePriority_SplitFirst verifies the sort invariant:
// SplitArchive < RarArchive < SevenZipArchive.
// SABnzbd requires file-join to run before RAR extraction in the same pass
// so that joined output can be immediately extracted.
func TestArchiveTypePriority_SplitFirst(t *testing.T) {
	t.Parallel()

	if archiveTypePriority(unpack.SplitArchive) >= archiveTypePriority(unpack.RarArchive) {
		t.Error("SplitArchive priority must be less than RarArchive")
	}
	if archiveTypePriority(unpack.RarArchive) >= archiveTypePriority(unpack.SevenZipArchive) {
		t.Error("RarArchive priority must be less than SevenZipArchive")
	}
}

// TestArchiveTypeName_KnownTypes verifies that each archive type maps to the
// correct tool name used in stage log output.
func TestArchiveTypeName_KnownTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		at   unpack.ArchiveType
		want string
	}{
		{unpack.RarArchive, "rar"},
		{unpack.SevenZipArchive, "7zip"},
		{unpack.SplitArchive, "filejoin"},
		{unpack.UnknownArchive, "unpack"},
	}
	for _, tc := range cases {
		if got := archiveTypeName(tc.at); got != tc.want {
			t.Errorf("archiveTypeName(%v) = %q, want %q", tc.at, got, tc.want)
		}
	}
}

func TestUnpackHelpers(t *testing.T) {
	t.Parallel()

	// Direct references for alignment check
	_ = extractRARArchive
	_ = extractSevenZipArchive
	_ = (*UnpackStage).extractPendingArchives

	// 1. Test prepareOptions
	t.Run("prepareOptions", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue: &queue.Job{
				Password: "jobpass",
			},
		}

		// Create a temporary password file
		tmpFile := filepath.Join(t.TempDir(), "passwords.txt")
		content := "pass1\npass2\n"
		if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		opts := u.prepareOptions(t.Context(), slog.Default(), job, unpack.Options{}, tmpFile)
		if len(opts.Passwords) != 3 {
			t.Errorf("len(Passwords) = %d; want 3", len(opts.Passwords))
		}
		if opts.Passwords[0] != "jobpass" {
			t.Errorf("Passwords[0] = %q; want jobpass", opts.Passwords[0])
		}

		// Non-existent password file
		_ = u.prepareOptions(t.Context(), slog.Default(), job, unpack.Options{}, "nonexistent-password-file.txt")
	})

	// 2. Test applyPermissions
	t.Run("applyPermissions", func(t *testing.T) {
		u := NewUnpackStage()
		dir := t.TempDir()
		// create a file inside dir
		fPath := filepath.Join(dir, "file.txt")
		if err := os.WriteFile(fPath, []byte("hello"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: dir,
		}
		u.applyPermissions(t.Context(), slog.Default(), job, "755", []unpack.Archive{{}})
	})

	// 3. Test extractArchive RAR scenarios
	t.Run("extractArchive RAR", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		a := unpack.Archive{Type: unpack.RarArchive, Name: "nonexistent.rar"}

		// Scenario A: Native Go, no fallback
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGoRAR: true, GoRarFallback: false}, true)

		// Scenario B: External only
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGoRAR: false, GoRarFallback: false}, true)

		// Scenario C: Native Go with external fallback
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGoRAR: true, GoRarFallback: true}, true)
	})

	// 4. Test extractArchive 7-Zip scenarios
	t.Run("extractArchive 7-Zip", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		a := unpack.Archive{Type: unpack.SevenZipArchive, Name: "nonexistent.7z"}

		// Scenario A: Native Go, no fallback
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGo7z: true, Go7zFallback: false}, true)

		// Scenario B: External only
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGo7z: false, Go7zFallback: false}, true)

		// Scenario C: Native Go with external fallback
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{UseGo7z: true, Go7zFallback: true}, true)
	})

	// 5. Test extractArchive (SplitArchive disabled)
	t.Run("extractArchive SplitArchive disabled", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		a := unpack.Archive{Type: unpack.SplitArchive, MainFile: "test.001"}
		_, _ = u.extractArchive(t.Context(), slog.Default(), job, a, unpack.Options{}, false)
	})

	// 6. Test extractPendingArchives (external tool success to cover CommandLine and Output)
	t.Run("extractPendingArchives external success", func(t *testing.T) {
		if _, err := exec.LookPath("unrar"); err != nil {
			t.Skip("unrar binary not found in PATH")
		}
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		copyToDir(t, unpackFixture("single_rar5.rar"), job.DownloadDir)

		processed := make(map[string]bool)
		var allSuccessful []unpack.Archive
		var firstErr error

		pending, err := unpack.Scan(job.DownloadDir)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}

		opts := unpack.Options{
			UseGoRAR: false,
		}

		ok := u.extractPendingArchives(t.Context(), slog.Default(), job, pending, processed, opts, true, &firstErr, &allSuccessful)
		if !ok {
			t.Error("expected extractPendingArchives to return true")
		}
		if firstErr != nil {
			t.Errorf("expected nil error, got %v", firstErr)
		}
		if len(allSuccessful) != 1 {
			t.Errorf("len(allSuccessful) = %d; want 1", len(allSuccessful))
		}
	})

	// 7. Test extractPendingArchives containment violation
	t.Run("extractPendingArchives containment violation", func(t *testing.T) {
		u := NewUnpackStage()
		job := &Job{
			Queue:       &queue.Job{ID: "testjob"},
			DownloadDir: t.TempDir(),
		}
		copyToDir(t, unpackFixture("single_rar5.rar"), job.DownloadDir)

		// Create a symlink pointing outside DownloadDir to trigger containment error
		target := filepath.Join(job.DownloadDir, "bad_symlink")
		if err := os.Symlink(t.TempDir(), target); err != nil {
			t.Skipf("symlinks not supported on this OS: %v", err)
		}

		processed := make(map[string]bool)
		var allSuccessful []unpack.Archive
		var firstErr error

		pending, err := unpack.Scan(job.DownloadDir)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}

		opts := unpack.Options{
			UseGoRAR: true, // Go-native is fast and sufficient
		}

		ok := u.extractPendingArchives(t.Context(), slog.Default(), job, pending, processed, opts, true, &firstErr, &allSuccessful)
		// Since containment check fails, firstErr should be non-nil
		if firstErr == nil {
			t.Error("expected containment violation error, got nil")
		}
		if !job.UnpackError {
			t.Error("expected job.UnpackError to be true")
		}
		// Since containment check fails, the archive is not successfully finished, so ok should be false
		if ok {
			t.Error("expected ok=false since containment check failed")
		}
	})
}

func TestUnpackStage_RealtimeLogTransitions(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		loggedLines = append(loggedLines, line)
	}

	if err := enabledUnpackStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawStart, sawComplete bool
	for _, l := range loggedLines {
		if strings.Contains(l, "Using go_unrar for RAR (pure-Go)") {
			sawStart = true
		}
		if strings.Contains(l, "go_unrar: unpacking complete") {
			sawComplete = true
		}
	}

	if !sawStart {
		t.Errorf("expected start log 'Using go_unrar for RAR (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawComplete {
		t.Errorf("expected complete log 'go_unrar: unpacking complete' in OnOutput, got: %v", loggedLines)
	}
}

func sevenZipFixture(name string) string {
	return filepath.Join("..", "unpack", "testdata", "sevenzip", name)
}

func TestUnpackStage_RealtimeLogTransitions_SevenZip(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyToDir(t, sevenZipFixture("lzma2.7z"), dir)

	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		loggedLines = append(loggedLines, line)
	}

	s := NewUnpackStageWith(unpack.Options{UseGo7z: true}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawStart, sawComplete bool
	for _, l := range loggedLines {
		if strings.Contains(l, "Using go_7z for 7-Zip (pure-Go)") {
			sawStart = true
		}
		if strings.Contains(l, "go_7z: unpacking complete") {
			sawComplete = true
		}
	}

	if !sawStart {
		t.Errorf("expected start log 'Using go_7z for 7-Zip (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawComplete {
		t.Errorf("expected complete log 'go_7z: unpacking complete' in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_RealtimeLogTransitions_SevenZip_Fallback(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	// Copy a RAR archive but name it as .7z to force GoSevenZip to fail
	// and trigger the fallback to external 7z.
	rarPath := copyToDir(t, unpackFixture("single_rar5.rar"), dir)
	fake7zPath := filepath.Join(dir, "single_rar5.7z")
	if err := os.Rename(rarPath, fake7zPath); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
	}

	// We must configure Go7zFallback: true, UseGo7z: true, and ensure SevenZipCommand is set/detectable
	s := NewUnpackStageWith(unpack.Options{
		UseGo7z:         true,
		Go7zFallback:    true,
		SevenZipCommand: "7z",
	}, false)
	s.SetEnabled(true)

	// We expect this to fail eventually because it's not a real 7z archive.
	_ = s.Run(t.Context(), job)

	var sawGo7zStart, sawGo7zFailRetry, sawExternal7zCommand bool
	for _, l := range loggedLines {
		if strings.Contains(l, "[go_7z] Using go_7z for 7-Zip (pure-Go)") {
			sawGo7zStart = true
		}
		if strings.Contains(l, "[go_7z] Go-native extraction failed:") && strings.Contains(l, "retrying with 7z") {
			sawGo7zFailRetry = true
		}
		if strings.Contains(l, "[7z] Running command:") {
			sawExternal7zCommand = true
		}
	}

	if !sawGo7zStart {
		t.Errorf("expected start log 'Using go_7z for 7-Zip (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawGo7zFailRetry {
		t.Errorf("expected fallback retry log in OnOutput, got: %v", loggedLines)
	}
	if !sawExternal7zCommand {
		t.Errorf("expected external command log in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_RealtimeLogTransitions_Rar_Fallback(t *testing.T) {
	t.Parallel()

	// Check if unrar is available on this system first.
	if _, lookErr := exec.LookPath("unrar"); lookErr != nil {
		t.Skip("unrar binary not found, skipping fallback test")
	}

	job, dir := stageJob(t)
	// Use a corrupt rar to trigger GoUnRAR failure and fallback to unrar
	copyToDir(t, unpackFixture("corrupt.rar"), dir)

	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGoRAR:      true,
		GoRarFallback: true,
		UnrarCommand:  "unrar",
	}, false)
	s.SetEnabled(true)

	_ = s.Run(t.Context(), job)

	var sawGoRarStart, sawGoRarFailRetry, sawExternalUnrarCommand bool
	for _, l := range loggedLines {
		if strings.Contains(l, "[go_unrar] Using go_unrar for RAR (pure-Go)") {
			sawGoRarStart = true
		}
		if strings.Contains(l, "[go_unrar] Go-native extraction failed:") && strings.Contains(l, "retrying with unrar") {
			sawGoRarFailRetry = true
		}
		if strings.Contains(l, "[unrar] Running command:") {
			sawExternalUnrarCommand = true
		}
	}

	if !sawGoRarStart {
		t.Errorf("expected start log 'Using go_unrar for RAR (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawGoRarFailRetry {
		t.Errorf("expected fallback retry log in OnOutput, got: %v", loggedLines)
	}
	if !sawExternalUnrarCommand {
		t.Errorf("expected external command log in OnOutput, got: %v", loggedLines)
	}
}

func createDummyExecutable(t *testing.T, dir, filename, content string) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write dummy executable: %v", err)
	}
	return path
}

func TestUnpackStage_RealtimeLogTransitions_Rar_Fallback_Success(t *testing.T) {
	t.Parallel()

	tmpBinDir := t.TempDir()
	unrarPath := filepath.Join(tmpBinDir, "mock-unrar")

	successScript := `#!/bin/sh
echo "unrar stdout line 1"
echo "unrar stdout line 2"
outdir=""
for arg in "$@"; do
  case "$arg" in
    -o*) outdir="${arg#-o}" ;;
    *) outdir="$arg" ;;
  esac
done
outdir="${outdir%/}"
if [ -n "$outdir" ] && [ -d "$outdir" ]; then
  touch "$outdir/mock_extracted_file.txt"
fi
exit 0
`
	if err := os.WriteFile(unrarPath, []byte(successScript), 0o755); err != nil {
		t.Fatalf("WriteFile success script: %v", err)
	}

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("corrupt.rar"), dir)

	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGoRAR:      true,
		GoRarFallback: true,
		UnrarCommand:  unrarPath,
	}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}

	if job.UnpackError {
		t.Error("expected job.UnpackError to be false since fallback succeeded")
	}

	var sawGoRarStart, sawGoRarFailRetry, sawExternalUnrarCommand, sawStdout, sawSuccess bool
	for _, l := range loggedLines {
		if strings.Contains(l, "[go_unrar] Using go_unrar for RAR (pure-Go)") {
			sawGoRarStart = true
		}
		if strings.Contains(l, "[go_unrar] Go-native extraction failed:") && strings.Contains(l, "retrying with") {
			sawGoRarFailRetry = true
		}
		if strings.Contains(l, "[unrar] Running command:") {
			sawExternalUnrarCommand = true
		}
		if strings.Contains(l, "[unrar] unrar stdout line 1") {
			sawStdout = true
		}
		if strings.Contains(l, "[unrar] Unpacking complete:") {
			sawSuccess = true
		}
	}

	if !sawGoRarStart {
		t.Errorf("missing go_unrar start log: %v", loggedLines)
	}
	if !sawGoRarFailRetry {
		t.Errorf("missing fallback retry log: %v", loggedLines)
	}
	if !sawExternalUnrarCommand {
		t.Errorf("missing external command log: %v", loggedLines)
	}
	if !sawStdout {
		t.Errorf("missing stdout line: %v", loggedLines)
	}
	if !sawSuccess {
		t.Errorf("missing success log: %v", loggedLines)
	}
}

func TestUnpackStage_RealtimeLogTransitions_SevenZip_Fallback_Success(t *testing.T) {
	t.Parallel()

	tmpBinDir := t.TempDir()
	dummyScript := `#!/bin/sh
echo "7z stdout line 1"
echo "7z stdout line 2"
outdir=""
for arg in "$@"; do
  case "$arg" in
    -o*) outdir="${arg#-o}" ;;
    *) outdir="$arg" ;;
  esac
done
outdir="${outdir%/}"
if [ -n "$outdir" ] && [ -d "$outdir" ]; then
  touch "$outdir/mock_extracted_file.txt"
fi
exit 0
`
	szPath := createDummyExecutable(t, tmpBinDir, "mock-7z", dummyScript)

	job, dir := stageJob(t)
	// Copy a RAR archive but name it as .7z to force GoSevenZip to fail
	rarPath := copyToDir(t, unpackFixture("single_rar5.rar"), dir)
	fake7zPath := filepath.Join(dir, "single_rar5.7z")
	if err := os.Rename(rarPath, fake7zPath); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGo7z:         true,
		Go7zFallback:    true,
		SevenZipCommand: szPath,
	}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}

	if job.UnpackError {
		t.Error("expected job.UnpackError to be false since fallback succeeded")
	}

	var sawGo7zStart, sawGo7zFailRetry, sawExternal7zCommand, sawStdout, sawSuccess bool
	for _, l := range loggedLines {
		if strings.Contains(l, "[go_7z] Using go_7z for 7-Zip (pure-Go)") {
			sawGo7zStart = true
		}
		if strings.Contains(l, "[go_7z] Go-native extraction failed:") && strings.Contains(l, "retrying with") {
			sawGo7zFailRetry = true
		}
		if strings.Contains(l, "[7z] Running command:") {
			sawExternal7zCommand = true
		}
		if strings.Contains(l, "[7z] 7z stdout line 1") {
			sawStdout = true
		}
		if strings.Contains(l, "[7z] Unpacking complete:") {
			sawSuccess = true
		}
	}

	if !sawGo7zStart {
		t.Errorf("expected start log 'Using go_7z for 7-Zip (pure-Go)' in OnOutput, got: %v", loggedLines)
	}
	if !sawGo7zFailRetry {
		t.Errorf("expected fallback retry log in OnOutput, got: %v", loggedLines)
	}
	if !sawExternal7zCommand {
		t.Errorf("expected external command log in OnOutput, got: %v", loggedLines)
	}
	if !sawStdout {
		t.Errorf("expected stdout line in OnOutput, got: %v", loggedLines)
	}
	if !sawSuccess {
		t.Errorf("expected success log in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_RealtimeLogTransitions_SevenZip_Direct_Success(t *testing.T) {
	t.Parallel()

	tmpBinDir := t.TempDir()
	dummyScript := `#!/bin/sh
echo "7z stdout line 1"
exit 0
`
	szPath := createDummyExecutable(t, tmpBinDir, "mock-7z", dummyScript)

	job, dir := stageJob(t)
	copyToDir(t, sevenZipFixture("lzma2.7z"), dir)

	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGo7z:         false,
		SevenZipCommand: szPath,
	}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("expected extraction to succeed, got error: %v", err)
	}

	var sawStart, sawComplete bool
	for _, l := range loggedLines {
		if strings.Contains(l, "[7z] Unpacking:") {
			sawStart = true
		}
		if strings.Contains(l, "[7z] Unpacking complete:") {
			sawComplete = true
		}
	}

	if !sawStart {
		t.Errorf("expected start log 'Unpacking:' in OnOutput, got: %v", loggedLines)
	}
	if !sawComplete {
		t.Errorf("expected complete log 'Unpacking complete:' in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_RealtimeLogTransitions_Rar_Direct_Success(t *testing.T) {
	t.Parallel()

	tmpBinDir := t.TempDir()
	dummyScript := `#!/bin/sh
echo "unrar stdout line 1"
exit 0
`
	unrarPath := createDummyExecutable(t, tmpBinDir, "mock-unrar", dummyScript)

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	var loggedLines []string
	job.OnOutput = func(tool, line string) {
		loggedLines = append(loggedLines, fmt.Sprintf("[%s] %s", tool, line))
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGoRAR:     false,
		UnrarCommand: unrarPath,
	}, false)
	s.SetEnabled(true)

	if err := s.Run(t.Context(), job); err != nil {
		t.Fatalf("expected extraction to succeed, got error: %v", err)
	}

	var sawStart, sawComplete bool
	for _, l := range loggedLines {
		if strings.Contains(l, "[unrar] Unpacking:") {
			sawStart = true
		}
		if strings.Contains(l, "[unrar] Unpacking complete:") {
			sawComplete = true
		}
	}

	if !sawStart {
		t.Errorf("expected start log 'Unpacking:' in OnOutput, got: %v", loggedLines)
	}
	if !sawComplete {
		t.Errorf("expected complete log 'Unpacking complete:' in OnOutput, got: %v", loggedLines)
	}
}

func TestUnpackStage_ContainmentViolation_ImmediateCleanup(t *testing.T) {
	t.Parallel()

	if _, lookErr := exec.LookPath("sh"); lookErr != nil {
		t.Skip("sh not found, skipping script-based containment cleanup test")
	}

	job, dir := stageJob(t)
	copyToDir(t, unpackFixture("single_rar5.rar"), dir)

	outsideDir := t.TempDir()
	escapedFile := filepath.Join(outsideDir, "secret_outside.txt")
	if err := os.WriteFile(escapedFile, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write escaped file: %v", err)
	}

	scriptPath := filepath.Join(t.TempDir(), "fake_unrar.sh")
	scriptContent := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  outDir="$arg"
done
outDir=$(echo "$outDir" | sed 's#/$##')
ln -s "%s" "$outDir/evil_link"
touch "$outDir/normal_file.txt"
exit 0
`, escapedFile)
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("write fake unrar script: %v", err)
	}

	s := NewUnpackStageWith(unpack.Options{
		UseGoRAR:     false,
		UnrarCommand: scriptPath,
	}, true)
	s.SetEnabled(true)

	_ = s.Run(t.Context(), job)

	if !job.UnpackError {
		t.Error("expected job.UnpackError = true when containment check fails")
	}

	if _, err := os.Stat(filepath.Join(dir, "evil_link")); !os.IsNotExist(err) {
		t.Errorf("evil_link inside DownloadDir should be deleted immediately, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "normal_file.txt")); !os.IsNotExist(err) {
		t.Errorf("normal_file.txt inside DownloadDir should be deleted immediately, stat err: %v", err)
	}
	if _, err := os.Stat(escapedFile); !os.IsNotExist(err) {
		t.Errorf("escaped target file %s outside DownloadDir should be deleted immediately by target removal, stat err: %v", escapedFile, err)
	}
}

func TestCleanupContainmentViolation_Direct(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	symlinkPath := filepath.Join(outDir, "link.txt")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	normalFile := filepath.Join(outDir, "normal.txt")
	if err := os.WriteFile(normalFile, []byte("normal content"), 0o644); err != nil {
		t.Fatalf("write normal file: %v", err)
	}

	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, nil)
	log := slog.New(handler)

	// We pass:
	// 1. "link.txt" (relative path to symlink pointing out of bounds)
	// 2. normalFile (absolute path to normal file inside outDir)
	// 3. "nonexistent.txt" (to cover error path when file doesn't exist)
	cleanupContainmentViolation(outDir, []string{"link.txt", normalFile, "nonexistent.txt"}, log)

	if _, err := os.Stat(outsideFile); !os.IsNotExist(err) {
		t.Errorf("expected out-of-bounds target %s to be deleted, stat err: %v", outsideFile, err)
	}
	if _, err := os.Stat(symlinkPath); !os.IsNotExist(err) {
		t.Errorf("expected symlink %s to be deleted, stat err: %v", symlinkPath, err)
	}
	if _, err := os.Stat(normalFile); !os.IsNotExist(err) {
		t.Errorf("expected normal file %s to be deleted, stat err: %v", normalFile, err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "removed out-of-bounds target file") {
		t.Errorf("expected log to contain 'removed out-of-bounds target file', got: %s", logs)
	}
	if !strings.Contains(logs, "removed extracted file after containment check failure") {
		t.Errorf("expected log to contain 'removed extracted file after containment check failure', got: %s", logs)
	}

	// Also test with nil logger to ensure no panic on logging branches
	if err := os.WriteFile(normalFile, []byte("again"), 0o644); err != nil {
		t.Fatalf("write normal file again: %v", err)
	}
	cleanupContainmentViolation(outDir, []string{"normal.txt"}, nil)
	if _, err := os.Stat(normalFile); !os.IsNotExist(err) {
		t.Errorf("expected normal file to be deleted with nil logger, stat err: %v", err)
	}
}
