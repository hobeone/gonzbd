package postproc

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestFinalizeHelpers(t *testing.T) {
	t.Parallel()

	// Direct references for alignment check
	_ = (*FinalizeStage).handleFailure
	_ = (*FinalizeStage).moveToDest
	_ = (*FinalizeStage).moveFileByFile

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
}
