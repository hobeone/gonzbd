package postproc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// ---------- FinalizeStage edge cases ----------

func TestFinalizeStage_SkipsOnParError(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	job.ParError = true
	job.FinalDir = t.TempDir()

	err := NewFinalizeStage().Run(t.Context(), job)
	if err != nil {
		t.Errorf("expected nil for ParError job, got %v", err)
	}
}

func TestFinalizeStage_SkipsOnUnpackError(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	job.UnpackError = true
	job.FinalDir = t.TempDir()

	err := NewFinalizeStage().Run(t.Context(), job)
	if err != nil {
		t.Errorf("expected nil for UnpackError job, got %v", err)
	}
}

func TestFinalizeStage_SkipsOnFailMsg(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	job.FailMsg = "something went wrong"
	job.FinalDir = t.TempDir()

	err := NewFinalizeStage().Run(t.Context(), job)
	if err != nil {
		t.Errorf("expected nil for FailMsg job, got %v", err)
	}
}

func TestFinalizeStage_ErrorOnEmptyFinalDir(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	job.FinalDir = ""

	err := NewFinalizeStage().Run(t.Context(), job)
	if err == nil {
		t.Error("expected error when FinalDir is empty")
	}
}

func TestFinalizeStage_SameDir(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)
	job.FinalDir = dir

	err := NewFinalizeStage().Run(t.Context(), job)
	if err != nil {
		t.Errorf("expected nil when DownloadDir == FinalDir, got %v", err)
	}
}

func TestFinalizeStage_MoveNestedDirectories(t *testing.T) {
	t.Parallel()
	job, srcDir := stageJob(t)
	finalDir := filepath.Join(t.TempDir(), "final")
	job.FinalDir = finalDir

	// Create nested structure.
	subdir := filepath.Join(srcDir, "sub")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, f := range []struct{ dir, name, content string }{
		{srcDir, "top.txt", "top-content"},
		{subdir, "nested.txt", "nested-content"},
	} {
		if err := os.WriteFile(filepath.Join(f.dir, f.name), []byte(f.content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if err := NewFinalizeStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify files at destination.
	got, err := os.ReadFile(filepath.Join(finalDir, "top.txt"))
	if err != nil {
		t.Fatalf("read top.txt: %v", err)
	}
	if string(got) != "top-content" {
		t.Errorf("top.txt = %q, want %q", got, "top-content")
	}

	got, err = os.ReadFile(filepath.Join(finalDir, "sub", "nested.txt"))
	if err != nil {
		t.Fatalf("read nested.txt: %v", err)
	}
	if string(got) != "nested-content" {
		t.Errorf("nested.txt = %q, want %q", got, "nested-content")
	}

	// Source should be removed.
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Errorf("source dir should be removed")
	}
}

func TestFinalizeStage_FolderRename_Success(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	os.WriteFile(filepath.Join(srcDir, "movie.mkv"), []byte("video"), 0o644)

	baseDir := t.TempDir()
	finalDir := filepath.Join(baseDir, "MyRelease")

	qjob := newQueueJob(t, "fr-ok", 0)
	qjob.SetName("MyRelease")
	job := &Job{
		Job:         qjob,
		DownloadDir: srcDir,
		FinalDir:    finalDir,
	}

	stage := NewFinalizeStage()
	stage.SetFolderRename(true)

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Final dir should exist (prefix stripped).
	if _, err := os.Stat(finalDir); err != nil {
		t.Errorf("FinalDir should exist: %v", err)
	}
	// _UNPACK_ dir should NOT exist.
	unpackDir := filepath.Join(baseDir, "_UNPACK_MyRelease")
	if _, err := os.Stat(unpackDir); !os.IsNotExist(err) {
		t.Errorf("_UNPACK_ dir should not persist")
	}
	// DownloadDir should point to final location.
	if job.DownloadDir != finalDir {
		t.Errorf("DownloadDir = %q, want %q", job.DownloadDir, finalDir)
	}
	// File should be there.
	if _, err := os.Stat(filepath.Join(finalDir, "movie.mkv")); err != nil {
		t.Errorf("movie.mkv should exist in finalDir")
	}
}

func TestFinalizeStage_FolderRename_Failed(t *testing.T) {
	t.Parallel()
	// Create a download dir with a known parent so we can verify the
	// _FAILED_ prefix is applied in-place (not moved to complete).
	parentDir := t.TempDir()
	srcDir := filepath.Join(parentDir, "MyRelease")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "movie.mkv"), []byte("video"), 0o644)

	completeDir := t.TempDir()
	finalDir := filepath.Join(completeDir, "MyRelease")

	qjob := newQueueJob(t, "fr-fail", 0)
	qjob.SetName("MyRelease")
	job := &Job{
		Job:         qjob,
		DownloadDir: srcDir,
		FinalDir:    finalDir,
		ParError:    true,
	}

	stage := NewFinalizeStage()
	stage.SetFolderRename(true)

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// _FAILED_ dir should be in the download area parent (in-place rename),
	// NOT in the complete directory.
	failedDir := filepath.Join(parentDir, "_FAILED_MyRelease")
	if _, err := os.Stat(failedDir); err != nil {
		t.Errorf("_FAILED_ dir should exist in download parent: %v", err)
	}
	// Complete directory should NOT have _FAILED_ dir.
	completeFailedDir := filepath.Join(completeDir, "_FAILED_MyRelease")
	if _, err := os.Stat(completeFailedDir); !os.IsNotExist(err) {
		t.Errorf("_FAILED_ dir should NOT be in complete directory")
	}
	// DownloadDir should point to _FAILED_ dir.
	if job.DownloadDir != failedDir {
		t.Errorf("DownloadDir = %q, want %q", job.DownloadDir, failedDir)
	}
	// File should be in _FAILED_ dir.
	if _, err := os.Stat(filepath.Join(failedDir, "movie.mkv")); err != nil {
		t.Errorf("movie.mkv should exist in _FAILED_ dir")
	}
}

