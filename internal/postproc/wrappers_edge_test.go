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

// ---------- moveRecursive ----------

func TestMoveRecursive_SingleFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("hello"), 0o644)

	if err := moveRecursive(context.Background(), src, dst); err != nil {
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

	if err := moveRecursive(context.Background(), srcDir, dstDir); err != nil {
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := moveRecursive(ctx, src, dst)
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestMoveRecursive_SourceNotExist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := moveRecursive(context.Background(), filepath.Join(dir, "nope"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Error("expected error for nonexistent source")
	}
}

// ---------- findNZFFiles ----------

func TestFindNZFFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create mix of .nzf and non-.nzf files.
	os.WriteFile(filepath.Join(dir, "0000.nzf"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "0001.nzf"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("c"), 0o644)
	os.MkdirAll(filepath.Join(dir, "subdir.nzf"), 0o755) // directory, should skip

	files, err := findNZFFiles(dir)
	if err != nil {
		t.Fatalf("findNZFFiles: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("found %d .nzf files, want 2: %v", len(files), files)
	}
}

func TestFindNZFFiles_EmptyDir(t *testing.T) {
	t.Parallel()
	files, err := findNZFFiles(t.TempDir())
	if err != nil {
		t.Fatalf("findNZFFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestFindNZFFiles_NonexistentDir(t *testing.T) {
	t.Parallel()
	_, err := findNZFFiles("/nonexistent/path")
	if err == nil {
		t.Error("expected error for nonexistent directory")
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
