package postproc

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFinalizeHelpers(t *testing.T) {
	t.Parallel()

	t.Run("handleFailure", func(t *testing.T) {
		f := NewFinalizeStage()
		job := &Job{
			ParError: true,
		}
		err := f.handleFailure(t.Context(), slog.Default(), job, false)
		if err != nil {
			t.Fatalf("handleFailure: %v", err)
		}
	})

	// moveToDest and moveFileByFile are tested with source and dest under
	// the same t.TempDir() parent: a nonexistent source elsewhere on the
	// filesystem can hit os.Rename's cross-device (EXDEV) branch instead
	// of ENOENT, silently falling through to moveFileByFile's ReadDir
	// error instead of exercising moveToDest's own rename-error return.
	t.Run("moveToDest nonexistent", func(t *testing.T) {
		f := NewFinalizeStage()
		base := t.TempDir()
		job := &Job{
			DownloadDir: filepath.Join(base, "nonexistent-src"),
		}
		err := f.moveToDest(t.Context(), slog.Default(), job, filepath.Join(base, "dest"), false)
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("moveToDest with nonexistent source: err = %v, want wrapped os.ErrNotExist", err)
		}
	})

	t.Run("moveFileByFile nonexistent", func(t *testing.T) {
		f := NewFinalizeStage()
		base := t.TempDir()
		job := &Job{
			DownloadDir: filepath.Join(base, "nonexistent-src"),
		}
		err := f.moveFileByFile(t.Context(), slog.Default(), job, filepath.Join(base, "dest"), false)
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("moveFileByFile with nonexistent source: err = %v, want wrapped os.ErrNotExist", err)
		}
	})

	t.Run("moveFileByFile mkdir error", func(t *testing.T) {
		f := NewFinalizeStage()
		base := t.TempDir()
		srcDir := filepath.Join(base, "src")
		if err := os.MkdirAll(srcDir, 0o750); err != nil {
			t.Fatalf("mkdir src: %v", err)
		}
		// Block MkdirAll(dest) by placing a regular file at dest.
		destFile := filepath.Join(base, "dest-file")
		if err := os.WriteFile(destFile, []byte("x"), 0o600); err != nil {
			t.Fatalf("write dest-file: %v", err)
		}
		job := &Job{DownloadDir: srcDir}
		if err := f.moveFileByFile(t.Context(), slog.Default(), job, destFile, false); err == nil {
			t.Error("moveFileByFile with regular file at dest: err = nil, want mkdir error")
		}
	})

	t.Run("moveFileByFile folderRename strip", func(t *testing.T) {
		f := NewFinalizeStage()
		base := t.TempDir()
		srcDir := filepath.Join(base, "src")
		dest := filepath.Join(base, "_UNPACK_rel")
		finalDir := filepath.Join(base, "rel")
		if err := os.MkdirAll(srcDir, 0o750); err != nil {
			t.Fatalf("mkdir src: %v", err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("a"), 0o600); err != nil {
			t.Fatalf("write a.txt: %v", err)
		}
		job := &Job{DownloadDir: srcDir, FinalDir: finalDir}
		if err := f.moveFileByFile(t.Context(), slog.Default(), job, dest, true); err != nil {
			t.Fatalf("moveFileByFile: %v", err)
		}
		if job.DownloadDir != finalDir {
			t.Errorf("job.DownloadDir = %q, want %q", job.DownloadDir, finalDir)
		}
	})
}

