package postproc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
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

	job := &Job{
		Queue:       &queue.Job{ID: "fr-ok", Name: "MyRelease"},
		DownloadDir: srcDir,
		FinalDir:    finalDir,
	}

	stage := NewFinalizeStage()
	stage.FolderRename = true

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
	srcDir := t.TempDir()
	os.WriteFile(filepath.Join(srcDir, "movie.mkv"), []byte("video"), 0o644)

	baseDir := t.TempDir()
	finalDir := filepath.Join(baseDir, "MyRelease")

	job := &Job{
		Queue:       &queue.Job{ID: "fr-fail", Name: "MyRelease"},
		DownloadDir: srcDir,
		FinalDir:    finalDir,
		ParError:    true,
	}

	stage := NewFinalizeStage()
	stage.FolderRename = true

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Should be renamed to _FAILED_ directory.
	failedDir := filepath.Join(baseDir, "_FAILED_MyRelease")
	if _, err := os.Stat(failedDir); err != nil {
		t.Errorf("_FAILED_ dir should exist: %v", err)
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

func TestFinalizeStage_FolderRename_Disabled(t *testing.T) {
	// When FolderRename=false, no prefix is used even on failure.
	t.Parallel()
	job, srcDir := stageJob(t)
	job.ParError = true
	baseDir := t.TempDir()
	job.FinalDir = filepath.Join(baseDir, "MyRelease")

	stage := NewFinalizeStage()
	stage.FolderRename = false

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

// ---------- SortStage edge cases ----------

func TestSortStage_EmptyDownloadDir(t *testing.T) {
	t.Parallel()
	job := &Job{
		Queue: &queue.Job{
			ID:   "sort-empty",
			Name: "sort-empty",
		},
		DownloadDir: t.TempDir(),
	}
	stage := NewSortStage(nil, t.TempDir())
	if err := stage.Run(t.Context(), job); err != nil {
		t.Errorf("Sort with empty dir: %v", err)
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

// ---------- SortStage skip on failure ----------

func TestSortStage_SkipsOnParError(t *testing.T) {
	t.Parallel()
	job := &Job{
		Queue: &queue.Job{
			ID:   "sort-par-err",
			Name: "sort-par-err",
		},
		DownloadDir: t.TempDir(),
		ParError:    true,
	}
	stage := NewSortStage(nil, t.TempDir())
	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Errorf("expected nil for ParError job, got %v", err)
	}
	if len(job.OutputLines) == 0 {
		t.Error("expected OutputLines to contain skip message")
	}
}

func TestSortStage_SkipsOnUnpackError(t *testing.T) {
	t.Parallel()
	job := &Job{
		Queue: &queue.Job{
			ID:   "sort-unpack-err",
			Name: "sort-unpack-err",
		},
		DownloadDir: t.TempDir(),
		UnpackError: true,
	}
	stage := NewSortStage(nil, t.TempDir())
	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Errorf("expected nil for UnpackError job, got %v", err)
	}
	if len(job.OutputLines) == 0 {
		t.Error("expected OutputLines to contain skip message")
	}
}