// TestFinalizeStage_FailedFilesAccessibleForRetry validates the retry invariant:
// after a failed job (with or without FolderRename), the downloaded files MUST
// remain accessible at job.DownloadDir, and that path must NOT be under the
// complete directory. This ensures the retry system can find the files.
//
// This is an integration-level contract test. The original _FAILED_ prefix
// implementation moved files to the complete directory, breaking retry.
func TestFinalizeStage_FailedFilesAccessibleForRetry(t *testing.T) {
	t.Parallel()

	incompleteDir := t.TempDir() // simulates the incomplete/download area
	completeDir := t.TempDir()   // simulates the complete area

	for _, folderRename := range []bool{true, false} {
		for _, errType := range []string{"par", "unpack", "failmsg"} {
			name := errType
			if folderRename {
				name += "_rename"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				// Create a download directory with files, as the downloader would.
				srcDir := filepath.Join(incompleteDir, name)
				os.MkdirAll(srcDir, 0o755)
				os.WriteFile(filepath.Join(srcDir, "data.rar"), []byte("archive"), 0o644)
				os.WriteFile(filepath.Join(srcDir, "data.par2"), []byte("parity"), 0o644)

				qjob := newQueueJob(t, "retry-"+name, 0)
				qjob.SetName("TestRelease")
				job := &Job{
					Job:         qjob,
					DownloadDir: srcDir,
					FinalDir:    filepath.Join(completeDir, "TestRelease"),
				}

				switch errType {
				case "par":
					job.ParError = true
				case "unpack":
					job.UnpackError = true
				case "failmsg":
					job.FailMsg = "beyond repair"
				}

				stage := NewFinalizeStage()
				stage.SetFolderRename(folderRename)

				if err := stage.Run(t.Context(), job); err != nil {
					t.Fatalf("Run: %v", err)
				}

				// INVARIANT 1: job.DownloadDir must exist and contain our files.
				if _, err := os.Stat(job.DownloadDir); err != nil {
					t.Fatalf("DownloadDir %q does not exist after failure: %v", job.DownloadDir, err)
				}
				for _, f := range []string{"data.rar", "data.par2"} {
					if _, err := os.Stat(filepath.Join(job.DownloadDir, f)); err != nil {
						t.Errorf("file %q not found at DownloadDir after failure: %v", f, err)
					}
				}

				// INVARIANT 2: files must NOT be in the complete directory.
				entries, _ := os.ReadDir(completeDir)
				for _, e := range entries {
					t.Errorf("complete directory should be empty after failure, but found: %s", e.Name())
				}

				// INVARIANT 3: DownloadDir must still be under the incomplete area.
				if !strings.HasPrefix(job.DownloadDir, incompleteDir) {
					t.Errorf("DownloadDir %q escaped incomplete area %q", job.DownloadDir, incompleteDir)
				}
			})
		}
	}
}

