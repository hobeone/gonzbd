package postproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFinalizeStage_PartialMovePreservesSourceDir verifies C7:
// When file-by-file move partially fails (cross-device fallback), the
// source directory must NOT be RemoveAll'd — unmoved files would be lost.
func TestFinalizeStage_PartialMovePreservesSourceDir(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	finalDir := filepath.Join(t.TempDir(), "final")

	// Create two files and a subdirectory with a file.
	os.WriteFile(filepath.Join(srcDir, "good.txt"), []byte("good data"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "also_good.txt"), []byte("also good"), 0o644)

	// Create a file that will fail to move by making the destination
	// unwriteable. We'll make the destination dir for one entry, then
	// make it read-only so the move fails.
	//
	// Strategy: use moveRecursive which calls fsutil.MoveFile. We can't
	// easily simulate a cross-device error, but we can create a scenario
	// where os.Rename fails and the file-by-file fallback is triggered,
	// then make one file unmovable.
	//
	// Simpler approach: put srcDir and finalDir on the same device (they
	// are in t.TempDir()), so os.Rename will succeed. Instead, test the
	// file-by-file path directly by pre-creating finalDir and a conflicting
	// entry that blocks the move.

	// Pre-create finalDir with a subdirectory named "good.txt" so the
	// file move for good.txt fails (can't overwrite dir with file).
	if err := os.MkdirAll(filepath.Join(finalDir, "good.txt"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	qjob := newNamedJob(t, "partial-move-test", "partial-move", 0)
	job := &Job{
		Job:         qjob,
		DownloadDir: srcDir,
		FinalDir:    finalDir,
	}

	err := NewFinalizeStage().Run(t.Context(), job)

	// Should return an error because at least one file failed to move.
	if err == nil {
		t.Fatal("expected error for partial move, got nil")
	}

	// The source directory MUST still exist (unmoved files preserved).
	if _, statErr := os.Stat(srcDir); os.IsNotExist(statErr) {
		t.Error("source directory was deleted despite partial move failure — data loss!")
	}

	// The file that couldn't move (good.txt) should still be in srcDir.
	if _, statErr := os.Stat(filepath.Join(srcDir, "good.txt")); os.IsNotExist(statErr) {
		t.Error("good.txt missing from source — it should have been preserved")
	}
}

// TestFinalizeStage_AllFilesMovedCleansSource verifies that when all files
// are successfully moved, the source directory IS cleaned up.
func TestFinalizeStage_AllFilesMovedCleansSource(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	finalDir := filepath.Join(t.TempDir(), "final")

	os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "b.txt"), []byte("bbb"), 0o644)

	// Pre-create finalDir to force the merge path (file-by-file).
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Write a dummy file so os.Rename gets ENOTEMPTY.
	os.WriteFile(filepath.Join(finalDir, "existing.txt"), []byte("x"), 0o644)

	qjob := newNamedJob(t, "full-move-test", "full-move", 0)
	job := &Job{
		Job:         qjob,
		DownloadDir: srcDir,
		FinalDir:    finalDir,
	}

	if err := NewFinalizeStage().Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify files arrived.
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(finalDir, name)); err != nil {
			t.Errorf("file %s not found in finalDir: %v", name, err)
		}
	}

	// Source should be cleaned up.
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Errorf("source directory should be removed after full move, stat err: %v", err)
	}
}

// TestFinalizeStage_PartialMoveErrorContainsAllFailures verifies that
// when multiple files fail to move, the error contains info about all
// of them (not just the first).
func TestFinalizeStage_PartialMoveErrorContainsAllFailures(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	finalDir := filepath.Join(t.TempDir(), "final")

	// Create source files.
	os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "file2.txt"), []byte("2"), 0o644)

	// Block both files by pre-creating directories with their names.
	os.MkdirAll(filepath.Join(finalDir, "file1.txt"), 0o755)
	os.MkdirAll(filepath.Join(finalDir, "file2.txt"), 0o755)

	qjob := newQueueJob(t, "multi-fail", 0)
	job := &Job{
		Job:         qjob,
		DownloadDir: srcDir,
		FinalDir:    finalDir,
	}

	err := NewFinalizeStage().Run(t.Context(), job)
	if err == nil {
		t.Fatal("expected error for blocked moves")
	}

	// The combined error should mention both files.
	errStr := err.Error()
	if !strings.Contains(errStr, "file1.txt") {
		t.Errorf("error should mention file1.txt: %v", err)
	}
	if !strings.Contains(errStr, "file2.txt") {
		t.Errorf("error should mention file2.txt: %v", err)
	}

	// Source files must be preserved.
	for _, name := range []string{"file1.txt", "file2.txt"} {
		if _, statErr := os.Stat(filepath.Join(srcDir, name)); os.IsNotExist(statErr) {
			t.Errorf("%s deleted from source despite move failure", name)
		}
	}
}