// TestFinalizeStage_MoveFailureSetsFailMsg verifies #761: when FinalizeStage
// cannot move a job's files into FinalDir (either because FinalDir is unset,
// the destination parent is read-only, or file-by-file move partially fails),
// it sets job.FailMsg = "finalize: ...", keeps job.DownloadDir at the source
// directory holding the unmoved bytes, causes the downstream ScriptStage to
// receive a non-zero status code, and causes buildSummaryEntry to report
// "Pipeline Failed" (buildHistoryEntry's Status/Path/Storage derivation is
// pinned in internal/app by TestBuildHistoryEntry_FinalizeFailureUsesDownloadDir).
// Conversely, a non-fatal _UNPACK_ prefix strip failure in either moveToDest or
// moveFileByFile stays a warning and leaves FailMsg empty.
func TestFinalizeStage_MoveFailureSetsFailMsg(t *testing.T) {
	t.Parallel()

	t.Run("empty FinalDir", func(t *testing.T) {
		t.Parallel()
		job, srcDir := stageJob(t)
		job.FinalDir = ""

		err := NewFinalizeStage().Run(t.Context(), job)
		if err == nil {
			t.Fatal("Run with empty FinalDir: err = nil, want error")
		}
		if job.FailMsg != "finalize: FinalDir not set" {
			t.Errorf("job.FailMsg = %q, want %q", job.FailMsg, "finalize: FinalDir not set")
		}
		if job.DownloadDir != srcDir {
			t.Errorf("job.DownloadDir = %q, want %q", job.DownloadDir, srcDir)
		}
	})

	t.Run("read-only destination parent", func(t *testing.T) {
		t.Parallel()
		if os.Getuid() == 0 {
			t.Skip("root bypasses read-only directory permissions")
		}

		srcDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(srcDir, "movie.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}

		roParent := filepath.Join(t.TempDir(), "readonly")
		if err := os.MkdirAll(roParent, 0o500); err != nil {
			t.Fatalf("mkdir readonly parent: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(roParent, 0o700) })

		finalDir := filepath.Join(roParent, "sub", "MyRelease")

		scriptDir := t.TempDir()
		statusFile := filepath.Join(scriptDir, "status.txt")
		dirFile := filepath.Join(scriptDir, "dir.txt")
		writeScript(t, filepath.Join(scriptDir, "notify.sh"),
			[]byte("#!/bin/sh\necho \"$SAB_PP_STATUS\" > "+statusFile+"\necho \"$SAB_FINAL_PROCESSING_DIR\" > "+dirFile+"\n"))

		qjob := newQueueJob(t, "ro-dest-job", 0)
		qjob.SetName("MyRelease")
		job := &Job{
			Job:         qjob,
			Filename:    "MyRelease.nzb",
			DownloadDir: srcDir,
			FinalDir:    finalDir,
			Script:      "notify.sh",
		}

		pp := New(Options{
			Stages: []Stage{
				NewFinalizeStage(),
				NewScriptStage(scriptDir, roParent, "test", "", ""),
			},
		})
		pp.processJob(t.Context(), job)

		if !strings.HasPrefix(job.FailMsg, "finalize: ") {
			t.Errorf("job.FailMsg = %q, want non-empty message with %q prefix", job.FailMsg, "finalize: ")
		}
		if job.DownloadDir != srcDir {
			t.Errorf("job.DownloadDir = %q, want %q (where the bytes still live)", job.DownloadDir, srcDir)
		}
		if summary := buildSummaryEntry(job); len(summary.Lines) == 0 || !strings.HasPrefix(summary.Lines[0], "Pipeline Failed") {
			t.Errorf("summary header = %v, want prefix %q", summary.Lines, "Pipeline Failed")
		}

		gotStatus, err := os.ReadFile(statusFile)
		if err != nil {
			t.Fatalf("read script status output: %v", err)
		}
		if strings.TrimSpace(string(gotStatus)) == "0" || strings.TrimSpace(string(gotStatus)) == "" {
			t.Errorf("script SAB_PP_STATUS = %q, want non-zero failure status", strings.TrimSpace(string(gotStatus)))
		}
		gotDir, err := os.ReadFile(dirFile)
		if err != nil {
			t.Fatalf("read script dir output: %v", err)
		}
		if strings.TrimSpace(string(gotDir)) != srcDir {
			t.Errorf("script SAB_FINAL_PROCESSING_DIR = %q, want DownloadDir %q", strings.TrimSpace(string(gotDir)), srcDir)
		}
	})

	t.Run("partial move records moved and unmoved files", func(t *testing.T) {
		t.Parallel()

		srcDir := t.TempDir()
		finalDir := filepath.Join(t.TempDir(), "final")

		if err := os.WriteFile(filepath.Join(srcDir, "moved.mkv"), []byte("moved"), 0o600); err != nil {
			t.Fatalf("write moved.mkv: %v", err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, "stuck.mkv"), []byte("stuck"), 0o600); err != nil {
			t.Fatalf("write stuck.mkv: %v", err)
		}

		// Pre-create a non-empty directory at finalDir/stuck.mkv so atomic
		// directory rename falls back to moveFileByFile (ENOTEMPTY) and moving
		// the regular file stuck.mkv over the directory fails.
		stuckBlocker := filepath.Join(finalDir, "stuck.mkv")
		if err := os.MkdirAll(stuckBlocker, 0o750); err != nil {
			t.Fatalf("mkdir blocker: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stuckBlocker, "keep"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write blocker child: %v", err)
		}

		scriptDir := t.TempDir()
		statusFile := filepath.Join(scriptDir, "status.txt")
		writeScript(t, filepath.Join(scriptDir, "notify.sh"),
			[]byte("#!/bin/sh\necho \"$SAB_PP_STATUS\" > "+statusFile+"\n"))

		qjob := newQueueJob(t, "partial-move-job", 0)
		qjob.SetName("MyRelease")
		job := &Job{
			Job:         qjob,
			Filename:    "MyRelease.nzb",
			DownloadDir: srcDir,
			FinalDir:    finalDir,
			Script:      "notify.sh",
		}

		pp := New(Options{
			Stages: []Stage{
				NewFinalizeStage(),
				NewScriptStage(scriptDir, filepath.Dir(finalDir), "test", "", ""),
			},
		})
		pp.processJob(t.Context(), job)

		if !strings.HasPrefix(job.FailMsg, "finalize: ") {
			t.Errorf("job.FailMsg = %q, want non-empty message with %q prefix", job.FailMsg, "finalize: ")
		}
		wantDetail := fmt.Sprintf("moved to %s: [moved.mkv]; unmoved in %s: [stuck.mkv]", finalDir, srcDir)
		if !strings.Contains(job.FailMsg, wantDetail) {
			t.Errorf("job.FailMsg = %q, want substring %q naming moved/unmoved files and their locations", job.FailMsg, wantDetail)
		}
		if job.DownloadDir != srcDir {
			t.Errorf("job.DownloadDir = %q, want %q", job.DownloadDir, srcDir)
		}

		// Verify on disk that the unmoved file stays in srcDir and the moved
		// file reached finalDir.
		stuckBytes, err := os.ReadFile(filepath.Join(srcDir, "stuck.mkv"))
		if err != nil || string(stuckBytes) != "stuck" {
			t.Errorf("srcDir/stuck.mkv = %q, err = %v; want %q preserved in DownloadDir", string(stuckBytes), err, "stuck")
		}
		movedBytes, err := os.ReadFile(filepath.Join(finalDir, "moved.mkv"))
		if err != nil || string(movedBytes) != "moved" {
			t.Errorf("finalDir/moved.mkv = %q, err = %v; want %q in FinalDir", string(movedBytes), err, "moved")
		}
		if summary := buildSummaryEntry(job); len(summary.Lines) == 0 || !strings.HasPrefix(summary.Lines[0], "Pipeline Failed") {
			t.Errorf("summary header = %v, want prefix %q", summary.Lines, "Pipeline Failed")
		}

		gotStatus, err := os.ReadFile(statusFile)
		if err != nil {
			t.Fatalf("read script status output: %v", err)
		}
		if strings.TrimSpace(string(gotStatus)) == "0" || strings.TrimSpace(string(gotStatus)) == "" {
			t.Errorf("script SAB_PP_STATUS = %q, want non-zero failure status", strings.TrimSpace(string(gotStatus)))
		}
	})

	t.Run("unpack prefix strip failure stays a warning in moveToDest", func(t *testing.T) {
		t.Parallel()

		baseDir := t.TempDir()
		srcDir := filepath.Join(baseDir, "src")
		if err := os.MkdirAll(srcDir, 0o750); err != nil {
			t.Fatalf("mkdir srcDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, "movie.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}

		finalDir := filepath.Join(baseDir, "MyRelease")
		// Pre-populate finalDir so atomic rename to _UNPACK_MyRelease succeeds,
		// while stripping the _UNPACK_ prefix (rename _UNPACK_MyRelease -> MyRelease)
		// fails with ENOTEMPTY.
		if err := os.MkdirAll(finalDir, 0o750); err != nil {
			t.Fatalf("mkdir finalDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(finalDir, "other.txt"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write other.txt: %v", err)
		}

		qjob := newQueueJob(t, "unpack-strip-warn-atomic", 0)
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
		if job.FailMsg != "" {
			t.Errorf("job.FailMsg = %q, want empty (prefix strip failure is non-fatal)", job.FailMsg)
		}
		wantUnpackDir := filepath.Join(baseDir, "_UNPACK_MyRelease")
		if job.DownloadDir != wantUnpackDir {
			t.Errorf("job.DownloadDir = %q, want %q", job.DownloadDir, wantUnpackDir)
		}
	})

	t.Run("unpack prefix strip failure stays a warning in moveFileByFile", func(t *testing.T) {
		t.Parallel()

		baseDir := t.TempDir()
		srcDir := filepath.Join(baseDir, "src")
		if err := os.MkdirAll(srcDir, 0o750); err != nil {
			t.Fatalf("mkdir srcDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, "movie.mkv"), []byte("video"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}

		unpackDir := filepath.Join(baseDir, "_UNPACK_MyRelease")
		finalDir := filepath.Join(baseDir, "MyRelease")
		// Pre-populate both _UNPACK_MyRelease (so moveToDest's atomic rename
		// falls back to moveFileByFile) and MyRelease (so moveFileByFile's
		// _UNPACK_ prefix-strip rename fails with ENOTEMPTY).
		for _, dir := range []string{unpackDir, finalDir} {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o600); err != nil {
				t.Fatalf("write existing.txt in %s: %v", dir, err)
			}
		}

		qjob := newQueueJob(t, "unpack-strip-warn-filebyfile", 0)
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
		if job.FailMsg != "" {
			t.Errorf("job.FailMsg = %q, want empty (moveFileByFile prefix strip failure is non-fatal)", job.FailMsg)
		}
		if job.DownloadDir != unpackDir {
			t.Errorf("job.DownloadDir = %q, want %q", job.DownloadDir, unpackDir)
		}
		if _, err := os.Stat(filepath.Join(unpackDir, "movie.mkv")); err != nil {
			t.Errorf("unpackDir/movie.mkv missing after moveFileByFile: %v", err)
		}
	})
}