func TestFinalizeStage_FolderRename_Disabled(t *testing.T) {
	// When FolderRename=false, no prefix is used even on failure.
	t.Parallel()
	job, srcDir := stageJob(t)
	job.ParError = true
	baseDir := t.TempDir()
	job.FinalDir = filepath.Join(baseDir, "MyRelease")

	stage := NewFinalizeStage()
	stage.SetFolderRename(false)

	stage.Run(t.Context(), job)

	// DownloadDir should still be the original (no move on failure).
	if job.DownloadDir != srcDir {
		t.Errorf("DownloadDir = %q, want %q", job.DownloadDir, srcDir)
	}
}

func TestPrefixDirName(t *testing.T) {
	tests := []struct {
		dir, prefix, want string
	}{
		{"/complete/movies/MyRelease", "_UNPACK_", "/complete/movies/_UNPACK_MyRelease"},
		{"/complete/MyRelease", "_FAILED_", "/complete/_FAILED_MyRelease"},
		{"release", "_UNPACK_", "_UNPACK_release"},
	}
	for _, tt := range tests {
		got := prefixDirName(tt.dir, tt.prefix)
		if got != tt.want {
			t.Errorf("prefixDirName(%q, %q) = %q, want %q", tt.dir, tt.prefix, got, tt.want)
		}
	}
}

// ---------- moveRecursive ----------

func TestMoveRecursive_SingleFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("hello"), 0o644)

	if err := moveRecursive(t.Context(), src, dst); err != nil {
		t.Fatalf("moveRecursive: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be removed")
	}
}

func TestMoveRecursive_Directory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "from")
	dstDir := filepath.Join(dir, "to")
	os.MkdirAll(filepath.Join(srcDir, "inner"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "inner", "b.txt"), []byte("B"), 0o644)

	if err := moveRecursive(t.Context(), srcDir, dstDir); err != nil {
		t.Fatalf("moveRecursive: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dstDir, "a.txt"))
	if string(got) != "A" {
		t.Errorf("a.txt = %q", got)
	}
	got, _ = os.ReadFile(filepath.Join(dstDir, "inner", "b.txt"))
	if string(got) != "B" {
		t.Errorf("inner/b.txt = %q", got)
	}
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Error("source directory should be removed")
	}
}

func TestMoveRecursive_ContextCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("data"), 0o644)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := moveRecursive(ctx, src, dst)
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestMoveRecursive_SourceNotExist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := moveRecursive(t.Context(), filepath.Join(dir, "nope"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Error("expected error for nonexistent source")
	}
}

// ---------- DeobfuscateStage with obfuscated file ----------

func TestDeobfuscateStage_WithFiles(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)
	// Create a clearly obfuscated filename.
	os.WriteFile(filepath.Join(dir, "abc123def456.mkv"), []byte("video"), 0o644)

	if err := NewDeobfuscateStage().Run(t.Context(), job); err != nil {
		t.Errorf("Deobfuscate: %v", err)
	}
}

// ---------- UnpackStage skip on ParError ----------

func TestUnpackStage_SkipsOnParError(t *testing.T) {
	t.Parallel()
	job, _ := stageJob(t)
	job.ParError = true

	err := NewUnpackStage().Run(t.Context(), job)
	if err != nil {
		t.Errorf("expected nil for ParError job, got %v", err)
	}
	if len(job.OutputLines) == 0 {
		t.Error("expected OutputLines to contain skip message")
	}
	if job.UnpackError {
		t.Error("UnpackError should not be set when skipped")
	}
}

// ---------- CleanupStage tests ----------

func TestCleanupStage_Name(t *testing.T) {
	t.Parallel()
	if got := (&CleanupStage{}).Name(); got != "cleanup" {
		t.Errorf("Name() = %q, want %q", got, "cleanup")
	}
}

func TestCleanupStage_RemovesAdminDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adminDir := filepath.Join(dir, "__ADMIN__")
	os.MkdirAll(adminDir, 0o755)
	os.WriteFile(filepath.Join(adminDir, "gonzbd_verified.json"), []byte(`{}`), 0o644)

	job := &Job{
		Job:         newQueueJob(t, "test-cleanup", 0),
		DownloadDir: dir,
	}
	stage := NewCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	if _, err := os.Stat(adminDir); !os.IsNotExist(err) {
		t.Error("expected admin dir to be removed after successful cleanup")
	}
}

func TestCleanupStage_PreservesOnParError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adminDir := filepath.Join(dir, "__ADMIN__")
	os.MkdirAll(adminDir, 0o755)
	os.WriteFile(filepath.Join(adminDir, "gonzbd_verified.json"), []byte(`{}`), 0o644)

	job := &Job{
		Job:         newQueueJob(t, "test-cleanup-par", 0),
		DownloadDir: dir,
		ParError:    true,
	}
	stage := NewCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	if _, err := os.Stat(adminDir); os.IsNotExist(err) {
		t.Error("expected admin dir to be preserved on ParError")
	}
}

