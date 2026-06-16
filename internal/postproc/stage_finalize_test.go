package postproc

import (
	"log/slog"
	"testing"
)

func TestFinalizeHelpers(t *testing.T) {
	t.Parallel()

	// Direct references for alignment check
	_ = (*FinalizeStage).handleFailure
	_ = (*FinalizeStage).moveToDest
	_ = (*FinalizeStage).moveFileByFile

	// 1. Test handleFailure
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

	// 2. Test moveToDest (nonexistent paths)
	t.Run("moveToDest nonexistent", func(t *testing.T) {
		f := NewFinalizeStage()
		job := &Job{
			DownloadDir: "/nonexistent-src-dir",
		}
		_ = f.moveToDest(t.Context(), slog.Default(), job, "/nonexistent-dest-dir", false)
	})

	// 3. Test moveFileByFile (nonexistent paths)
	t.Run("moveFileByFile nonexistent", func(t *testing.T) {
		f := NewFinalizeStage()
		job := &Job{
			DownloadDir: "/nonexistent-src-dir",
		}
		_ = f.moveFileByFile(t.Context(), slog.Default(), job, "/nonexistent-dest-dir", false)
	})
}