func TestCleanupStage_PreservesOnUnpackError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adminDir := filepath.Join(dir, "__ADMIN__")
	os.MkdirAll(adminDir, 0o755)

	job := &Job{
		Job:         newQueueJob(t, "test-cleanup-unpack", 0),
		DownloadDir: dir,
		UnpackError: true,
	}
	stage := NewCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	if _, err := os.Stat(adminDir); os.IsNotExist(err) {
		t.Error("expected admin dir to be preserved on UnpackError")
	}
}

func TestCleanupStage_NoAdminDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// No __ADMIN__ dir exists — should succeed gracefully.
	job := &Job{
		Job:         newQueueJob(t, "test-cleanup-none", 0),
		DownloadDir: dir,
	}
	stage := NewCleanupStage()
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run() = %v, expected nil", err)
	}
}

func TestDeobfuscateStage_FullFlow(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)
	// 11 MiB obfuscated file (above 10 MiB threshold).
	mkvFile := filepath.Join(dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv")
	data := make([]byte, 11*1024*1024)
	if err := os.WriteFile(mkvFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Add english.srt subtitle (size 100).
	srtFile := filepath.Join(dir, "english.srt")
	if err := os.WriteFile(srtFile, []byte("subtitles content"), 0o644); err != nil {
		t.Fatal(err)
	}

	stage := NewDeobfuscateStage()
	if err := stage.Run(t.Context(), job); err != nil {
		t.Errorf("Deobfuscate: %v", err)
	}

	// Verify deobfuscation happened.
	expectedMkv := filepath.Join(dir, "test.job.mkv")
	if _, err := os.Stat(expectedMkv); err != nil {
		t.Errorf("expected renamed mkv file not found: %v", err)
	}
	// Verify subtitle renaming happened.
	expectedSrt := filepath.Join(dir, "test.job.english.srt")
	if _, err := os.Stat(expectedSrt); err != nil {
		t.Errorf("expected renamed srt file not found: %v", err)
	}

	// Verify OutputLines logs.
	hasDeobfLog := false
	hasSubLog := false
	for _, line := range job.OutputLines {
		if strings.Contains(line, "Deobfuscated 1 file(s)") {
			hasDeobfLog = true
		}
		if strings.Contains(line, "Renamed 1 subtitle file(s)") {
			hasSubLog = true
		}
	}
	if !hasDeobfLog {
		t.Errorf("expected 'Deobfuscated 1 file(s)' in output, got: %v", job.OutputLines)
	}
	if !hasSubLog {
		t.Errorf("expected 'Renamed 1 subtitle file(s)' in output, got: %v", job.OutputLines)
	}
}

// Must stay non-parallel: SetRemoveBackoffsForTest mutates fsutil's shared
// removeBackoffs package var, which is safe only while this test runs
// serially (see the same note on TestSetRemoveBackoffsForTest).
func TestCleanupStage_LogOnFailure(t *testing.T) {
	restore := fsutil.SetRemoveBackoffsForTest(nil)
	defer restore()

	dir := t.TempDir()
	adminDir := filepath.Join(dir, "__ADMIN__")
	if err := os.MkdirAll(adminDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a file inside __ADMIN__.
	if err := os.WriteFile(filepath.Join(adminDir, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	job := &Job{
		Job:         newQueueJob(t, "test-cleanup-fail", 0),
		DownloadDir: dir,
	}

	// Make __ADMIN__ directory non-writable/non-executable.
	if err := os.Chmod(adminDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chmod(adminDir, 0o755)
	}()

	handler := &testLogHandler{}
	stage := &CleanupStage{Log: slog.New(handler)}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	// Verify that warnings were logged because RemoveAll failed and directory still exists.
	handler.mu.Lock()
	warnLogged := handler.warnLogged
	handler.mu.Unlock()

	if !warnLogged {
		t.Error("expected warnings to be logged on cleanup failure")
	}
}

func TestJob_NilDelegation(t *testing.T) {
	t.Parallel()
	j := &Job{}
	if got := j.Name(); got != "" {
		t.Fatalf("Name() = %q, want empty", got)
	}
	if _, err := j.Manifest(); err == nil {
		t.Fatal("Manifest() should error when Job is nil")
	}
	if got := j.Progress(); got != nil {
		t.Fatal("Progress() should be nil when Job is nil")
	}
	if got := j.NumFiles(); got != 0 {
		t.Fatalf("NumFiles() = %d, want 0", got)
	}
}
